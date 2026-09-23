package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/matty-v/kyber/pkg/archivejob"
	"github.com/matty-v/kyber/pkg/archivestore"
	"github.com/matty-v/kyber/pkg/diskarchive"
)

// WithArchiveService registers the archive pods' endpoints on the internal
// API. They authenticate with the per-job token, not a pod identity token:
// an archive pod may act for exactly one job and nothing else.
func WithArchiveService(a *ArchiveService) InternalServerOption {
	return func(s *InternalServer) {
		s.archives = a
	}
}

// handleArchiveJobRoutes serves the archive pods, one job each:
//
//	PUT  /internal/archive-jobs/{id}/upload   export pod's archive stream
//	GET  /internal/archive-jobs/{id}/archive  ranged plaintext reads (verify, restore)
//	POST /internal/archive-jobs/{id}/summary  verify pod's result
func (s *InternalServer) handleArchiveJobRoutes(w http.ResponseWriter, r *http.Request) {
	a := s.archives
	if ok, _ := a.Available(); !ok {
		http.Error(w, "disk archives unavailable", http.StatusServiceUnavailable)
		return
	}
	id, action, ok := strings.Cut(strings.TrimPrefix(r.URL.Path, "/internal/archive-jobs/"), "/")
	if !ok || id == "" {
		http.NotFound(w, r)
		return
	}
	j, err := a.Jobs.Get(r.Context(), id)
	token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if err != nil || !checkPodToken(j, token) || j.State != archivejob.StateRunning || j.CancelRequested || j.Finishing != "" {
		// One answer for unknown, finished and unauthorized jobs, so the
		// endpoint cannot be used to probe for job IDs.
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	verifying := j.Step == archivejob.StepVerifying && j.Kind != archivejob.KindImport
	switch {
	case action == "upload" && r.Method == http.MethodPut && j.Kind == archivejob.KindExport && j.Step == archivejob.StepArchiving:
		a.receiveExport(w, r, j)
	case action == "archive" && (r.Method == http.MethodGet || r.Method == http.MethodHead) && verifying:
		a.serveArchiveReads(w, r, j)
	case action == "archive" && (r.Method == http.MethodGet || r.Method == http.MethodHead) && j.Kind == archivejob.KindImport && j.Step == archivejob.StepRestoring:
		a.serveRestoreReads(w, r, j)
	case action == "summary" && r.Method == http.MethodPost && verifying:
		a.receiveSummary(w, r, j)
	default:
		http.Error(w, "forbidden", http.StatusForbidden)
	}
}

// receiveExport stores the export pod's ZIP stream. The internal server's
// two-minute read timeout is lifted for this request and replaced by the
// job's own deadline.
func (a *ArchiveService) receiveExport(w http.ResponseWriter, r *http.Request, j *archivejob.Job) {
	// Claim the one upload this job accepts before reading a byte. A second
	// attempt would reuse the job's data key, and its failure path would
	// delete the first attempt's object.
	if j.UploadClaimed || j.Uploaded {
		http.Error(w, "already uploaded", http.StatusConflict)
		return
	}
	j.UploadClaimed = true
	if err := a.Jobs.Update(r.Context(), j); err != nil {
		http.Error(w, "upload already claimed", http.StatusConflict)
		return
	}
	// j.Deadline is on the service clock; translate the time remaining onto
	// the wall clock the network deadlines use.
	remaining := a.limits().JobTimeout
	if j.Deadline != nil {
		remaining = j.Deadline.Sub(a.now())
	}
	deadline := time.Now().Add(remaining)
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(deadline)
	_ = rc.SetWriteDeadline(deadline)
	ctx, cancel := context.WithDeadline(r.Context(), deadline)
	defer cancel()
	a.trackUpload(j.ID, cancel)
	defer a.untrackUpload(j.ID)

	var lastReport time.Time
	plain, stored, err := a.storeSealed(ctx, j, r.Body, func(n int64) {
		// Progress is best-effort and throttled; the version check means a
		// concurrent cancel always wins over a progress write.
		if time.Since(lastReport) < 5*time.Second {
			return
		}
		lastReport = time.Now()
		if cur, gerr := a.Jobs.Get(context.WithoutCancel(ctx), j.ID); gerr == nil && !cur.CancelRequested && cur.Finishing == "" {
			cur.BytesDone = n
			_ = a.Jobs.Update(context.WithoutCancel(ctx), cur)
		}
	})
	if err != nil {
		_ = a.Store.Delete(context.WithoutCancel(ctx), j.ObjectKey)
		status := http.StatusInternalServerError
		if errors.Is(err, errArchiveTooLarge) {
			status = http.StatusRequestEntityTooLarge
		}
		slog.Warn("disk export upload failed", "job", j.ID, "agent", j.Agent, "error", err)
		http.Error(w, "upload failed", status)
		return
	}
	// A progress write may have raced this one; retry the record on a
	// version conflict as long as the job is still waiting for it.
	for attempt := 0; attempt < 5; attempt++ {
		cur, gerr := a.Jobs.Get(context.WithoutCancel(ctx), j.ID)
		if gerr != nil || cur.CancelRequested || cur.Finishing != "" || cur.State != archivejob.StateRunning {
			break
		}
		cur.Uploaded = true
		cur.PlainSize = plain
		cur.SealedSize = stored
		cur.BytesDone = plain
		err = a.Jobs.Update(context.WithoutCancel(ctx), cur)
		if err == nil {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if !errors.Is(err, archivejob.ErrConflict) {
			break
		}
	}
	_ = a.Store.Delete(context.WithoutCancel(ctx), j.ObjectKey)
	http.Error(w, "upload not recorded", http.StatusConflict)
}

// serveArchiveReads answers ranged reads of a job's archive plaintext.
// http.ServeContent handles HEAD, Range and Content-Range over the sealed
// reader.
func (a *ArchiveService) serveArchiveReads(w http.ResponseWriter, r *http.Request, src *archivejob.Job) {
	ra, err := a.sharedReader(src)
	if err != nil {
		slog.Warn("opening archive for reading failed", "job", src.ID, "error", err)
		http.Error(w, "archive unreadable", http.StatusInternalServerError)
		return
	}
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(10 * time.Minute))
	w.Header().Set("Content-Type", "application/zip")
	http.ServeContent(w, r, "", time.Time{}, ra)
}

// maxSummaryBytes bounds the verify pod's result. The summary carries no
// per-entry list; exclusions are capped by the pod.
const maxSummaryBytes = 8 << 20

// receiveSummary records a verify pod's result: a manifest summary of an
// archive it has read back in full and found consistent.
func (a *ArchiveService) receiveSummary(w http.ResponseWriter, r *http.Request, j *archivejob.Job) {
	var sum archivejob.Summary
	if err := json.NewDecoder(io.LimitReader(r.Body, maxSummaryBytes)).Decode(&sum); err != nil {
		http.Error(w, "bad summary", http.StatusBadRequest)
		return
	}
	if diskarchive.CheckVersion(sum.FormatVersion) != nil {
		http.Error(w, "unsupported format version", http.StatusBadRequest)
		return
	}
	j.Summary = &sum
	j.Verified = true
	if err := a.Jobs.Update(r.Context(), j); err != nil {
		http.Error(w, "job changed", http.StatusConflict)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// sharedArchive is one opened archive shared across a pod's ranged reads.
// Opening costs a size lookup, a header read and a final-chunk read; a verify
// or restore pod reads a large archive in thousands of 4 MiB requests, so the
// opened reader, with its decrypted chunk window, is kept between them.
type sharedArchive struct {
	mu   sync.Mutex
	r    *archivestore.SealedReader
	used time.Time
}

func (s *sharedArchive) ReadAt(p []byte, off int64) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.used = time.Now()
	return s.r.ReadAt(p, off)
}

// sharedReaderIdle is how long an unused opened archive is kept.
const sharedReaderIdle = 5 * time.Minute

// sharedReader returns the cached reader for a job's archive, opening it on
// first use. Readers outlive the request that opened them, so they read with
// a context of their own; idle ones are dropped.
func (a *ArchiveService) sharedReader(j *archivejob.Job) (*io.SectionReader, error) {
	a.readersMu.Lock()
	defer a.readersMu.Unlock()
	now := time.Now()
	for id, c := range a.readers {
		c.mu.Lock()
		idle := now.Sub(c.used) > sharedReaderIdle
		c.mu.Unlock()
		if idle {
			delete(a.readers, id)
		}
	}
	if c, ok := a.readers[j.ID]; ok {
		return io.NewSectionReader(c, 0, c.r.Size()), nil
	}
	r, err := a.openArchive(context.Background(), j)
	if err != nil {
		return nil, err
	}
	if a.readers == nil {
		a.readers = map[string]*sharedArchive{}
	}
	c := &sharedArchive{r: r, used: now}
	a.readers[j.ID] = c
	return io.NewSectionReader(c, 0, r.Size()), nil
}

// dropReader forgets a job's opened archive once the job no longer serves it.
func (a *ArchiveService) dropReader(id string) {
	a.readersMu.Lock()
	defer a.readersMu.Unlock()
	delete(a.readers, id)
}

package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/matty-v/kyber/pkg/archivejob"
)

// WithArchiveService registers the archive pods' endpoints on the internal
// API. They authenticate with the per-job token, not a pod identity token:
// an archive pod may act for exactly one job and nothing else.
func WithArchiveService(a *ArchiveService) InternalServerOption {
	return func(s *InternalServer) {
		s.archives = a
	}
}

// handleArchiveJobRoutes serves /internal/archive-jobs/{id}/upload (PUT, the
// export pod's archive stream) and /internal/archive-jobs/{id}/archive (GET,
// the restore pod's ranged reads).
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
	if err != nil || !checkPodToken(j, token) || j.State != archivejob.StateRunning || j.CancelRequested {
		// One answer for unknown, finished and unauthorized jobs, so the
		// endpoint cannot be used to probe for job IDs.
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	switch {
	case action == "upload" && r.Method == http.MethodPut && j.Kind == archivejob.KindExport && j.Step == archivejob.StepArchiving:
		a.receiveExport(w, r, j)
	case action == "archive" && (r.Method == http.MethodGet || r.Method == http.MethodHead) && j.Kind == archivejob.KindImport && j.Step == archivejob.StepRestoring:
		a.serveRestoreReads(w, r, j)
	default:
		http.Error(w, "forbidden", http.StatusForbidden)
	}
}

// receiveExport stores the export pod's ZIP stream. The internal server's
// two-minute read timeout is lifted for this request and replaced by the
// job's own deadline.
func (a *ArchiveService) receiveExport(w http.ResponseWriter, r *http.Request, j *archivejob.Job) {
	if j.Uploaded {
		http.Error(w, "already uploaded", http.StatusConflict)
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
		if cur, gerr := a.Jobs.Get(context.WithoutCancel(ctx), j.ID); gerr == nil && !cur.CancelRequested {
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
	cur, err := a.Jobs.Get(context.WithoutCancel(ctx), j.ID)
	if err == nil && !cur.CancelRequested && cur.State == archivejob.StateRunning {
		cur.Uploaded = true
		cur.PlainSize = plain
		cur.SealedSize = stored
		cur.BytesDone = plain
		err = a.Jobs.Update(context.WithoutCancel(ctx), cur)
	} else if err == nil {
		err = errors.New("job was canceled during upload")
	}
	if err != nil {
		_ = a.Store.Delete(context.WithoutCancel(ctx), j.ObjectKey)
		http.Error(w, "upload not recorded", http.StatusConflict)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

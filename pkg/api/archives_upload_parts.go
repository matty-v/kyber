package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/matty-v/kyber/pkg/archivejob"
	"github.com/matty-v/kyber/pkg/archivestore"
)

// Uploads in parts (MAT-90). One request body the size of an agent's disk
// cannot pass a proxy such as Cloudflare, so the console sends the archive as
// fixed-size parts. Each part is sealed on arrival as its own object, with its
// chunk indexes offset by its position; only part 0 carries the seal header
// and only the last part seals the final chunk. Joining the parts in order is
// therefore byte-identical to a single sealed Put, and verification, download
// and import read the joined object exactly as before. Parts live in the store
// and the job record, so any control-plane replica can accept any part.

// archiveUploadPartSize is the plaintext size of every part but the last: a
// whole number of seal chunks, above S3's 5 MiB minimum copy part and well
// under common proxy body limits.
const archiveUploadPartSize int64 = 32 << 20

// uploadPartIdleTimeout fails an upload in parts that receives nothing for
// this long, deleting what it holds.
const uploadPartIdleTimeout = 30 * time.Minute

// maxUploadParts bounds an upload by the smallest backend limit (S3's 10,000
// parts in one multipart copy).
const maxUploadParts = 10000

func uploadPartCount(size, partSize int64) int {
	if partSize <= 0 {
		return 0
	}
	return int((size + partSize - 1) / partSize)
}

func uploadPartKey(id string, n int) string {
	return fmt.Sprintf("uploads/%s/parts/%06d.sealed", id, n)
}

// uploadPartLen is the plaintext length part n must carry.
func uploadPartLen(j *archivejob.Job, n int) int64 {
	last := uploadPartCount(j.UploadSize, j.PartSize) - 1
	if n < last {
		return j.PartSize
	}
	return j.UploadSize - int64(last)*j.PartSize
}

func uploadReceivedBytes(j *archivejob.Job) int64 {
	var n int64
	for _, p := range j.PartsReceived {
		n += uploadPartLen(j, p)
	}
	return n
}

// uploadIdle reports an upload in parts that has stopped receiving.
func (a *ArchiveService) uploadIdle(j *archivejob.Job) bool {
	if j.PartSize == 0 || j.Step != archivejob.StepReceiving || j.UploadClaimed {
		return false
	}
	last := j.StartedAt
	if j.LastPartAt != nil {
		last = j.LastPartAt
	}
	return last != nil && a.now().Sub(*last) > uploadPartIdleTimeout
}

// deleteUploadParts removes every part object an upload in parts may hold.
func (a *ArchiveService) deleteUploadParts(ctx context.Context, j *archivejob.Job) error {
	for n := range uploadPartCount(j.UploadSize, j.PartSize) {
		if err := a.Store.Delete(ctx, uploadPartKey(j.ID, n)); err != nil {
			return fmt.Errorf("deleting upload part %d: %w", n, err)
		}
	}
	return nil
}

// updateUpload re-reads the job and applies change while it still accepts
// parts, retrying version conflicts. It reports false when the job has moved
// on (canceled, failed, completing).
func (a *ArchiveService) updateUpload(ctx context.Context, id string, change func(*archivejob.Job) error) (*archivejob.Job, bool, error) {
	for attempt := 0; attempt < 8; attempt++ {
		cur, err := a.Jobs.Get(ctx, id)
		if err != nil {
			return nil, false, err
		}
		if cur.State != archivejob.StateRunning || cur.Finishing != "" || cur.CancelRequested || cur.Step != archivejob.StepReceiving || cur.UploadClaimed {
			return cur, false, nil
		}
		if err := change(cur); err != nil {
			return nil, false, err
		}
		err = a.Jobs.Update(ctx, cur)
		if err == nil {
			return cur, true, nil
		}
		if !errors.Is(err, archivejob.ErrConflict) {
			return nil, false, err
		}
	}
	return nil, false, archivejob.ErrConflict
}

// startPartUpload begins an upload in parts: POST /api/v1/archive-uploads
// with {"size": <archive bytes>}.
func (s *Server) startPartUpload(w http.ResponseWriter, r *http.Request) {
	a := s.Archives
	var req struct {
		Size int64 `json:"size"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil || req.Size <= 0 {
		writeJSONErrorWithField(w, http.StatusBadRequest, "VALIDATION_ERROR", "size must be the archive's length in bytes", "size")
		return
	}
	if req.Size > a.limits().MaxArchiveBytes {
		writeJSONError(w, http.StatusRequestEntityTooLarge, "upload_failed", errArchiveTooLarge.Error())
		return
	}
	partSize := a.limits().UploadPartBytes
	if uploadPartCount(req.Size, partSize) > maxUploadParts {
		writeJSONError(w, http.StatusRequestEntityTooLarge, "upload_failed", "archive needs more parts than the store supports")
		return
	}
	caller := callerFrom(r.Context())
	j, err := a.StartUpload(r.Context(), caller.Name)
	if errors.Is(err, archivejob.ErrConcurrency) {
		w.Header().Set("Retry-After", "60")
		writeJSONError(w, http.StatusTooManyRequests, "archive_capacity", "the installation is already running its maximum number of archive jobs")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to start the upload")
		return
	}
	cur, ok, err := a.updateUpload(r.Context(), j.ID, func(c *archivejob.Job) error {
		c.UploadSize, c.PartSize = req.Size, partSize
		c.BytesTotal = req.Size
		c.Message = fmt.Sprintf("Receiving the archive (0 of %d parts)", uploadPartCount(req.Size, partSize))
		return nil
	})
	if err != nil || !ok {
		_ = a.fail(context.WithoutCancel(r.Context()), j, "upload could not be prepared")
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to start the upload")
		return
	}
	writeJSON(w, http.StatusCreated, viewArchiveJob(cur))
}

// receivePart stores part n: PUT /api/v1/archive-uploads/{id}/parts/{n}.
// Sending the same part again replaces it, so a failed part is retried alone.
func (s *Server) receivePart(w http.ResponseWriter, r *http.Request, id, number string) {
	a := s.Archives
	n, err := strconv.Atoi(number)
	j, gerr := a.Jobs.Get(r.Context(), id)
	if gerr != nil || j.Kind != archivejob.KindUpload || j.PartSize == 0 {
		writeJSONError(w, http.StatusNotFound, "not_found", "upload not found")
		return
	}
	if err != nil || n < 0 || n >= uploadPartCount(j.UploadSize, j.PartSize) {
		writeJSONErrorWithField(w, http.StatusBadRequest, "VALIDATION_ERROR", "part number is out of range", "part")
		return
	}
	if j.State != archivejob.StateRunning || j.CancelRequested || j.Step != archivejob.StepReceiving || j.UploadClaimed {
		writeJSONError(w, http.StatusConflict, "upload_changed", "the upload is no longer receiving parts")
		return
	}
	want := uploadPartLen(j, n)
	if r.ContentLength >= 0 && r.ContentLength != want {
		writeJSONError(w, http.StatusBadRequest, "upload_failed", fmt.Sprintf("part %d must be %d bytes, got %d", n, want, r.ContentLength))
		return
	}
	key, err := archivestore.UnwrapKey(a.SigningKey, j.WrappedKey)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to read the upload key")
		return
	}
	deadline := time.Now().Add(a.limits().JobTimeout)
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(deadline)
	ctx, cancel := context.WithDeadline(r.Context(), deadline)
	defer cancel()

	last := n == uploadPartCount(j.UploadSize, j.PartSize)-1
	firstChunk := uint64(int64(n) * j.PartSize / archivestore.ChunkSize)
	partKey := uploadPartKey(j.ID, n)
	pr, pw := io.Pipe()
	var got int64
	go func() {
		sw, err := archivestore.NewPartSealWriter(pw, key, firstChunk, n == 0, last)
		if err != nil {
			pw.CloseWithError(err)
			return
		}
		got, err = io.Copy(sw, io.LimitReader(r.Body, want+1))
		if err == nil && got != want {
			err = fmt.Errorf("part %d must be %d bytes, got %d", n, want, got)
		}
		if err == nil {
			err = sw.Close()
		}
		pw.CloseWithError(err)
	}()
	_, err = a.Store.Put(ctx, partKey, pr)
	pr.CloseWithError(err)
	bg := context.WithoutCancel(ctx)
	if err != nil {
		_ = a.Store.Delete(bg, partKey)
		writeJSONError(w, http.StatusBadRequest, "upload_failed", "part not stored: "+err.Error())
		return
	}
	cur, ok, err := a.updateUpload(bg, j.ID, func(c *archivejob.Job) error {
		if !slices.Contains(c.PartsReceived, n) {
			c.PartsReceived = append(c.PartsReceived, n)
			slices.Sort(c.PartsReceived)
		}
		now := a.now()
		c.LastPartAt = &now
		c.BytesDone = uploadReceivedBytes(c)
		c.Message = fmt.Sprintf("Receiving the archive (%d of %d parts)", len(c.PartsReceived), uploadPartCount(c.UploadSize, c.PartSize))
		return nil
	})
	if err != nil || !ok {
		// The job was canceled, failed or completed while this part arrived;
		// its cleanup may already have run, so remove the part here.
		_ = a.Store.Delete(bg, partKey)
		writeJSONError(w, http.StatusConflict, "upload_changed", "the upload was canceled or finished while the part was received")
		return
	}
	writeJSON(w, http.StatusOK, viewArchiveJob(cur))
}

// completeUpload joins the parts and hands the archive to verification:
// POST /api/v1/archive-uploads/{id}/complete.
func (s *Server) completeUpload(w http.ResponseWriter, r *http.Request, id string) {
	a := s.Archives
	j, err := a.Jobs.Get(r.Context(), id)
	if err != nil || j.Kind != archivejob.KindUpload || j.PartSize == 0 {
		writeJSONError(w, http.StatusNotFound, "not_found", "upload not found")
		return
	}
	var missing []int
	for n := range uploadPartCount(j.UploadSize, j.PartSize) {
		if !slices.Contains(j.PartsReceived, n) {
			missing = append(missing, n)
		}
	}
	if len(missing) > 0 {
		writeJSONError(w, http.StatusConflict, "upload_incomplete", fmt.Sprintf("parts not yet received: %v", missing))
		return
	}
	// Claim the join with the version check so a second complete, or a late
	// part, cannot race it.
	claimed, ok, err := a.updateUpload(r.Context(), id, func(c *archivejob.Job) error {
		c.UploadClaimed = true
		c.Message = "Assembling the archive"
		return nil
	})
	if err != nil || !ok {
		writeJSONError(w, http.StatusConflict, "upload_changed", "the upload is no longer receiving parts")
		return
	}
	parts := make([]string, uploadPartCount(claimed.UploadSize, claimed.PartSize))
	for n := range parts {
		parts[n] = uploadPartKey(claimed.ID, n)
	}
	deadline := time.Now().Add(a.limits().JobTimeout)
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(deadline)
	ctx, cancel := context.WithDeadline(r.Context(), deadline)
	defer cancel()
	stored, err := archivestore.Compose(ctx, a.Store, claimed.ObjectKey, parts)
	bg := context.WithoutCancel(ctx)
	if err == nil && stored != archivestore.SealedLength(claimed.UploadSize) {
		err = fmt.Errorf("assembled %d bytes, expected %d", stored, archivestore.SealedLength(claimed.UploadSize))
	}
	if err != nil {
		reason := "upload could not be assembled: " + err.Error()
		if cur, gerr := a.Jobs.Get(bg, id); gerr == nil && !cur.State.Terminal() {
			_ = a.fail(bg, cur, reason)
		}
		writeJSONError(w, http.StatusInternalServerError, "upload_failed", reason)
		return
	}
	_ = a.deleteUploadParts(bg, claimed)
	for attempt := 0; attempt < 5; attempt++ {
		cur, gerr := a.Jobs.Get(bg, id)
		if gerr != nil || cur.State != archivejob.StateRunning || cur.Finishing != "" || cur.CancelRequested || cur.Step != archivejob.StepReceiving {
			break
		}
		cur.Uploaded, cur.PlainSize, cur.SealedSize, cur.BytesDone = true, cur.UploadSize, stored, cur.UploadSize
		cur.Step, cur.Message = archivejob.StepVerifying, "Verifying the archive"
		err = a.Jobs.Update(bg, cur)
		if err == nil {
			writeJSON(w, http.StatusAccepted, viewArchiveJob(cur))
			return
		}
		if !errors.Is(err, archivejob.ErrConflict) {
			break
		}
	}
	_ = a.Store.Delete(bg, claimed.ObjectKey)
	writeJSONError(w, http.StatusConflict, "upload_changed", "the upload was canceled or timed out while it was assembled; retry")
}

package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/matty-v/kyber/pkg/archivejob"
)

var (
	errArchiveRunning = errors.New("the archive job is still running; cancel it instead")
	errArchiveInUse   = errors.New("an import is reading this archive; delete it once the import finishes")
)

// DeleteArchive deletes a finished export's or upload's archive now rather
// than at the end of its retention. It crypto-shreds the same way retention
// does (the object is deleted and the key discarded), so outstanding
// download links stop working. Deleting an archive that is already gone is a
// no-op.
func (a *ArchiveService) DeleteArchive(ctx context.Context, j *archivejob.Job) error {
	if !j.State.Terminal() {
		return errArchiveRunning
	}
	if j.State != archivejob.StateCompleted {
		return nil
	}
	active, err := a.Jobs.ListActive(ctx, a.now())
	if err != nil {
		return err
	}
	if referencedSources(active)[j.ID] {
		return errArchiveInUse
	}
	if err := a.shred(ctx, j, "Deleted"); err != nil {
		return err
	}
	slog.Info("disk archive deleted by operator", "job", j.ID, "kind", j.Kind, "agent", j.Agent)
	return nil
}

// deleteArchive answers DELETE on an export or upload.
func (s *Server) deleteArchive(w http.ResponseWriter, r *http.Request, j *archivejob.Job) {
	switch err := s.Archives.DeleteArchive(r.Context(), j); {
	case errors.Is(err, errArchiveRunning):
		writeJSONError(w, http.StatusConflict, "archive_running", err.Error())
	case errors.Is(err, errArchiveInUse):
		writeJSONError(w, http.StatusConflict, "archive_in_use", err.Error())
	case errors.Is(err, archivejob.ErrConflict):
		writeJSONError(w, http.StatusConflict, "archive_changed", "the archive changed; retry")
	case err != nil:
		slog.Error("deleting disk archive failed", "job", j.ID, "error", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to delete the archive")
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

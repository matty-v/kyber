package api

import (
	"context"
	"net/http"

	"github.com/matty-v/kyber/pkg/archivejob"
)

// Import jobs (MAT-88) are not created by this build; a stray row is failed
// rather than left active.
func (a *ArchiveService) advanceImport(ctx context.Context, j *archivejob.Job) error {
	return a.fail(ctx, j, "creating agents from archives is not supported by this control plane")
}

func (a *ArchiveService) discardImportDestination(ctx context.Context, j *archivejob.Job) error {
	return nil
}

func (a *ArchiveService) serveRestoreReads(w http.ResponseWriter, r *http.Request, j *archivejob.Job) {
	http.Error(w, "forbidden", http.StatusForbidden)
}

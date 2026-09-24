package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/matty-v/kyber/pkg/archivejob"
)

func TestDeleteExport(t *testing.T) {
	h := newExportHarness(t)
	ctx := context.Background()

	// A running export cannot be deleted; it is canceled instead.
	other := newExportHarness(t)
	running := other.runToArchiving()
	if rr := other.do(http.MethodDelete, "/api/v1/agents/"+exportAgent+"/exports/"+running, testAPIKey); rr.Code != http.StatusConflict {
		t.Fatalf("delete running export = %d %s", rr.Code, rr.Body.String())
	}

	id, _ := h.completedExport()
	rr := h.do(http.MethodPost, "/api/v1/agents/"+exportAgent+"/exports/"+id+"/download-link", testAPIKey)
	var link struct{ URL string }
	json.Unmarshal(rr.Body.Bytes(), &link)

	// An import reading it holds it.
	imp := h.startImport(map[string]string{"exportId": id})
	if rr := h.do(http.MethodDelete, "/api/v1/agents/"+exportAgent+"/exports/"+id, testAPIKey); rr.Code != http.StatusConflict {
		t.Fatalf("delete export an import reads = %d %s", rr.Code, rr.Body.String())
	}
	if rr := h.post("/api/v1/agent-imports/"+imp+"/cancel", testAPIKey, nil); rr.Code != http.StatusAccepted {
		t.Fatalf("cancel import = %d", rr.Code)
	}
	h.svc.Tick(ctx)
	if j := h.job(imp); !j.State.Terminal() {
		t.Fatalf("import state = %s", j.State)
	}

	if rr := h.do(http.MethodDelete, "/api/v1/agents/"+exportAgent+"/exports/"+id, testAPIKey); rr.Code != http.StatusNoContent {
		t.Fatalf("delete = %d %s", rr.Code, rr.Body.String())
	}
	j := h.job(id)
	if j.State != archivejob.StateExpired || j.Message != "Deleted" || j.WrappedKey != nil {
		t.Errorf("deleted export = %s %q key=%v", j.State, j.Message, j.WrappedKey != nil)
	}
	if _, err := h.store.Open(ctx, j.ObjectKey, 0, -1); err == nil {
		t.Error("archive object still stored")
	}
	if rr := h.do(http.MethodGet, link.URL, ""); rr.Code != http.StatusNotFound {
		t.Errorf("download after delete = %d, want 404", rr.Code)
	}
	// Deleting again is a no-op.
	if rr := h.do(http.MethodDelete, "/api/v1/agents/"+exportAgent+"/exports/"+id, testAPIKey); rr.Code != http.StatusNoContent {
		t.Errorf("second delete = %d", rr.Code)
	}
	if rr := h.post("/api/v1/agent-imports", testAPIKey, importBody(map[string]string{"exportId": id}, "again", "20Gi")); rr.Code != http.StatusBadRequest {
		t.Errorf("import from a deleted export = %d", rr.Code)
	}
}

func TestDeleteUpload(t *testing.T) {
	h := newExportHarness(t)
	ctx := context.Background()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/archive-uploads", bytes.NewReader(sampleDisk(t)))
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	rr := httptest.NewRecorder()
	h.server.BuildHandler().ServeHTTP(rr, req)
	var up struct{ ID string }
	json.Unmarshal(rr.Body.Bytes(), &up)
	h.svc.Tick(ctx)
	h.playVerifyPod(up.ID)
	h.svc.Tick(ctx)

	if rr := h.do(http.MethodDelete, "/api/v1/archive-uploads/"+up.ID, testAPIKey); rr.Code != http.StatusNoContent {
		t.Fatalf("delete upload = %d %s", rr.Code, rr.Body.String())
	}
	j := h.job(up.ID)
	if _, err := h.store.Open(ctx, j.ObjectKey, 0, -1); err == nil || j.State != archivejob.StateExpired {
		t.Errorf("upload not deleted: %s %v", j.State, err)
	}
	if rr := h.do(http.MethodDelete, "/api/v1/archive-uploads/nope", testAPIKey); rr.Code != http.StatusNotFound {
		t.Errorf("delete unknown upload = %d", rr.Code)
	}
}

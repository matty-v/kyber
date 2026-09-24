package api_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/matty-v/kyber/pkg/archivejob"
	"github.com/matty-v/kyber/pkg/archivestore"
	"github.com/matty-v/kyber/pkg/diskarchive"
)

// largeSampleDisk is sampleDisk plus an incompressible file, so the archive
// spans several 64 KiB upload parts.
func largeSampleDisk(t *testing.T) []byte {
	t.Helper()
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "agentroot/home/kyber/dev/identity/skills/s"), 0o755)
	os.WriteFile(filepath.Join(root, "agentroot/home/kyber/dev/identity/skills/s/SKILL.md"), []byte("# s\n"), 0o644)
	blob := make([]byte, 200<<10)
	_, _ = rand.Read(blob)
	os.WriteFile(filepath.Join(root, "agentroot/home/kyber/notes.bin"), blob, 0o644)
	var buf bytes.Buffer
	if _, err := diskarchive.Write(context.Background(), root, &buf, diskarchive.WriteOptions{Source: diskarchive.Source{Agent: exportAgent}}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

type partUpload struct {
	ID            string `json:"id"`
	PartSize      int64  `json:"partSize"`
	PartCount     int    `json:"partCount"`
	PartsReceived []int  `json:"partsReceived"`
}

func (h *exportHarness) startPartUpload(size int) partUpload {
	h.t.Helper()
	rr := h.post("/api/v1/archive-uploads", testAPIKey, map[string]any{"size": size})
	if rr.Code != http.StatusCreated {
		h.t.Fatalf("start = %d %s", rr.Code, rr.Body.String())
	}
	var up partUpload
	json.Unmarshal(rr.Body.Bytes(), &up)
	return up
}

func (h *exportHarness) putPart(id string, n int, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPut, fmt.Sprintf("/api/v1/archive-uploads/%s/parts/%d", id, n), bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	rr := httptest.NewRecorder()
	h.server.BuildHandler().ServeHTTP(rr, req)
	return rr
}

func part(data []byte, size int64, n int) []byte {
	return data[int64(n)*size : min(int64(n+1)*size, int64(len(data)))]
}

func TestUploadInPartsThenImport(t *testing.T) {
	h := newExportHarness(t)
	h.svc.Limits.UploadPartBytes = archivestore.ChunkSize
	ctx := context.Background()
	data := largeSampleDisk(t)

	up := h.startPartUpload(len(data))
	if up.PartSize != archivestore.ChunkSize || up.PartCount < 3 {
		t.Fatalf("plan = %+v, want 64 KiB parts and at least 3 of them", up)
	}
	last := up.PartCount - 1
	// Out of order, with the last (short) part first and one part retried.
	order := []int{last}
	for n := last - 1; n >= 0; n-- {
		order = append(order, n)
	}
	order = append(order, 1)
	for i, n := range order {
		if i == 1 {
			if rr := h.post("/api/v1/archive-uploads/"+up.ID+"/complete", testAPIKey, nil); rr.Code != http.StatusConflict {
				t.Fatalf("complete with parts missing = %d %s", rr.Code, rr.Body.String())
			}
		}
		if rr := h.putPart(up.ID, n, part(data, up.PartSize, n)); rr.Code != http.StatusOK {
			t.Fatalf("part %d = %d %s", n, rr.Code, rr.Body.String())
		}
	}
	if j := h.job(up.ID); len(j.PartsReceived) != up.PartCount || j.BytesDone != int64(len(data)) {
		t.Fatalf("received %v (%d bytes), want %d parts and %d bytes", j.PartsReceived, j.BytesDone, up.PartCount, len(data))
	}

	if rr := h.post("/api/v1/archive-uploads/"+up.ID+"/complete", testAPIKey, nil); rr.Code != http.StatusAccepted {
		t.Fatalf("complete = %d %s", rr.Code, rr.Body.String())
	}
	if rr := h.post("/api/v1/archive-uploads/"+up.ID+"/complete", testAPIKey, nil); rr.Code != http.StatusConflict {
		t.Fatalf("second complete = %d, want 409", rr.Code)
	}
	for n := range up.PartCount {
		if _, err := h.store.Size(ctx, fmt.Sprintf("uploads/%s/parts/%06d.sealed", up.ID, n)); err != archivestore.ErrNotFound {
			t.Errorf("part %d left in the store: %v", n, err)
		}
	}
	sealed, err := h.store.Size(ctx, "uploads/"+up.ID+"/archive.sealed")
	if err != nil || sealed != archivestore.SealedLength(int64(len(data))) {
		t.Fatalf("assembled object = %d bytes (%v), want %d", sealed, err, archivestore.SealedLength(int64(len(data))))
	}

	h.svc.Tick(ctx) // verification pod
	h.playVerifyPod(up.ID)
	h.svc.Tick(ctx)
	if j := h.job(up.ID); j.State != archivejob.StateCompleted || j.Summary == nil {
		t.Fatalf("upload state = %s (%s)", j.State, j.Error)
	}
	id := h.startImport(map[string]string{"uploadId": up.ID})
	h.svc.Tick(ctx)
	if got := h.readRestoreSource(id, h.podTokenFor("disk-import-"+id)); !bytes.Equal(got, data) {
		t.Fatal("restore source differs from the archive sent in parts")
	}
}

func TestUploadPartValidation(t *testing.T) {
	h := newExportHarness(t)
	h.svc.Limits.UploadPartBytes = archivestore.ChunkSize
	data := largeSampleDisk(t)
	up := h.startPartUpload(len(data))

	if rr := h.putPart(up.ID, 0, part(data, up.PartSize, 0)[:100]); rr.Code != http.StatusBadRequest {
		t.Errorf("short part = %d, want 400", rr.Code)
	}
	if rr := h.putPart(up.ID, up.PartCount, []byte("x")); rr.Code != http.StatusBadRequest {
		t.Errorf("part past the end = %d, want 400", rr.Code)
	}
	if j := h.job(up.ID); len(j.PartsReceived) != 0 {
		t.Errorf("a rejected part was recorded: %v", j.PartsReceived)
	}
	if rr := h.post("/api/v1/archive-uploads", testAPIKey, map[string]any{"size": 0}); rr.Code != http.StatusBadRequest {
		t.Errorf("size 0 = %d, want 400", rr.Code)
	}
	if rr := h.post("/api/v1/archive-uploads", testAPIKey, map[string]any{"size": 11 << 20}); rr.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("size over the limit = %d, want 413", rr.Code)
	}
}

func TestIdleUploadInPartsFailsAndDeletesParts(t *testing.T) {
	h := newExportHarness(t)
	h.svc.Limits.UploadPartBytes = archivestore.ChunkSize
	ctx := context.Background()
	data := largeSampleDisk(t)
	up := h.startPartUpload(len(data))
	if rr := h.putPart(up.ID, 0, part(data, up.PartSize, 0)); rr.Code != http.StatusOK {
		t.Fatalf("part 0 = %d", rr.Code)
	}
	h.now = h.now.Add(10 * time.Minute)
	h.svc.Tick(ctx)
	if j := h.job(up.ID); j.State != archivejob.StateRunning {
		t.Fatalf("upload failed before going idle: %s", j.Error)
	}
	h.now = h.now.Add(31 * time.Minute)
	h.svc.Tick(ctx)
	if j := h.job(up.ID); j.State != archivejob.StateFailed {
		t.Fatalf("idle upload state = %s", j.State)
	}
	if _, err := h.store.Size(ctx, "uploads/"+up.ID+"/parts/000000.sealed"); err != archivestore.ErrNotFound {
		t.Errorf("idle upload left its parts: %v", err)
	}
	if rr := h.putPart(up.ID, 1, part(data, up.PartSize, 1)); rr.Code != http.StatusConflict {
		t.Errorf("part after failure = %d, want 409", rr.Code)
	}
}

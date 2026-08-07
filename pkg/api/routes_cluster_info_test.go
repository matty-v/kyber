package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestServer_handleClusterInfo(t *testing.T) {
	s := &Server{
		ClusterName:  "kyber-test",
		ChartVersion: "1.6.0",
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/cluster-info", nil)
	w := httptest.NewRecorder()
	s.handleClusterInfo(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	var got clusterInfoResponse
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if got.Name != "kyber-test" {
		t.Errorf("Name = %q, want kyber-test", got.Name)
	}
	if got.Version != "1.6.0" {
		t.Errorf("Version = %q, want 1.6.0", got.Version)
	}
	wantCaps := []string{
		"agents", "machines", "shell", "inbound",
		"command-palette", "activity-stream",
	}
	if len(got.Capabilities) != len(wantCaps) {
		t.Fatalf("Capabilities len = %d, want %d", len(got.Capabilities), len(wantCaps))
	}
	for i, c := range wantCaps {
		if got.Capabilities[i] != c {
			t.Errorf("Capabilities[%d] = %q, want %q", i, got.Capabilities[i], c)
		}
	}
}

func TestServer_handleClusterInfo_RejectsNonGet(t *testing.T) {
	s := &Server{}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/cluster-info", nil)
	w := httptest.NewRecorder()
	s.handleClusterInfo(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", w.Code)
	}
}

func TestServer_handleClusterInfo_EmptyName(t *testing.T) {
	// An unconfigured cluster (KYBER_CLUSTER_NAME unset) returns empty name —
	// not an error. The PWA renders blank in the header, which is acceptable
	// for first-deploy state before the chart values are updated.
	s := &Server{ClusterName: "", ChartVersion: "1.6.0"}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/cluster-info", nil)
	w := httptest.NewRecorder()
	s.handleClusterInfo(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
}

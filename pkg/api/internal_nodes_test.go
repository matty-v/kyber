package api_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/matty-v/kyber/pkg/api"
	"github.com/matty-v/kyber/pkg/briefstore"
	"github.com/matty-v/kyber/pkg/metricsstore"
)

func newNodeServer(t *testing.T, ns metricsstore.NodeStore) *httptest.Server {
	t.Helper()
	srv := api.NewInternalServer(briefstore.NewMemoryStore(),
		api.WithNodeStore(ns),
	)
	return httptest.NewServer(srv.Handler())
}

func TestHandleNodeResources_StoresSample(t *testing.T) {
	ns := metricsstore.NewMemoryNodeStore()
	ts := newNodeServer(t, ns)
	defer ts.Close()

	body := `{"cpuPercent":42.5,"memUsedBytes":1073741824,"memTotalBytes":8589934592,"diskUsedBytes":5368709120,"diskTotalBytes":107374182400,"updatedAt":"2026-05-25T00:00:00Z"}`
	resp, err := http.Post(ts.URL+"/internal/nodes/node-1/resources", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want 200", resp.StatusCode)
	}

	ctx := context.Background()
	samples, err := ns.GetAllNodes(ctx, "")
	if err != nil {
		t.Fatalf("GetAllNodes: %v", err)
	}
	if len(samples) != 1 {
		t.Fatalf("want 1 sample, got %d", len(samples))
	}
	if samples[0].CPUPercent != 42.5 {
		t.Errorf("CPUPercent = %f, want 42.5", samples[0].CPUPercent)
	}
}

func TestHandleNodeResources_InvalidDNS1123Name(t *testing.T) {
	ns := metricsstore.NewMemoryNodeStore()
	ts := newNodeServer(t, ns)
	defer ts.Close()

	resp, _ := http.Post(ts.URL+"/internal/nodes/INVALID_NODE/resources", "application/json",
		strings.NewReader(`{"cpuPercent":50}`))
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", resp.StatusCode)
	}
}

func TestHandleNodeResources_CPUOutOfRange(t *testing.T) {
	ns := metricsstore.NewMemoryNodeStore()
	ts := newNodeServer(t, ns)
	defer ts.Close()

	for _, body := range []string{
		`{"cpuPercent":101}`,
		`{"cpuPercent":-1}`,
	} {
		resp, _ := http.Post(ts.URL+"/internal/nodes/node-1/resources", "application/json",
			strings.NewReader(body))
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("body=%s: status = %d, want 400", body, resp.StatusCode)
		}
	}
}

func TestHandleNodeResources_NegativeBytes(t *testing.T) {
	ns := metricsstore.NewMemoryNodeStore()
	ts := newNodeServer(t, ns)
	defer ts.Close()

	body := `{"cpuPercent":50,"memUsedBytes":-1}`
	resp, _ := http.Post(ts.URL+"/internal/nodes/node-1/resources", "application/json",
		strings.NewReader(body))
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", resp.StatusCode)
	}
}

func TestHandleNodeResources_NotConfigured(t *testing.T) {
	srv := api.NewInternalServer(briefstore.NewMemoryStore())
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, _ := http.Post(ts.URL+"/internal/nodes/node-1/resources", "application/json",
		strings.NewReader(`{"cpuPercent":50}`))
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status: got %d, want 503", resp.StatusCode)
	}
}

func TestHandleNodeResources_MethodNotAllowed(t *testing.T) {
	ns := metricsstore.NewMemoryNodeStore()
	ts := newNodeServer(t, ns)
	defer ts.Close()

	resp, _ := http.Get(ts.URL + "/internal/nodes/node-1/resources")
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status: got %d, want 405", resp.StatusCode)
	}
}

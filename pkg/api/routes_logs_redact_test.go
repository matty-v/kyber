package api_test

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/matty-v/kyber/pkg/api"
	"github.com/matty-v/kyber/pkg/messagebuffer"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// secretStorageDetail is a stand-in for the internal storage topology (bucket
// name, object-key prefix, GCP project id) that upstream errors can carry. None
// of it must ever appear in a public HTTP error body. (kyber#434)
const secretStorageDetail = "kyber-prod-secret-logs-bucket/agents/dave project=my-gcp-project-99999"

// TestArchiveLogs_502_RedactsError forces the ArchiveReader to fail with an
// error that embeds internal GCS topology and asserts the 502 body carries a
// generic message with NONE of that topology leaked.
func TestArchiveLogs_502_RedactsError(t *testing.T) {
	reader := &fakeArchiveReader{
		byAgent: map[string][]api.LogLine{"dave": nil},
		err:     errors.New("list " + secretStorageDetail + ": googleapi: Error 403"),
	}
	h := buildArchiveHandler(t, reader, sampleAgentCRD("dave"))

	req := authedRequest(t, http.MethodGet,
		"/api/v1/agents/dave/logs?source=archive&since=2026-06-04T00:00:00Z&until=2026-06-04T01:00:00Z", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadGateway {
		t.Fatalf("want 502, got %d: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "failed to read archived logs") {
		t.Errorf("want generic message 'failed to read archived logs', got: %s", body)
	}
	if !strings.Contains(body, "archive_read_error") {
		t.Errorf("want stable error code archive_read_error, got: %s", body)
	}
	// The leak guard: no part of the upstream error / storage topology.
	for _, leak := range []string{secretStorageDetail, "kyber-prod-secret-logs-bucket", "my-gcp-project-99999", "googleapi", "Error 403"} {
		if strings.Contains(body, leak) {
			t.Errorf("LEAK: 502 body exposes internal detail %q: %s", leak, body)
		}
	}
}

// TestAgentLogs_StreamError_RedactsError forces the kubelet stream open to fail
// with a non-NotFound (500) carrying internal detail; the 500 body must be
// generic with no raw upstream error leaked. (routes_logs.go handleAgentLogs)
func TestAgentLogs_StreamError_RedactsError(t *testing.T) {
	// Mock "k8s apiserver" returns 500 with an internal-detail body for the
	// pod-logs request — a non-NotFound error → the handler's stream_error branch.
	mockAPIServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"InternalError","message":"`+secretStorageDetail+`","code":500}`)
	}))
	defer mockAPIServer.Close()

	agent := sampleAgentCRD("dave")
	scheme := mustNewScheme(t)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(agent).Build()
	s := &api.Server{
		K8sClient:     fakeClient,
		MessageBuffer: messagebuffer.NewMemoryBuffer(),
		APIKey:        testAPIKey,
		Namespace:     "kyber-system",
		Clientset:     buildClientsetWithServer(t, mockAPIServer.URL),
	}
	h := s.BuildHandler()

	req := authedRequest(t, http.MethodGet, "/api/v1/agents/dave/logs", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "failed to open log stream") {
		t.Errorf("want generic message 'failed to open log stream', got: %s", body)
	}
	if !strings.Contains(body, "stream_error") {
		t.Errorf("want stable error code stream_error, got: %s", body)
	}
	if strings.Contains(body, secretStorageDetail) {
		t.Errorf("LEAK: 500 body exposes upstream detail %q: %s", secretStorageDetail, body)
	}
}

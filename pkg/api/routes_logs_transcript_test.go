package api_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"

	"github.com/matty-v/kyber/pkg/api"
	"github.com/matty-v/kyber/pkg/messagebuffer"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// buildTranscriptHandler builds a logs handler with an injected TranscriptReader
// (and, optionally, a distinct ArchiveReader to prove the two surfaces don't
// cross-read). No Clientset — the transcript path must not need the kubelet path.
func buildTranscriptHandler(t *testing.T, transcript api.ArchiveReader, archive api.ArchiveReader, objs ...runtime.Object) http.Handler {
	t.Helper()
	scheme := mustNewScheme(t)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...).Build()
	s := &api.Server{
		K8sClient:        fakeClient,
		MessageBuffer:    messagebuffer.NewMemoryBuffer(),
		APIKey:           testAPIKey,
		Namespace:        "kyber-system",
		ArchiveReader:    archive,
		TranscriptReader: transcript,
	}
	return s.BuildHandler()
}

// TestTranscriptLogs_WindowReturned is the happy-path AC: source=transcript
// returns the agent's session JSONL lines for the absolute window, read from the
// TranscriptReader (Clientset nil — not the kubelet path).
func TestTranscriptLogs_WindowReturned(t *testing.T) {
	reader := &fakeArchiveReader{byAgent: map[string][]api.LogLine{
		"dave": {
			{Timestamp: mustRFC3339(t, "2026-06-03T10:00:00Z"), Text: `{"type":"user","uuid":"1"}`},
			{Timestamp: mustRFC3339(t, "2026-06-03T10:30:00Z"), Text: `{"type":"assistant","uuid":"2"}`},
		},
	}}
	h := buildTranscriptHandler(t, reader, nil, sampleAgentCRD("dave"))

	req := authedRequest(t, http.MethodGet,
		"/api/v1/agents/dave/logs?source=transcript&since=2026-06-03T09:00:00Z&until=2026-06-03T11:00:00Z", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, `"type":"user"`) || !strings.Contains(body, `"type":"assistant"`) {
		t.Errorf("want both transcript JSONL lines in body, got: %q", body)
	}
	if reader.gotAgent != "dave" {
		t.Errorf("reader queried for %q, want dave", reader.gotAgent)
	}
	if !reader.gotSince.Equal(mustRFC3339(t, "2026-06-03T09:00:00Z")) ||
		!reader.gotUntil.Equal(mustRFC3339(t, "2026-06-03T11:00:00Z")) {
		t.Errorf("reader window = [%v,%v], want [09:00,11:00]", reader.gotSince, reader.gotUntil)
	}
}

// TestTranscriptLogs_EmptyWindow200 is the edge-case AC: a fresh pod / quiet
// window returns HTTP 200 with zero lines, NOT an error.
func TestTranscriptLogs_EmptyWindow200(t *testing.T) {
	reader := &fakeArchiveReader{byAgent: map[string][]api.LogLine{"dave": nil}}
	h := buildTranscriptHandler(t, reader, nil, sampleAgentCRD("dave"))
	req := authedRequest(t, http.MethodGet,
		"/api/v1/agents/dave/logs?source=transcript&since=2026-06-03T09:00:00Z&until=2026-06-03T11:00:00Z", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200 for empty transcript window, got %d: %s", rr.Code, rr.Body.String())
	}
	if body := strings.TrimSpace(rr.Body.String()); body != "" {
		t.Errorf("want empty body for no-activity window, got: %q", body)
	}
}

// TestTranscriptLogs_BadWindow is the validation AC: missing/malformed
// since/until return 400 invalid_window, same as source=archive.
func TestTranscriptLogs_BadWindow(t *testing.T) {
	reader := &fakeArchiveReader{byAgent: map[string][]api.LogLine{"dave": nil}}
	h := buildTranscriptHandler(t, reader, nil, sampleAgentCRD("dave"))

	cases := []struct{ name, query string }{
		{"missing both", "source=transcript"},
		{"missing until", "source=transcript&since=2026-06-03T10:00:00Z"},
		{"missing since", "source=transcript&until=2026-06-03T10:00:00Z"},
		{"malformed since", "source=transcript&since=not-a-time&until=2026-06-03T10:00:00Z"},
		{"relative since", "source=transcript&since=5m&until=2026-06-03T10:00:00Z"},
		{"until before since", "source=transcript&since=2026-06-03T11:00:00Z&until=2026-06-03T10:00:00Z"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := authedRequest(t, http.MethodGet, "/api/v1/agents/dave/logs?"+tc.query, nil)
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			if rr.Code != http.StatusBadRequest {
				t.Errorf("want 400, got %d: %s", rr.Code, rr.Body.String())
			}
			if !strings.Contains(rr.Body.String(), "invalid_window") {
				t.Errorf("want invalid_window, got: %s", rr.Body.String())
			}
		})
	}
	if reader.callCount != 0 {
		t.Errorf("reader must not be called on bad window; was called %d times", reader.callCount)
	}
}

// TestTranscriptLogs_NoReader is the 503 AC: when TranscriptReader is
// unconfigured, source=transcript returns 503 (distinct from 400/500) and names
// the missing config — and source=archive (if configured) is unaffected.
func TestTranscriptLogs_NoReader(t *testing.T) {
	scheme := mustNewScheme(t)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithRuntimeObjects(sampleAgentCRD("dave")).Build()
	s := &api.Server{
		K8sClient:                fakeClient,
		MessageBuffer:            messagebuffer.NewMemoryBuffer(),
		APIKey:                   testAPIKey,
		Namespace:                "kyber-system",
		TranscriptReader:         nil,
		TranscriptDisabledReason: "KYBER_LOG_ARCHIVE_BUCKET unset",
	}
	h := s.BuildHandler()
	req := authedRequest(t, http.MethodGet,
		"/api/v1/agents/dave/logs?source=transcript&since=2026-06-03T09:00:00Z&until=2026-06-03T11:00:00Z", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503 when TranscriptReader nil, got %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "KYBER_LOG_ARCHIVE_BUCKET") {
		t.Errorf("want 503 body to name the missing config key, got: %q", rr.Body.String())
	}
}

// TestTranscriptLogs_NoAuth is the authz AC: unauthenticated requests are
// rejected identically to source=archive (auth inherited from the protected mux,
// reached before the reader). Critical because transcripts carry full session
// content including possible secrets.
func TestTranscriptLogs_NoAuth(t *testing.T) {
	reader := &fakeArchiveReader{byAgent: map[string][]api.LogLine{"dave": {
		{Timestamp: mustRFC3339(t, "2026-06-03T10:00:00Z"), Text: "SECRET-SESSION-CONTENT"},
	}}}
	h := buildTranscriptHandler(t, reader, nil, sampleAgentCRD("dave"))
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/agents/dave/logs?source=transcript&since=2026-06-03T09:00:00Z&until=2026-06-03T11:00:00Z", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("want 401 for unauthenticated transcript request, got %d: %s", rr.Code, rr.Body.String())
	}
	if reader.callCount != 0 {
		t.Errorf("reader must not be reached unauthenticated; called %d times", reader.callCount)
	}
	if strings.Contains(rr.Body.String(), "SECRET-SESSION-CONTENT") {
		t.Fatal("LEAK: unauthenticated response exposed session content")
	}
}

// TestTranscriptLogs_AgentNotFound is the 404 AC: an unknown agent returns 404
// on the transcript path too (the existence check runs before the source switch).
func TestTranscriptLogs_AgentNotFound(t *testing.T) {
	reader := &fakeArchiveReader{byAgent: map[string][]api.LogLine{}}
	h := buildTranscriptHandler(t, reader, nil) // no agent CRDs
	req := authedRequest(t, http.MethodGet,
		"/api/v1/agents/ghost/logs?source=transcript&since=2026-06-03T09:00:00Z&until=2026-06-03T11:00:00Z", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("want 404 for unknown agent, got %d: %s", rr.Code, rr.Body.String())
	}
	if reader.callCount != 0 {
		t.Errorf("reader must not be called for a missing agent; called %d times", reader.callCount)
	}
}

// TestTranscriptLogs_InvalidSourceListsTranscript is the AC that an unknown
// source value 400s AND the error message now lists transcript alongside
// kubelet and archive.
func TestTranscriptLogs_InvalidSourceListsTranscript(t *testing.T) {
	reader := &fakeArchiveReader{byAgent: map[string][]api.LogLine{"dave": nil}}
	h := buildTranscriptHandler(t, reader, nil, sampleAgentCRD("dave"))
	req := authedRequest(t, http.MethodGet, "/api/v1/agents/dave/logs?source=bogus", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for invalid source, got %d: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "invalid_source") {
		t.Errorf("want invalid_source code, got: %s", body)
	}
	if !strings.Contains(body, "transcript") {
		t.Errorf("invalid_source message must list 'transcript' as a valid value, got: %s", body)
	}
}

// TestTranscriptLogs_SurfaceIsolation proves the two surfaces don't cross-read:
// source=transcript reads ONLY the TranscriptReader, never the ArchiveReader,
// even with both wired and holding distinct lines for the same agent.
func TestTranscriptLogs_SurfaceIsolation(t *testing.T) {
	transcript := &fakeArchiveReader{byAgent: map[string][]api.LogLine{
		"dave": {{Timestamp: mustRFC3339(t, "2026-06-03T10:00:00Z"), Text: "TRANSCRIPT-LINE"}},
	}}
	archive := &fakeArchiveReader{byAgent: map[string][]api.LogLine{
		"dave": {{Timestamp: mustRFC3339(t, "2026-06-03T10:00:00Z"), Text: "ARCHIVE-LINE"}},
	}}
	h := buildTranscriptHandler(t, transcript, archive, sampleAgentCRD("dave"))

	// source=transcript → only the transcript reader is consulted.
	req := authedRequest(t, http.MethodGet,
		"/api/v1/agents/dave/logs?source=transcript&since=2026-06-03T09:00:00Z&until=2026-06-03T11:00:00Z", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "TRANSCRIPT-LINE") {
		t.Errorf("want transcript line, got: %q", body)
	}
	if strings.Contains(body, "ARCHIVE-LINE") {
		t.Fatal("lane breach: transcript surface returned an archive line")
	}
	if archive.callCount != 0 {
		t.Errorf("archive reader must not be touched by source=transcript; called %d times", archive.callCount)
	}
}

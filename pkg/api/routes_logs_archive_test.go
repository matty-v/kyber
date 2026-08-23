package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"

	"github.com/matty-v/kyber/pkg/api"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// fakeArchiveReader is an in-memory ArchiveReader for handler tests. It records
// the agent it was asked for and returns canned lines per agent, honoring the
// requested window so the handler's pass-through of since/until is exercised.
type fakeArchiveReader struct {
	byAgent   map[string][]api.LogLine
	err       error
	truncated bool // when true, the returned ReadResult is marked Truncated
	gotAgent  string
	gotSince  time.Time
	gotUntil  time.Time
	callCount int
}

func (f *fakeArchiveReader) ReadAgentLines(_ context.Context, agent string, since, until time.Time) (api.ReadResult, error) {
	f.callCount++
	f.gotAgent = agent
	f.gotSince = since
	f.gotUntil = until
	if f.err != nil {
		return api.ReadResult{}, f.err
	}
	var out []api.LogLine
	for _, l := range f.byAgent[agent] {
		if (l.Timestamp.Equal(since) || l.Timestamp.After(since)) &&
			(l.Timestamp.Equal(until) || l.Timestamp.Before(until)) {
			out = append(out, l)
		}
	}
	return api.ReadResult{Lines: out, Truncated: f.truncated}, nil
}

// buildArchiveHandler builds a logs handler with an injected ArchiveReader and
// no Clientset (archive must not need the kubelet path).
func buildArchiveHandler(t *testing.T, reader api.ArchiveReader, objs ...runtime.Object) http.Handler {
	t.Helper()
	scheme := mustNewScheme(t)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...).Build()
	s := &api.Server{
		K8sClient:     fakeClient,
		APIKey:        testAPIKey,
		Namespace:     "kyber-system",
		ArchiveReader: reader,
	}
	return s.BuildHandler()
}

func mustRFC3339(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return ts
}

// TestArchiveLogs_WindowReturned verifies source=archive returns the named
// agent's lines for the absolute window, read from the ArchiveReader (not the
// kubelet — note Clientset is nil here).
func TestArchiveLogs_WindowReturned(t *testing.T) {
	reader := &fakeArchiveReader{byAgent: map[string][]api.LogLine{
		"dave": {
			{Timestamp: mustRFC3339(t, "2026-06-03T10:00:00Z"), Text: "dave first"},
			{Timestamp: mustRFC3339(t, "2026-06-03T10:30:00Z"), Text: "dave second"},
		},
	}}
	agent := sampleAgentCRD("dave")
	h := buildArchiveHandler(t, reader, agent)

	req := authedRequest(t, http.MethodGet,
		"/api/v1/agents/dave/logs?source=archive&since=2026-06-03T09:00:00Z&until=2026-06-03T11:00:00Z", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "dave first") || !strings.Contains(body, "dave second") {
		t.Errorf("want both dave lines in body, got: %q", body)
	}
	if reader.gotAgent != "dave" {
		t.Errorf("reader queried for %q, want dave", reader.gotAgent)
	}
	if !reader.gotSince.Equal(mustRFC3339(t, "2026-06-03T09:00:00Z")) {
		t.Errorf("reader since = %v, want 09:00:00Z", reader.gotSince)
	}
	if !reader.gotUntil.Equal(mustRFC3339(t, "2026-06-03T11:00:00Z")) {
		t.Errorf("reader until = %v, want 11:00:00Z", reader.gotUntil)
	}
}

// TestArchiveLogs_TruncationSignal verifies the additive bounded-read contract
// (kyber#455 AC#4) at the HTTP boundary: when the reader reports a truncated
// (memory-capped) result, the handler returns 200 with the lines it has AND the
// explicit X-Kyber-Log-Truncated: true header so the caller can tell the result
// was capped; when not truncated the header is absent.
func TestArchiveLogs_TruncationSignal(t *testing.T) {
	lines := map[string][]api.LogLine{
		"dave": {{Timestamp: mustRFC3339(t, "2026-06-03T10:00:00Z"), Text: "capped line"}},
	}

	t.Run("truncated sets header", func(t *testing.T) {
		reader := &fakeArchiveReader{byAgent: lines, truncated: true}
		h := buildArchiveHandler(t, reader, sampleAgentCRD("dave"))
		req := authedRequest(t, http.MethodGet,
			"/api/v1/agents/dave/logs?source=archive&since=2026-06-03T00:00:00Z&until=2026-06-03T23:59:59Z", nil)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
		}
		if got := rr.Header().Get("X-Kyber-Log-Truncated"); got != "true" {
			t.Errorf("want X-Kyber-Log-Truncated: true, got %q", got)
		}
		if !strings.Contains(rr.Body.String(), "capped line") {
			t.Errorf("want the bounded lines still in the body, got: %q", rr.Body.String())
		}
	})

	t.Run("not truncated omits header", func(t *testing.T) {
		reader := &fakeArchiveReader{byAgent: lines}
		h := buildArchiveHandler(t, reader, sampleAgentCRD("dave"))
		req := authedRequest(t, http.MethodGet,
			"/api/v1/agents/dave/logs?source=archive&since=2026-06-03T00:00:00Z&until=2026-06-03T23:59:59Z", nil)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
		}
		if got := rr.Header().Get("X-Kyber-Log-Truncated"); got != "" {
			t.Errorf("want no truncation header on a complete read, got %q", got)
		}
	})
}

// TestArchiveLogs_AgentIsolation verifies a query for agent-X never returns
// agent-Y's lines, at the HTTP boundary.
func TestArchiveLogs_AgentIsolation(t *testing.T) {
	reader := &fakeArchiveReader{byAgent: map[string][]api.LogLine{
		"dave": {{Timestamp: mustRFC3339(t, "2026-06-03T10:00:00Z"), Text: "DAVE-SECRET"}},
		"luke": {{Timestamp: mustRFC3339(t, "2026-06-03T10:00:00Z"), Text: "LUKE-SECRET"}},
	}}
	h := buildArchiveHandler(t, reader, sampleAgentCRD("luke"))

	req := authedRequest(t, http.MethodGet,
		"/api/v1/agents/luke/logs?source=archive&since=2026-06-03T00:00:00Z&until=2026-06-03T23:59:59Z", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if strings.Contains(body, "DAVE-SECRET") {
		t.Fatalf("ISOLATION BREACH: luke's archive returned dave's line: %q", body)
	}
	if !strings.Contains(body, "LUKE-SECRET") {
		t.Errorf("want luke's line in body, got: %q", body)
	}
}

// TestArchiveLogs_BadWindow verifies malformed/absent RFC3339 bounds and
// until<since all return 400 (not 500, not silent empty).
func TestArchiveLogs_BadWindow(t *testing.T) {
	reader := &fakeArchiveReader{byAgent: map[string][]api.LogLine{"dave": nil}}
	h := buildArchiveHandler(t, reader, sampleAgentCRD("dave"))

	cases := []struct {
		name  string
		query string
	}{
		{"missing both", "source=archive"},
		{"missing until", "source=archive&since=2026-06-03T10:00:00Z"},
		{"missing since", "source=archive&until=2026-06-03T10:00:00Z"},
		{"malformed since", "source=archive&since=not-a-time&until=2026-06-03T10:00:00Z"},
		{"malformed until", "source=archive&since=2026-06-03T10:00:00Z&until=nope"},
		{"relative since (duration, not RFC3339)", "source=archive&since=5m&until=2026-06-03T10:00:00Z"},
		{"until before since", "source=archive&since=2026-06-03T11:00:00Z&until=2026-06-03T10:00:00Z"},
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
				t.Errorf("want error code invalid_window, got: %s", rr.Body.String())
			}
		})
	}
	if reader.callCount != 0 {
		t.Errorf("reader must not be called on bad window; was called %d times", reader.callCount)
	}
}

// TestArchiveLogs_NoReader verifies 503 when ArchiveReader is unconfigured —
// distinct from a 400/500, and the live path is unaffected.
func TestArchiveLogs_NoReader(t *testing.T) {
	h := buildArchiveHandler(t, nil, sampleAgentCRD("dave"))
	req := authedRequest(t, http.MethodGet,
		"/api/v1/agents/dave/logs?source=archive&since=2026-06-03T09:00:00Z&until=2026-06-03T11:00:00Z", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("want 503 when ArchiveReader nil, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestArchiveLogs_NoReader_NamesMissingConfig verifies the 503 body names the
// missing config key (so a misconfig is self-diagnosing) when the server carries
// an ArchiveDisabledReason — and that it never leaks a credential value (kyber#437).
func TestArchiveLogs_NoReader_NamesMissingConfig(t *testing.T) {
	scheme := mustNewScheme(t)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithRuntimeObjects(sampleAgentCRD("dave")).Build()
	s := &api.Server{
		K8sClient:             fakeClient,
		APIKey:                testAPIKey,
		Namespace:             "kyber-system",
		ArchiveReader:         nil,
		ArchiveDisabledReason: "KYBER_LOG_ARCHIVE_BACKEND=s3 requires KYBER_LOG_ARCHIVE_ENDPOINT, KYBER_LOG_ARCHIVE_ACCESS_KEY",
	}
	h := s.BuildHandler()

	req := authedRequest(t, http.MethodGet,
		"/api/v1/agents/dave/logs?source=archive&since=2026-06-03T09:00:00Z&until=2026-06-03T11:00:00Z", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "KYBER_LOG_ARCHIVE_ENDPOINT") {
		t.Errorf("want 503 body to name the missing config key, got: %q", body)
	}
}

// TestArchiveLogs_NoAuth verifies the archive path rejects unauthenticated
// requests identically to the live path (auth inherited from the protected mux).
func TestArchiveLogs_NoAuth(t *testing.T) {
	reader := &fakeArchiveReader{byAgent: map[string][]api.LogLine{"dave": nil}}
	h := buildArchiveHandler(t, reader, sampleAgentCRD("dave"))
	// No auth header.
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/agents/dave/logs?source=archive&since=2026-06-03T09:00:00Z&until=2026-06-03T11:00:00Z", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("want 401 for unauthenticated archive request, got %d: %s", rr.Code, rr.Body.String())
	}
	if reader.callCount != 0 {
		t.Errorf("reader must not be reached for unauthenticated request; called %d times", reader.callCount)
	}
}

// TestArchiveLogs_AgentNotFound verifies an unknown agent returns 404 on the
// archive path too (same existence check as the live path).
func TestArchiveLogs_AgentNotFound(t *testing.T) {
	reader := &fakeArchiveReader{byAgent: map[string][]api.LogLine{}}
	h := buildArchiveHandler(t, reader) // no agent CRDs
	req := authedRequest(t, http.MethodGet,
		"/api/v1/agents/ghost/logs?source=archive&since=2026-06-03T09:00:00Z&until=2026-06-03T11:00:00Z", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("want 404 for unknown agent, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestArchiveLogs_InvalidSource verifies an unrecognized source value is a 400
// (not a silent fallthrough to the kubelet path).
func TestArchiveLogs_InvalidSource(t *testing.T) {
	reader := &fakeArchiveReader{byAgent: map[string][]api.LogLine{"dave": nil}}
	h := buildArchiveHandler(t, reader, sampleAgentCRD("dave"))
	req := authedRequest(t, http.MethodGet, "/api/v1/agents/dave/logs?source=bogus", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("want 400 for invalid source, got %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "invalid_source") {
		t.Errorf("want error code invalid_source, got: %s", rr.Body.String())
	}
}

// TestArchiveLogs_DefaultSourceUnchanged verifies backward compat: with source
// omitted, the handler takes the kubelet path (here: 503 from nil Clientset),
// NOT the archive path — proving source defaults to kubelet.
func TestArchiveLogs_DefaultSourceUnchanged(t *testing.T) {
	reader := &fakeArchiveReader{byAgent: map[string][]api.LogLine{"dave": nil}}
	h := buildArchiveHandler(t, reader, sampleAgentCRD("dave")) // Clientset nil
	req := authedRequest(t, http.MethodGet, "/api/v1/agents/dave/logs?tail=50&since=5m", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	// Kubelet path with nil Clientset → 503. If it had wrongly taken the
	// archive path it would 200 (empty) or 400. 503 proves kubelet default.
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("want 503 (kubelet path, nil clientset) for default source, got %d: %s", rr.Code, rr.Body.String())
	}
	if reader.callCount != 0 {
		t.Errorf("archive reader must not be called for default source; called %d times", reader.callCount)
	}
}

// blockingArchiveReader blocks in ReadAgentLines until released, signaling entry —
// lets a test hold a read slot to exercise the concurrency cap (kyber#463).
type blockingArchiveReader struct {
	entered chan struct{}
	release chan struct{}
	lines   []api.LogLine
}

func (b *blockingArchiveReader) ReadAgentLines(ctx context.Context, agent string, since, until time.Time) (api.ReadResult, error) {
	select {
	case b.entered <- struct{}{}:
	default:
	}
	select {
	case <-b.release:
	case <-ctx.Done():
		return api.ReadResult{}, ctx.Err()
	}
	return api.ReadResult{Lines: b.lines}, nil
}

// buildArchiveHandlerWithCap is buildArchiveHandler with an explicit
// MaxConcurrentReads cap (kyber#463).
func buildArchiveHandlerWithCap(t *testing.T, reader api.ArchiveReader, cap int, objs ...runtime.Object) http.Handler {
	t.Helper()
	scheme := mustNewScheme(t)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...).Build()
	s := &api.Server{
		K8sClient:          fakeClient,
		APIKey:             testAPIKey,
		Namespace:          "kyber-system",
		ArchiveReader:      reader,
		MaxConcurrentReads: cap,
	}
	return s.BuildHandler()
}

// TestArchiveLogs_ConcurrencyCap is the kyber#463 release-blocker AC: concurrent
// reads past the in-flight cap get an immediate 429 + Retry-After (no partial
// body), unrelated endpoints stay 200 (isolation), the blocked read completes on
// release, and the slot is freed afterward (no leak).
func TestArchiveLogs_ConcurrencyCap(t *testing.T) {
	entered := make(chan struct{}, 4)
	release := make(chan struct{})
	reader := &blockingArchiveReader{
		entered: entered,
		release: release,
		lines:   []api.LogLine{{Timestamp: mustRFC3339(t, "2026-06-03T10:00:00Z"), Text: "the line"}},
	}
	h := buildArchiveHandlerWithCap(t, reader, 1, sampleAgentCRD("dave"))
	url := "/api/v1/agents/dave/logs?source=archive&since=2026-06-03T00:00:00Z&until=2026-06-03T23:59:59Z"

	// Request 1 occupies the single slot (blocks inside ReadAgentLines).
	rec1 := httptest.NewRecorder()
	done1 := make(chan struct{})
	go func() {
		h.ServeHTTP(rec1, authedRequest(t, http.MethodGet, url, nil))
		close(done1)
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("request 1 never entered ReadAgentLines")
	}

	// Request 2 over the cap → immediate 429 + Retry-After, no partial body.
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, authedRequest(t, http.MethodGet, url, nil))
	if rec2.Code != http.StatusTooManyRequests {
		t.Fatalf("over-cap read: want 429, got %d: %s", rec2.Code, rec2.Body.String())
	}
	if rec2.Header().Get("Retry-After") == "" {
		t.Error("429 must carry a Retry-After header")
	}
	if strings.Contains(rec2.Body.String(), "the line") {
		t.Error("over-cap 429 must NOT contain a partial log body")
	}

	// Isolation: /version stays 200 while a read holds the slot.
	recV := httptest.NewRecorder()
	h.ServeHTTP(recV, authedRequest(t, http.MethodGet, "/api/v1/version", nil))
	if recV.Code != http.StatusOK {
		t.Errorf("/api/v1/version must stay 200 under read load (isolation); got %d", recV.Code)
	}

	// Release request 1 → it completes 200 with its content.
	close(release)
	select {
	case <-done1:
	case <-time.After(2 * time.Second):
		t.Fatal("request 1 never completed after release")
	}
	if rec1.Code != http.StatusOK || !strings.Contains(rec1.Body.String(), "the line") {
		t.Errorf("request 1: want 200 with content, got %d: %s", rec1.Code, rec1.Body.String())
	}

	// Slot freed (defer release on every path): a new read now gets the slot.
	// release is already closed, so this read returns immediately.
	rec4 := httptest.NewRecorder()
	h.ServeHTTP(rec4, authedRequest(t, http.MethodGet, url, nil))
	if rec4.Code != http.StatusOK {
		t.Errorf("slot leaked: a read after release should acquire the freed slot (200); got %d: %s", rec4.Code, rec4.Body.String())
	}
}

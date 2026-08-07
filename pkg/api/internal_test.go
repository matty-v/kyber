package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/matty-v/kyber/pkg/api"
	"github.com/matty-v/kyber/pkg/briefstore"
	"github.com/matty-v/kyber/pkg/metricsstore"
	"github.com/matty-v/kyber/pkg/tokenreport"
	"github.com/matty-v/kyber/pkg/tokenstore"
)

// makeTestBrief returns a minimal Brief for HTTP handler tests.
func makeTestBrief(agentName string) *briefstore.Brief {
	return &briefstore.Brief{
		Version:       1,
		AgentName:     agentName,
		Timestamp:     "2026-04-10T23:00:00Z",
		ShutdownType:  "planned",
		RestartReason: "operator",
		LastActivity:  "working on B2",
		Metadata: briefstore.BriefMetadata{
			PreviousModel: "claude-sonnet-4",
			UptimeSeconds: 3600,
			RestartCount:  0,
		},
	}
}

// newTestServer returns an InternalServer backed by a fresh MemoryStore,
// and an httptest.Server for making requests. The test server is closed when
// the returned cleanup function is called.
func newTestServer(t *testing.T) (*api.InternalServer, *briefstore.MemoryStore, *httptest.Server, func()) {
	t.Helper()
	store := briefstore.NewMemoryStore()
	srv := api.NewInternalServer(store)
	ts := httptest.NewServer(srv.Handler())
	return srv, store, ts, ts.Close
}

// TestInternalAPI_GetSessionBrief_Found verifies that a stored brief is returned as JSON.
func TestInternalAPI_GetSessionBrief_Found(t *testing.T) {
	_, store, ts, cleanup := newTestServer(t)
	defer cleanup()

	brief := makeTestBrief("dave")
	if err := store.Put(context.Background(), "dave", brief); err != nil {
		t.Fatalf("Put: %v", err)
	}

	resp, err := http.Get(ts.URL + "/internal/agents/dave/session-brief")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status: got %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type: got %q, want %q", ct, "application/json")
	}

	var got briefstore.Brief
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if got.AgentName != "dave" {
		t.Errorf("AgentName: got %q, want %q", got.AgentName, "dave")
	}
	if got.ShutdownType != "planned" {
		t.Errorf("ShutdownType: got %q, want %q", got.ShutdownType, "planned")
	}
	if got.Metadata.PreviousModel != "claude-sonnet-4" {
		t.Errorf("Metadata.PreviousModel: got %q", got.Metadata.PreviousModel)
	}
}

// TestInternalAPI_GetSessionBrief_NotFound verifies a 404 when no brief is stored.
func TestInternalAPI_GetSessionBrief_NotFound(t *testing.T) {
	_, _, ts, cleanup := newTestServer(t)
	defer cleanup()

	resp, err := http.Get(ts.URL + "/internal/agents/ghost/session-brief")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status: got %d, want %d", resp.StatusCode, http.StatusNotFound)
	}
}

// TestInternalAPI_WrongPath verifies that unrecognized paths under /internal/agents/ return 404.
func TestInternalAPI_WrongPath(t *testing.T) {
	_, _, ts, cleanup := newTestServer(t)
	defer cleanup()

	paths := []string{
		"/internal/agents/dave/other",
		"/internal/agents/",
		"/internal/agents//session-brief",
	}

	for _, p := range paths {
		resp, err := http.Get(ts.URL + p)
		if err != nil {
			t.Fatalf("GET %s: %v", p, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("path %q: status got %d, want 404", p, resp.StatusCode)
		}
	}
}

// TestInternalAPI_MethodNotAllowed verifies that POST returns 405.
func TestInternalAPI_MethodNotAllowed(t *testing.T) {
	_, _, ts, cleanup := newTestServer(t)
	defer cleanup()

	resp, err := http.Post(ts.URL+"/internal/agents/dave/session-brief", "application/json", nil)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status: got %d, want %d", resp.StatusCode, http.StatusMethodNotAllowed)
	}
}

// TestInternalAPI_AgentNameTooLong verifies that a name exceeding 253 characters returns 400.
func TestInternalAPI_AgentNameTooLong(t *testing.T) {
	_, _, ts, cleanup := newTestServer(t)
	defer cleanup()

	// 254-character agent name — one over the Kubernetes DNS subdomain limit.
	longName := strings.Repeat("a", 254)
	resp, err := http.Get(ts.URL + "/internal/agents/" + longName + "/session-brief")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status: got %d, want %d", resp.StatusCode, http.StatusBadRequest)
	}
}

// TestInternalAPI_MultipleAgents verifies isolation between different agents' briefs.
func TestInternalAPI_MultipleAgents(t *testing.T) {
	_, store, ts, cleanup := newTestServer(t)
	defer cleanup()

	ctx := context.Background()
	agents := []string{"dave", "chewie", "han"}
	for _, name := range agents {
		b := makeTestBrief(name)
		b.LastActivity = name + "-activity"
		if err := store.Put(ctx, name, b); err != nil {
			t.Fatalf("Put %s: %v", name, err)
		}
	}

	for _, name := range agents {
		resp, err := http.Get(ts.URL + "/internal/agents/" + name + "/session-brief")
		if err != nil {
			t.Fatalf("GET %s: %v", name, err)
		}

		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			t.Errorf("%s: status got %d, want 200", name, resp.StatusCode)
			continue
		}

		var got briefstore.Brief
		if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
			resp.Body.Close()
			t.Fatalf("%s: decode: %v", name, err)
		}
		resp.Body.Close()

		wantActivity := name + "-activity"
		if got.LastActivity != wantActivity {
			t.Errorf("%s: LastActivity got %q, want %q", name, got.LastActivity, wantActivity)
		}
	}
}

// TestPreemptionNotice verifies that POST /internal/machines/{name}/preemption-notice
// invokes the PreemptionHandler with the correct machine name.
func TestPreemptionNotice(t *testing.T) {
	store := briefstore.NewMemoryStore()
	notified := make(chan string, 1)
	srv := api.NewInternalServer(store, api.WithPreemptionHandler(func(machineName, instanceId string) {
		notified <- machineName
	}))

	body := `{"timestamp":"2026-04-13T12:00:00Z","instanceId":"kyber-testvm"}`
	req := httptest.NewRequest("POST", "/internal/machines/test-machine/preemption-notice", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status: got %d, want %d", w.Code, http.StatusOK)
	}

	select {
	case name := <-notified:
		if name != "test-machine" {
			t.Errorf("machine name: got %q, want %q", name, "test-machine")
		}
	case <-time.After(time.Second):
		t.Fatal("preemption handler not called")
	}
}

// TestPreemptionNotice_MethodNotAllowed verifies that GET on the preemption-notice
// endpoint returns 405.
func TestPreemptionNotice_MethodNotAllowed(t *testing.T) {
	srv := api.NewInternalServer(briefstore.NewMemoryStore())
	req := httptest.NewRequest("GET", "/internal/machines/test/preemption-notice", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status: got %d, want %d", w.Code, http.StatusMethodNotAllowed)
	}
}

// newOAuthTestScheme builds a runtime.Scheme that includes corev1 types.
func newOAuthTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	return s
}

// TestInternalAPI_OAuthRotation_UpdatesSecret verifies that POST /internal/agents/{name}/refresh-token
// writes the new token to the Secret while leaving other keys (access_token) intact.
func TestInternalAPI_OAuthRotation_UpdatesSecret(t *testing.T) {
	scheme := newOAuthTestScheme(t)
	existing := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "alice-oauth", Namespace: "kyber-system"},
		Data: map[string][]byte{
			"access_token":  []byte("old-at"),
			"refresh_token": []byte("old-rt"),
		},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existing).Build()

	store := briefstore.NewMemoryStore()
	srv := api.NewInternalServer(store, api.WithKubeClient(fakeClient, "kyber-system"))
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/internal/agents/alice/refresh-token",
		strings.NewReader(`{"refresh_token":"new-rt"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status: got %d, want %d", resp.StatusCode, http.StatusNoContent)
	}

	got := &corev1.Secret{}
	if err := fakeClient.Get(context.Background(),
		types.NamespacedName{Name: "alice-oauth", Namespace: "kyber-system"}, got); err != nil {
		t.Fatalf("Get secret: %v", err)
	}
	if string(got.Data["refresh_token"]) != "new-rt" {
		t.Fatalf("refresh_token not updated: got %q", got.Data["refresh_token"])
	}
	if string(got.Data["access_token"]) != "old-at" {
		t.Fatalf("access_token should be unchanged: got %q", got.Data["access_token"])
	}
}

// TestInternalAPI_CodexAuthRotation_UpdatesSecret verifies that POST
// /internal/agents/{name}/codex-auth replaces auth.json in the agent's
// <name>-codex-auth Secret. This is what keeps a Codex agent alive across
// reboots: ChatGPT refresh tokens are single use, so a Secret that never
// learns about the rotated document holds a burnt token (kyber#681).
func TestInternalAPI_CodexAuthRotation_UpdatesSecret(t *testing.T) {
	scheme := newOAuthTestScheme(t)
	existing := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "alice-codex-auth", Namespace: "kyber-system"},
		Data:       map[string][]byte{"auth.json": []byte(`{"tokens":{"refresh_token":"burnt"}}`)},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existing).Build()

	store := briefstore.NewMemoryStore()
	srv := api.NewInternalServer(store, api.WithKubeClient(fakeClient, "kyber-system"))
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	rotated := `{"tokens":{"refresh_token":"rotated"}}`
	body, err := json.Marshal(map[string]string{"auth_json": rotated})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/internal/agents/alice/codex-auth",
		bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status: got %d, want %d", resp.StatusCode, http.StatusNoContent)
	}

	got := &corev1.Secret{}
	if err := fakeClient.Get(context.Background(),
		types.NamespacedName{Name: "alice-codex-auth", Namespace: "kyber-system"}, got); err != nil {
		t.Fatalf("Get secret: %v", err)
	}
	if string(got.Data["auth.json"]) != rotated {
		t.Fatalf("auth.json not updated: got %q, want %q", got.Data["auth.json"], rotated)
	}
}

// TestInternalAPI_CodexAuthRotation_RejectsBadBody pins the guards: the
// document must be present, valid JSON, and within the same ceiling the create
// path enforces. A corrupt push must never land in the Secret.
func TestInternalAPI_CodexAuthRotation_RejectsBadBody(t *testing.T) {
	scheme := newOAuthTestScheme(t)
	existing := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "alice-codex-auth", Namespace: "kyber-system"},
		Data:       map[string][]byte{"auth.json": []byte(`{"good":true}`)},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existing).Build()
	store := briefstore.NewMemoryStore()
	srv := api.NewInternalServer(store, api.WithKubeClient(fakeClient, "kyber-system"))

	cases := []struct {
		name string
		body string
	}{
		{"empty body", ""},
		{"missing field", `{}`},
		{"empty string", `{"auth_json":""}`},
		{"not json", `{"auth_json":"this is not json"}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/internal/agents/alice/codex-auth",
				strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, req)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status: got %d, want %d", w.Code, http.StatusBadRequest)
			}
		})
	}

	// The stored credential must be untouched by every rejected attempt.
	got := &corev1.Secret{}
	if err := fakeClient.Get(context.Background(),
		types.NamespacedName{Name: "alice-codex-auth", Namespace: "kyber-system"}, got); err != nil {
		t.Fatalf("Get secret: %v", err)
	}
	if string(got.Data["auth.json"]) != `{"good":true}` {
		t.Fatalf("secret was modified by a rejected request: got %q", got.Data["auth.json"])
	}
}

// TestInternalAPI_OAuthRotation_MissingBody verifies that POST with an empty
// or missing refresh_token returns 400.
func TestInternalAPI_OAuthRotation_MissingBody(t *testing.T) {
	scheme := newOAuthTestScheme(t)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	store := briefstore.NewMemoryStore()
	srv := api.NewInternalServer(store, api.WithKubeClient(fakeClient, "kyber-system"))

	cases := []struct {
		name string
		body string
	}{
		{"empty body", ""},
		{"missing field", `{}`},
		{"empty string", `{"refresh_token":""}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/internal/agents/alice/refresh-token",
				strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, req)
			if w.Code != http.StatusBadRequest {
				t.Errorf("status: got %d, want %d", w.Code, http.StatusBadRequest)
			}
		})
	}
}

// TestInternalAPI_OAuthRotation_NotConfigured verifies that a server without
// WithKubeClient returns 503 Service Unavailable.
func TestInternalAPI_OAuthRotation_NotConfigured(t *testing.T) {
	srv := api.NewInternalServer(briefstore.NewMemoryStore())
	req := httptest.NewRequest(http.MethodPost, "/internal/agents/alice/refresh-token",
		strings.NewReader(`{"refresh_token":"some-rt"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status: got %d, want %d", w.Code, http.StatusServiceUnavailable)
	}
}

// TestInternalAPI_OAuthRotation_MethodNotAllowed verifies that GET on the rotation endpoint returns 405.
func TestInternalAPI_OAuthRotation_MethodNotAllowed(t *testing.T) {
	scheme := newOAuthTestScheme(t)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	srv := api.NewInternalServer(briefstore.NewMemoryStore(), api.WithKubeClient(fakeClient, "kyber-system"))
	req := httptest.NewRequest(http.MethodGet, "/internal/agents/alice/refresh-token", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status: got %d, want %d", w.Code, http.StatusMethodNotAllowed)
	}
}

// TestInternalAPI_OAuthRotation_WritesFullCredentialSet verifies that when the
// rotation push includes access_token and expires_at, all three fields land in
// the Secret atomically.
func TestInternalAPI_OAuthRotation_WritesFullCredentialSet(t *testing.T) {
	scheme := newOAuthTestScheme(t)
	existing := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "alice-oauth", Namespace: "kyber-system"},
		Data: map[string][]byte{
			"access_token":  []byte("old-access"),
			"refresh_token": []byte("old-refresh"),
			"expires_at":    []byte("1000"),
		},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existing).Build()
	srv := api.NewInternalServer(briefstore.NewMemoryStore(), api.WithKubeClient(fakeClient, "kyber-system"))

	req := httptest.NewRequest(http.MethodPost, "/internal/agents/alice/refresh-token",
		strings.NewReader(`{"access_token":"new-access","refresh_token":"new-refresh","expires_at":2000}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("status: got %d, want %d", rr.Code, http.StatusNoContent)
	}

	updated := &corev1.Secret{}
	if err := fakeClient.Get(context.Background(),
		types.NamespacedName{Name: "alice-oauth", Namespace: "kyber-system"}, updated); err != nil {
		t.Fatalf("Get secret: %v", err)
	}
	if string(updated.Data["access_token"]) != "new-access" {
		t.Errorf("access_token: got %q, want %q", updated.Data["access_token"], "new-access")
	}
	if string(updated.Data["refresh_token"]) != "new-refresh" {
		t.Errorf("refresh_token: got %q, want %q", updated.Data["refresh_token"], "new-refresh")
	}
	if string(updated.Data["expires_at"]) != "2000" {
		t.Errorf("expires_at: got %q, want %q", updated.Data["expires_at"], "2000")
	}
}

// TestInternalServer_TokenUsagePost verifies that a well-formed Snapshot is
// persisted under the agent name when the server has a TokenStore configured.
func TestInternalServer_TokenUsagePost(t *testing.T) {
	store := tokenstore.NewMemoryStore()
	briefs := briefstore.NewMemoryStore()
	s := api.NewInternalServer(briefs, api.WithTokenStore(store))
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	snap := tokenreport.Snapshot{
		Model: "claude-sonnet-4-5",
		Tokens: tokenreport.Tokens{
			Used: 1000, Limit: 200000, Input: 500, CacheCreation: 300, CacheRead: 200,
		},
		Percentage:  0.5,
		EffortLevel: "medium",
		Speed:       "normal",
	}
	body, _ := json.Marshal(snap)
	resp, err := http.Post(srv.URL+"/internal/agents/alice/token-usage",
		"application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	got, err := store.Get(context.Background(), "alice")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got == nil || got.Tokens.Used != 1000 {
		t.Errorf("stored snapshot = %+v", got)
	}
}

// TestInternalServer_TokenUsagePost_DuplicatePost verifies that a second POST
// with the same cumulative counts as the first (d==0, newVal>0) contributes 0
// to the accumulator — no double-counting on idle heartbeat or duplicate delivery.
func TestInternalServer_TokenUsagePost_DuplicatePost(t *testing.T) {
	tokenStr := tokenstore.NewMemoryStore()
	acc := tokenstore.NewMemoryAccumulator()
	briefs := briefstore.NewMemoryStore()
	s := api.NewInternalServer(briefs,
		api.WithTokenStore(tokenStr),
		api.WithTokenAccumulator(acc),
	)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	snap := tokenreport.Snapshot{
		Model: "claude-sonnet-4-5",
		Tokens: tokenreport.Tokens{
			Used: 500, Limit: 200000, Input: 300, CacheCreation: 150, CacheRead: 50,
		},
	}
	body, _ := json.Marshal(snap)

	// First POST — no prev; delta equals snap values.
	resp, err := http.Post(srv.URL+"/internal/agents/alice/token-usage",
		"application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("first POST: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("first POST status=%d", resp.StatusCode)
	}

	// Second POST — identical snapshot: d==0, newVal>0 for all fields.
	// safeDelta must return 0; accumulator total must not increase.
	resp, err = http.Post(srv.URL+"/internal/agents/alice/token-usage",
		"application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("second POST: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("second POST status=%d", resp.StatusCode)
	}

	counts, err := acc.GetAll(context.Background(), "")
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	if len(counts) == 0 {
		t.Fatal("no accumulator counts recorded after first POST")
	}
	got := counts[0]
	if got.Input != 300 {
		t.Errorf("Input: got %d, want 300 (duplicate POST must not double-count)", got.Input)
	}
	if got.CacheCreation != 150 {
		t.Errorf("CacheCreation: got %d, want 150", got.CacheCreation)
	}
	if got.CacheRead != 50 {
		t.Errorf("CacheRead: got %d, want 50", got.CacheRead)
	}
}

// TestInternalServer_TokenUsagePost_WritesWindowedSeries verifies that a
// token-usage POST records the per-type delta to the windowed Redis token time
// series (kyber#428), so GET /metrics/tokens can honor start/end. Mirrors the
// accumulator write but asserts the new ts:token_usage:* series.
func TestInternalServer_TokenUsagePost_WritesWindowedSeries(t *testing.T) {
	tokenStr := tokenstore.NewMemoryStore()
	acc := tokenstore.NewMemoryAccumulator()
	ms := metricsstore.NewMemoryMetricsStore()
	briefs := briefstore.NewMemoryStore()
	s := api.NewInternalServer(briefs,
		api.WithTokenStore(tokenStr),
		api.WithTokenAccumulator(acc),
		api.WithMetricsStore(ms),
	)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	const model = "claude-sonnet-4-5"
	snap := tokenreport.Snapshot{
		Model: model,
		Tokens: tokenreport.Tokens{
			Used: 500, Limit: 200000, Input: 300, CacheCreation: 150, CacheRead: 50,
		},
	}
	body, _ := json.Marshal(snap)

	resp, err := http.Post(srv.URL+"/internal/agents/alice/token-usage",
		"application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200", resp.StatusCode)
	}

	ctx := context.Background()
	now := time.Now().Unix()
	// InternalServer namespace defaults to "" without WithKubeClient.
	want := map[string]float64{"input": 300, "cache_creation": 150, "cache_read": 50}
	for tt, exp := range want {
		key := metricsstore.TokenUsageKey("", "alice", model, tt)
		pts, err := ms.RangeQuery(ctx, key, now-3600, now+3600)
		if err != nil {
			t.Fatalf("RangeQuery %s: %v", tt, err)
		}
		var sum float64
		for _, p := range pts {
			sum += p.Value
		}
		if sum != exp {
			t.Errorf("series %q sum = %v, want %v", tt, sum, exp)
		}
	}
}

// TestInternalServer_TokenUsagePost_NotConfigured verifies that a server without
// WithTokenStore returns 503 Service Unavailable.
func TestInternalServer_TokenUsagePost_NotConfigured(t *testing.T) {
	briefs := briefstore.NewMemoryStore()
	s := api.NewInternalServer(briefs)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/internal/agents/alice/token-usage",
		"application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status=%d want 503", resp.StatusCode)
	}
}

// TestInternalAPI_OAuthRotation_PartialBody_RefreshOnly preserves backward compat:
// a body with only refresh_token (legacy bootstrap script) still works and only
// updates that field.
func TestInternalAPI_OAuthRotation_PartialBody_RefreshOnly(t *testing.T) {
	scheme := newOAuthTestScheme(t)
	existing := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "alice-oauth", Namespace: "kyber-system"},
		Data: map[string][]byte{
			"access_token":  []byte("keep-access"),
			"refresh_token": []byte("old-refresh"),
			"expires_at":    []byte("1000"),
		},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existing).Build()
	srv := api.NewInternalServer(briefstore.NewMemoryStore(), api.WithKubeClient(fakeClient, "kyber-system"))

	req := httptest.NewRequest(http.MethodPost, "/internal/agents/alice/refresh-token",
		strings.NewReader(`{"refresh_token":"new-refresh-only"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("status: got %d, want %d", rr.Code, http.StatusNoContent)
	}

	updated := &corev1.Secret{}
	if err := fakeClient.Get(context.Background(),
		types.NamespacedName{Name: "alice-oauth", Namespace: "kyber-system"}, updated); err != nil {
		t.Fatalf("Get secret: %v", err)
	}
	if string(updated.Data["access_token"]) != "keep-access" {
		t.Errorf("access_token must be preserved when not in body: got %q", updated.Data["access_token"])
	}
	if string(updated.Data["refresh_token"]) != "new-refresh-only" {
		t.Errorf("refresh_token: got %q, want %q", updated.Data["refresh_token"], "new-refresh-only")
	}
	if string(updated.Data["expires_at"]) != "1000" {
		t.Errorf("expires_at must be preserved when not in body: got %q", updated.Data["expires_at"])
	}
}

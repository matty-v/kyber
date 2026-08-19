package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/matty-v/kyber/pkg/api"
	"github.com/matty-v/kyber/pkg/contextwindowmap"
	"github.com/matty-v/kyber/pkg/messagebuffer"
	"github.com/matty-v/kyber/pkg/tokenreport"
	"github.com/matty-v/kyber/pkg/tokenstore"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// buildHandlerWithTokenStoreAndResolver injects both a TokenStore and a
// context-window Resolver backed by a fake kyber-model-context-windows
// ConfigMap (#396 serve-time resolution).
func buildHandlerWithTokenStoreAndResolver(t *testing.T, ts tokenstore.TokenStore, windows map[string]string, objs ...runtime.Object) http.Handler {
	t.Helper()
	scheme := mustNewScheme(t)
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "kyber-model-context-windows", Namespace: "kyber-system"},
		Data:       windows,
	}
	all := append([]runtime.Object{defaultMachine(), cm}, objs...)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(all...).Build()
	s := &api.Server{
		K8sClient:     fakeClient,
		MessageBuffer: messagebuffer.NewMemoryBuffer(),
		APIKey:        testAPIKey,
		Namespace:     "kyber-system",
		ValidRuntimes: map[string]bool{"claude-code": true},
		TokenStore:    ts,
		ContextWindows: &contextwindowmap.Resolver{
			Client:        fakeClient,
			Namespace:     "kyber-system",
			ConfigMapName: "kyber-model-context-windows",
		},
	}
	return s.BuildHandler()
}

// fakeSnapshotWindows is an in-memory stand-in for runtimedetect.SnapshotResolver
// (kyber#500): model id → auto-detected window, known=true on hit.
type fakeSnapshotWindows map[string]int64

func (f fakeSnapshotWindows) LookupWindow(_ context.Context, id string) (int64, bool) {
	if v, ok := f[id]; ok {
		return v, true
	}
	return 0, false
}

// buildHandlerWithSnapshot wires a TokenStore, a ConfigMap-backed
// ContextWindows resolver, AND a fake detection-snapshot resolver (kyber#500),
// so tests can assert the serve-time gauge resolves through the same
// override→snapshot→floor precedence as /available.
func buildHandlerWithSnapshot(t *testing.T, ts tokenstore.TokenStore, windows map[string]string, snapshot map[string]int64, objs ...runtime.Object) http.Handler {
	t.Helper()
	scheme := mustNewScheme(t)
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "kyber-model-context-windows", Namespace: "kyber-system"},
		Data:       windows,
	}
	all := append([]runtime.Object{defaultMachine(), cm}, objs...)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(all...).Build()
	snap := fakeSnapshotWindows{}
	for k, v := range snapshot {
		snap[k] = int64(v)
	}
	s := &api.Server{
		K8sClient:     fakeClient,
		MessageBuffer: messagebuffer.NewMemoryBuffer(),
		APIKey:        testAPIKey,
		Namespace:     "kyber-system",
		ValidRuntimes: map[string]bool{"claude-code": true},
		TokenStore:    ts,
		ContextWindows: &contextwindowmap.Resolver{
			Client:        fakeClient,
			Namespace:     "kyber-system",
			ConfigMapName: "kyber-model-context-windows",
		},
		Snapshots: snap,
	}
	return s.BuildHandler()
}

// kyber#500: the gauge must resolve an auto-detected model (present in the
// detection snapshot, ABSENT from the ConfigMap) to its real window — the bug
// was that the serve path only consulted the ConfigMap, so opus-4-8 floored at
// 200K/unverified while /available already showed 1M. Goes RED on the old
// LookupNormalized-only path.
func TestAgents_TokenUsage_Get_ResolvesViaSnapshot(t *testing.T) {
	ts := tokenstore.NewMemoryStore()
	_ = ts.Put(context.Background(), "r2-d2", &tokenreport.Snapshot{
		Model:  "claude-opus-4-8", // absent from knownModels AND the ConfigMap
		Tokens: tokenreport.Tokens{Used: 377_000, Limit: 0},
	})
	// ConfigMap has only opus-4-7; the snapshot carries opus-4-8 @ 1M.
	h := buildHandlerWithSnapshot(t, ts,
		map[string]string{"claude-opus-4-7": "1000000"},
		map[string]int64{"claude-opus-4-8": 1_000_000},
		sampleAgentCRD("r2-d2"))

	req := authedRequest(t, http.MethodGet, "/api/v1/agents/r2-d2/token-usage", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var got tokenreport.Snapshot
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Tokens.Limit != 1_000_000 {
		t.Errorf("Limit=%d want 1000000 (resolved from detection snapshot, not ConfigMap)", got.Tokens.Limit)
	}
	if !got.ContextWindowKnown {
		t.Errorf("ContextWindowKnown=false want true (auto-detected) — the #488 'unverified window' symptom")
	}
	if got.Percentage > 100 {
		t.Errorf("Percentage=%f want sub-100 (377K/1M≈37.7) — the original #488 >100%% gauge bug", got.Percentage)
	}
}

func TestAgents_TokenUsage_Get_UsesRuntimeReportedWindowWhenServerUnknown(t *testing.T) {
	ts := tokenstore.NewMemoryStore()
	_ = ts.Put(context.Background(), "r2-d2", &tokenreport.Snapshot{
		Model:              "gpt-5.6-sol",
		Tokens:             tokenreport.Tokens{Used: 25_840, Limit: 258_400},
		ContextWindowKnown: true,
	})
	h := buildHandlerWithSnapshot(t, ts, map[string]string{}, map[string]int64{}, sampleAgentCRD("r2-d2"))
	req := authedRequest(t, http.MethodGet, "/api/v1/agents/r2-d2/token-usage", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	var got tokenreport.Snapshot
	_ = json.NewDecoder(rr.Body).Decode(&got)
	if got.Tokens.Limit != 258_400 || !got.ContextWindowKnown || got.Percentage != 10 {
		t.Fatalf("runtime window not preserved: %+v", got)
	}
}

// Precedence: the operator override ConfigMap wins over the snapshot (matches
// #493 — explicit human intent on top).
func TestAgents_TokenUsage_Get_ConfigMapWinsOverSnapshot(t *testing.T) {
	ts := tokenstore.NewMemoryStore()
	_ = ts.Put(context.Background(), "r2-d2", &tokenreport.Snapshot{
		Model:  "claude-opus-4-8",
		Tokens: tokenreport.Tokens{Used: 100_000, Limit: 0},
	})
	h := buildHandlerWithSnapshot(t, ts,
		map[string]string{"claude-opus-4-8": "500000"}, // operator pinned 500K
		map[string]int64{"claude-opus-4-8": 1_000_000}, // snapshot says 1M
		sampleAgentCRD("r2-d2"))

	req := authedRequest(t, http.MethodGet, "/api/v1/agents/r2-d2/token-usage", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	var got tokenreport.Snapshot
	_ = json.NewDecoder(rr.Body).Decode(&got)
	if got.Tokens.Limit != 500_000 {
		t.Errorf("Limit=%d want 500000 (ConfigMap override beats snapshot)", got.Tokens.Limit)
	}
}

// Normalization preserved: a "[1m]"-suffixed concrete known model resolves via
// the in-Go table even with empty ConfigMap + empty snapshot. (Old
// LookupNormalized floored this — it never consulted knownModels.)
func TestAgents_TokenUsage_Get_NormalizationPreserved(t *testing.T) {
	ts := tokenstore.NewMemoryStore()
	_ = ts.Put(context.Background(), "r2-d2", &tokenreport.Snapshot{
		Model:  "claude-opus-4-7[1m]",
		Tokens: tokenreport.Tokens{Used: 100_000, Limit: 0},
	})
	h := buildHandlerWithSnapshot(t, ts,
		map[string]string{}, map[string]int64{"claude-opus-4-7": 1_000_000}, sampleAgentCRD("r2-d2"))

	req := authedRequest(t, http.MethodGet, "/api/v1/agents/r2-d2/token-usage", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	var got tokenreport.Snapshot
	_ = json.NewDecoder(rr.Body).Decode(&got)
	if got.Tokens.Limit != 1_000_000 || !got.ContextWindowKnown {
		t.Errorf("got (%d, known=%v) want (1000000, true) — [1m] strip + knownModels resolution", got.Tokens.Limit, got.ContextWindowKnown)
	}
}

// The embedded TokenUsage in the agents LIST payload (routes_agents.go) must
// resolve identically — Boba Fett flagged this second serve site.
func TestAgents_List_EmbeddedTokenUsage_ResolvesViaSnapshot(t *testing.T) {
	ts := tokenstore.NewMemoryStore()
	_ = ts.Put(context.Background(), "r2-d2", &tokenreport.Snapshot{
		Model:  "claude-opus-4-8",
		Tokens: tokenreport.Tokens{Used: 377_000, Limit: 0},
	})
	h := buildHandlerWithSnapshot(t, ts,
		map[string]string{}, map[string]int64{"claude-opus-4-8": 1_000_000},
		sampleAgentCRD("r2-d2"))

	req := authedRequest(t, http.MethodGet, "/api/v1/agents", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var list struct {
		Items []struct {
			ID         string                `json:"id"`
			TokenUsage *tokenreport.Snapshot `json:"tokenUsage"`
		} `json:"items"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var found bool
	for _, it := range list.Items {
		if it.ID == "r2-d2" {
			found = true
			if it.TokenUsage == nil {
				t.Fatalf("r2-d2 tokenUsage missing")
			}
			if it.TokenUsage.Tokens.Limit != 1_000_000 || !it.TokenUsage.ContextWindowKnown {
				t.Errorf("embedded TokenUsage = (%d, known=%v) want (1000000, true) — second serve site must agree", it.TokenUsage.Tokens.Limit, it.TokenUsage.ContextWindowKnown)
			}
		}
	}
	if !found {
		t.Fatalf("r2-d2 not in list response")
	}
}

// TestAgents_TokenUsage_Get_ResolvesLimitServerSide is the #396 round-trip /
// wire-contract pin: the reporter stores a raw snapshot (limit=0/pct=0
// sentinel); the GET resolves the limit from the ConfigMap at serve-time and
// the served response exposes used / limit / pct / contextWindowKnown. A 1M
// model with ~300K used reads ~30%, not ~150% against the old 200K floor.
func TestAgents_TokenUsage_Get_ResolvesLimitServerSide(t *testing.T) {
	ts := tokenstore.NewMemoryStore()
	// Reporter-shaped snapshot: raw used + model, limit/pct = 0 sentinel.
	_ = ts.Put(context.Background(), "dave", &tokenreport.Snapshot{
		Model:      "claude-opus-4-7",
		Tokens:     tokenreport.Tokens{Used: 300_000, Limit: 0},
		Percentage: 0,
	})
	h := buildHandlerWithTokenStoreAndResolver(t, ts,
		map[string]string{"claude-opus-4-7": "1000000"}, sampleAgentCRD("dave"))

	req := authedRequest(t, http.MethodGet, "/api/v1/agents/dave/token-usage", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var got tokenreport.Snapshot
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Tokens.Used != 300_000 {
		t.Errorf("Used=%d want 300000 (raw, preserved)", got.Tokens.Used)
	}
	if got.Tokens.Limit != 1_000_000 {
		t.Errorf("Limit=%d want 1000000 (server-resolved from ConfigMap)", got.Tokens.Limit)
	}
	if got.Percentage < 29.9 || got.Percentage > 30.1 {
		t.Errorf("Percentage=%f want ~30 (300K/1M), not ~150 against the old 200K floor", got.Percentage)
	}
	if !got.ContextWindowKnown {
		t.Errorf("ContextWindowKnown=false want true (model is in the ConfigMap)")
	}
}

// A model absent from every authoritative source fails loudly rather than
// receiving a guessed context-window floor.
func TestAgents_TokenUsage_Get_UnknownModelFails(t *testing.T) {
	ts := tokenstore.NewMemoryStore()
	_ = ts.Put(context.Background(), "dave", &tokenreport.Snapshot{
		Model:  "claude-mystery-9",
		Tokens: tokenreport.Tokens{Used: 50_000, Limit: 0},
	})
	h := buildHandlerWithTokenStoreAndResolver(t, ts,
		map[string]string{"claude-opus-4-7": "1000000"}, sampleAgentCRD("dave"))

	req := authedRequest(t, http.MethodGet, "/api/v1/agents/dave/token-usage", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d: %s", rr.Code, rr.Body.String())
	}
}

// buildHandlerWithTokenStore mirrors buildAgentHandler but injects a TokenStore.
func buildHandlerWithTokenStore(t *testing.T, ts tokenstore.TokenStore, objs ...runtime.Object) http.Handler {
	t.Helper()
	scheme := mustNewScheme(t)
	all := append([]runtime.Object{defaultMachine()}, objs...)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(all...).Build()
	s := &api.Server{
		K8sClient:     fakeClient,
		MessageBuffer: messagebuffer.NewMemoryBuffer(),
		APIKey:        testAPIKey,
		Namespace:     "kyber-system",
		ValidRuntimes: map[string]bool{"claude-code": true},
		TokenStore:    ts,
	}
	return s.BuildHandler()
}

func TestAgents_TokenUsage_Get(t *testing.T) {
	ts := tokenstore.NewMemoryStore()
	_ = ts.Put(context.Background(), "dave", &tokenreport.Snapshot{
		Model:              "claude-sonnet-4-5",
		Tokens:             tokenreport.Tokens{Used: 1234, Limit: 200000},
		ContextWindowKnown: true,
	})
	h := buildHandlerWithTokenStore(t, ts, sampleAgentCRD("dave"))

	req := authedRequest(t, http.MethodGet, "/api/v1/agents/dave/token-usage", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var got tokenreport.Snapshot
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Tokens.Used != 1234 {
		t.Errorf("Used=%d want 1234", got.Tokens.Used)
	}
}

func TestAgents_TokenUsage_Get_Missing(t *testing.T) {
	ts := tokenstore.NewMemoryStore()
	h := buildHandlerWithTokenStore(t, ts, sampleAgentCRD("dave"))

	req := authedRequest(t, http.MethodGet, "/api/v1/agents/dave/token-usage", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestAgents_TokenUsage_Get_NotConfigured(t *testing.T) {
	// No TokenStore — Server field is nil → 503
	h, _ := buildAgentHandler(t, sampleAgentCRD("dave"))
	req := authedRequest(t, http.MethodGet, "/api/v1/agents/dave/token-usage", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d: %s", rr.Code, rr.Body.String())
	}
}

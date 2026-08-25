package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/matty-v/kyber/pkg/adapters"
	"github.com/matty-v/kyber/pkg/runtimedetect"
)

type schedulerDemandConfigProvider struct {
	*adapters.FakeComputeAdapter
}

func (p *schedulerDemandConfigProvider) Capabilities(ctx context.Context) (adapters.Capabilities, error) {
	capabilities, err := p.FakeComputeAdapter.Capabilities(ctx)
	capabilities.RequiresSchedulerDemand = true
	return capabilities, err
}

func TestHandleConfig_ReturnsProvider(t *testing.T) {
	s := &Server{
		ComputeProvider: "mock",
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/config", nil)
	rr := httptest.NewRecorder()
	s.handleConfig(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var got ConfigResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Compute.Provider != "mock" {
		t.Errorf("provider = %q, want mock", got.Compute.Provider)
	}
}

func TestHandleConfig_ReturnsSchedulerDemandCapability(t *testing.T) {
	s := &Server{
		ComputeProvider:  "fake",
		CapacityProvider: &schedulerDemandConfigProvider{FakeComputeAdapter: adapters.NewFakeComputeAdapter()},
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/config", nil)
	rr := httptest.NewRecorder()
	s.handleConfig(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var got ConfigResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Compute.Managed == nil || got.Compute.Managed.Capabilities == nil {
		t.Fatalf("managed capabilities missing: %+v", got.Compute.Managed)
	}
	if !got.Compute.Managed.Capabilities.RequiresSchedulerDemand {
		t.Fatal("requiresSchedulerDemand = false, want true")
	}
	if got.Compute.Managed.Capabilities.ReliableFallbackMode != adapters.ReliableFallbackAutomatic {
		t.Fatalf("reliableFallbackMode = %q, want Automatic", got.Compute.Managed.Capabilities.ReliableFallbackMode)
	}
}

func TestHandleConfig_RejectsNonGet(t *testing.T) {
	s := &Server{ComputeProvider: "mock"}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/config", nil)
	rr := httptest.NewRecorder()
	s.handleConfig(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rr.Code)
	}
}

func TestHandleConfig_EmptyProvider(t *testing.T) {
	// When server has no provider configured (dev/test), the field is empty string.
	s := &Server{}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/config", nil)
	rr := httptest.NewRecorder()
	s.handleConfig(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var got ConfigResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &got)
	if got.Compute.Provider != "" {
		t.Errorf("provider = %q, want empty", got.Compute.Provider)
	}
}

// TestHandleConfig_GCEVMTypes_FromCatalog verifies that when the server is
// configured for GCE with a custom catalog, the response surfaces every
// catalog entry sorted alphabetically. This is what keeps the PWA's
// machine-type picker in lockstep with the validator on POST /machines.
func TestHandleConfig_GCEVMTypes_FromCatalog(t *testing.T) {
	catalog, err := ParseVMTypeCatalog(
		`[{"type":"e2-small","cpu":"2","memory":"2Gi"},` +
			`{"type":"n1-standard-2","cpu":"2","memory":"7500Mi"},` +
			`{"type":"e2-medium","cpu":"2","memory":"4Gi"}]`)
	if err != nil {
		t.Fatalf("ParseVMTypeCatalog: %v", err)
	}
	s := &Server{
		ComputeProvider:  "gce",
		GCEVMTypeCatalog: catalog,
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/config", nil)
	rr := httptest.NewRecorder()
	s.handleConfig(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var got ConfigResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	want := []ConfigVMType{
		{Type: "e2-medium", CPU: "2", Memory: "4Gi"},
		{Type: "e2-small", CPU: "2", Memory: "2Gi"},
		{Type: "n1-standard-2", CPU: "2", Memory: "7500Mi"},
	}
	if len(got.Compute.GCEVMTypes) != len(want) {
		t.Fatalf("GCEVMTypes len = %d, want %d; got=%v", len(got.Compute.GCEVMTypes), len(want), got.Compute.GCEVMTypes)
	}
	for i, w := range want {
		g := got.Compute.GCEVMTypes[i]
		if g != w {
			t.Errorf("GCEVMTypes[%d] = %+v, want %+v", i, g, w)
		}
	}
	if got.Compute.Managed == nil {
		t.Fatal("managed capabilities missing")
	}
	if len(got.Compute.Managed.Locations) == 0 || got.Compute.Managed.Locations[0] != "us-central1-a" {
		t.Errorf("managed locations = %v, want GCE locations", got.Compute.Managed.Locations)
	}
	if !got.Compute.Managed.SupportsInterruptible {
		t.Error("managed provider should advertise interruptible instances")
	}
}

// TestHandleConfig_GCEVMTypes_FallsBackToDefault verifies that when the
// catalog map is nil (operator did not set the env var), the response uses
// DefaultGCEVMTypeCatalog so the picker still has options.
func TestHandleConfig_GCEVMTypes_FallsBackToDefault(t *testing.T) {
	s := &Server{ComputeProvider: "gce"} // GCEVMTypeCatalog left nil
	req := httptest.NewRequest(http.MethodGet, "/api/v1/config", nil)
	rr := httptest.NewRecorder()
	s.handleConfig(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var got ConfigResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &got)
	if len(got.Compute.GCEVMTypes) == 0 {
		t.Fatal("expected default catalog in response, got empty")
	}
	// Spot-check: the default catalog includes e2-small per machinecaps.go.
	found := false
	for _, vt := range got.Compute.GCEVMTypes {
		if vt.Type == "e2-small" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("default catalog missing e2-small; got=%v", got.Compute.GCEVMTypes)
	}
}

// TestHandleConfig_GCEVMTypes_OmittedForMockProvider verifies the field is
// not surfaced when the provider is not "gce" — the picker only renders for
// gce so sending an irrelevant catalog list would be noise.
func TestHandleConfig_GCEVMTypes_OmittedForMockProvider(t *testing.T) {
	s := &Server{ComputeProvider: "mock"}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/config", nil)
	rr := httptest.NewRecorder()
	s.handleConfig(rr, req)

	var got ConfigResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &got)
	if len(got.Compute.GCEVMTypes) != 0 {
		t.Errorf("GCEVMTypes should be omitted for non-gce providers; got=%v", got.Compute.GCEVMTypes)
	}
	if got.Compute.Managed != nil {
		t.Errorf("managed capabilities should be omitted for existing-node provider; got=%+v", got.Compute.Managed)
	}
}

func TestHandleConfig_FakeUsesLocalLocation(t *testing.T) {
	s := &Server{ComputeProvider: "fake"}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/config", nil)
	rr := httptest.NewRecorder()
	s.handleConfig(rr, req)
	var got ConfigResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Compute.Managed == nil || len(got.Compute.Managed.Locations) != 1 || got.Compute.Managed.Locations[0] != "local-a" {
		t.Fatalf("fake managed locations = %+v, want [local-a]", got.Compute.Managed)
	}
}

// TestHandleConfig_PublicURL_Surfaced verifies that PublicURL is exposed
// to the PWA so it can render webhook URLs for inbound bindings (#208).
func TestHandleConfig_PublicURL_Surfaced(t *testing.T) {
	s := &Server{
		ComputeProvider: "mock",
		PublicURL:       "https://kyber-test.example",
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/config", nil)
	rr := httptest.NewRecorder()
	s.handleConfig(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var got ConfigResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.PublicURL != "https://kyber-test.example" {
		t.Errorf("PublicURL = %q, want %q", got.PublicURL, "https://kyber-test.example")
	}
}

// TestHandleConfig_PublicURL_OmittedWhenEmpty: empty PublicURL → field
// absent in the JSON response (PWA falls back to displaying just the path).
func TestHandleConfig_PublicURL_OmittedWhenEmpty(t *testing.T) {
	s := &Server{ComputeProvider: "mock"} // PublicURL left empty
	req := httptest.NewRequest(http.MethodGet, "/api/v1/config", nil)
	rr := httptest.NewRecorder()
	s.handleConfig(rr, req)
	body := rr.Body.String()
	if strings.Contains(body, "publicUrl") {
		t.Errorf("publicUrl should be omitted when empty, got %s", body)
	}
}

// TestHandleConfig_ModelsSourceOrdering pins the PR-D contract:
// detected models from the runtimedetect cache override the in-Go
// knownModels list when present; the knownModels fallback kicks in only
// when the cache is empty/absent. (kyber#378 AC: "tokenreport.Models()
// returns detected models + override-map entries when detection is
// healthy; falls back to knownModels only when both detection and
// override-map are empty").
func TestHandleConfig_ModelsSourceOrdering_DetectedWinsWhenCachePopulated(t *testing.T) {
	cache := runtimedetect.NewMemoryCache()
	if err := cache.Put(context.Background(), &runtimedetect.Snapshot{
		Models: []runtimedetect.Model{
			{ID: "claude-opus-4-99-future", DisplayName: "Future Opus", ContextWindow: 1_000_000, ContextWindowKnown: true},
		},
	}); err != nil {
		t.Fatalf("seeding cache: %v", err)
	}
	s := &Server{ComputeProvider: "mock", RuntimeDetectCache: cache}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/config", nil)
	rr := httptest.NewRecorder()
	s.handleConfig(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var got ConfigResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Models) != 1 || got.Models[0].ID != "claude-opus-4-99-future" {
		t.Fatalf("expected detected model to win, got %+v", got.Models)
	}
	if got.Models[0].ContextWindow != 1_000_000 {
		t.Errorf("detected contextWindow not surfaced: got %d", got.Models[0].ContextWindow)
	}
}

func TestHandleConfig_ModelsSourceOrdering_FallsBackToKnownModelsOnEmptyCache(t *testing.T) {
	cache := runtimedetect.NewMemoryCache() // empty
	s := &Server{ComputeProvider: "mock", RuntimeDetectCache: cache}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/config", nil)
	rr := httptest.NewRecorder()
	s.handleConfig(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var got ConfigResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// knownModels is the Go-side fallback; assert at least one well-known
	// ID appears so we're confident the fallback path fired.
	if len(got.Models) == 0 {
		t.Fatal("expected knownModels fallback to populate Models, got empty")
	}
	foundOpus := false
	for _, m := range got.Models {
		if m.ID == "claude-opus-4-7" {
			foundOpus = true
		}
	}
	if !foundOpus {
		t.Errorf("expected knownModels (claude-opus-4-7) in fallback list, got %+v", got.Models)
	}
}

func TestHandleConfig_ModelsSourceOrdering_FallsBackWhenCacheNil(t *testing.T) {
	s := &Server{ComputeProvider: "mock"} // RuntimeDetectCache: nil
	req := httptest.NewRequest(http.MethodGet, "/api/v1/config", nil)
	rr := httptest.NewRecorder()
	s.handleConfig(rr, req)
	var got ConfigResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &got)
	if len(got.Models) == 0 {
		t.Fatal("expected knownModels fallback when cache nil, got empty")
	}
}

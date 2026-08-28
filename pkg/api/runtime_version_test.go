package api_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/matty-v/kyber/pkg/api"
	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/briefstore"
)

func newRuntimeVersionScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := kyberv1.AddToScheme(s); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	return s
}

func newRuntimeVersionAgent(name string) *kyberv1.Agent {
	return &kyberv1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "kyber-system"},
	}
}

func TestInternalAPI_RuntimeVersion_PatchesStatus(t *testing.T) {
	scheme := newRuntimeVersionScheme(t)
	agent := newRuntimeVersionAgent("chewie")
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(agent).
		WithObjects(agent).
		Build()

	srv := api.NewInternalServer(briefstore.NewMemoryStore(),
		api.WithKubeClient(fakeClient, "kyber-system"))
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	body := `{"version":"2.1.119","reportedAt":"2026-04-24T02:15:00Z"}`
	resp, err := http.Post(ts.URL+"/internal/agents/chewie/runtime-version",
		"application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status: got %d, want 204", resp.StatusCode)
	}

	got := &kyberv1.Agent{}
	if err := fakeClient.Get(context.Background(),
		k8stypes.NamespacedName{Name: "chewie", Namespace: "kyber-system"}, got); err != nil {
		t.Fatalf("Get agent: %v", err)
	}
	if got.Status.Runtime.InstalledVersion != "2.1.119" {
		t.Errorf("InstalledVersion: got %q, want 2.1.119", got.Status.Runtime.InstalledVersion)
	}
	if got.Status.Runtime.InstalledAt == nil {
		t.Fatal("InstalledAt: got nil, want set")
	}
	if got.Status.Runtime.InstalledAt.UTC().Format("15:04:05") != "02:15:00" {
		t.Errorf("InstalledAt: got %v, want 02:15:00 UTC", got.Status.Runtime.InstalledAt)
	}
}

func TestInternalAPI_RuntimeVersion_OverwritesOnReport(t *testing.T) {
	scheme := newRuntimeVersionScheme(t)
	agent := newRuntimeVersionAgent("chewie")
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(agent).
		WithObjects(agent).
		Build()

	srv := api.NewInternalServer(briefstore.NewMemoryStore(),
		api.WithKubeClient(fakeClient, "kyber-system"))
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	post := func(version string) {
		t.Helper()
		body := `{"version":"` + version + `","reportedAt":"2026-04-24T02:15:00Z"}`
		resp, err := http.Post(ts.URL+"/internal/agents/chewie/runtime-version",
			"application/json", bytes.NewBufferString(body))
		if err != nil {
			t.Fatalf("POST %s: %v", version, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("POST %s: status %d", version, resp.StatusCode)
		}
	}

	post("2.1.118")
	post("2.1.119")

	got := &kyberv1.Agent{}
	if err := fakeClient.Get(context.Background(),
		k8stypes.NamespacedName{Name: "chewie", Namespace: "kyber-system"}, got); err != nil {
		t.Fatalf("Get agent: %v", err)
	}
	if got.Status.Runtime.InstalledVersion != "2.1.119" {
		t.Errorf("after two posts: got %q, want 2.1.119",
			got.Status.Runtime.InstalledVersion)
	}
}

func TestInternalAPI_RuntimeVersion_Validation(t *testing.T) {
	scheme := newRuntimeVersionScheme(t)
	agent := newRuntimeVersionAgent("chewie")
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(agent).
		WithObjects(agent).
		Build()

	srv := api.NewInternalServer(briefstore.NewMemoryStore(),
		api.WithKubeClient(fakeClient, "kyber-system"))
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	cases := []struct {
		name     string
		body     string
		wantCode int
	}{
		{"missing-version", `{"reportedAt":"2026-04-24T02:15:00Z"}`, http.StatusBadRequest},
		{"malformed-json", `{not-json`, http.StatusBadRequest},
		{"version-too-long", `{"version":"` + strings.Repeat("x", 129) + `"}`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := http.Post(ts.URL+"/internal/agents/chewie/runtime-version",
				"application/json", bytes.NewBufferString(tc.body))
			if err != nil {
				t.Fatalf("POST: %v", err)
			}
			resp.Body.Close()
			if resp.StatusCode != tc.wantCode {
				t.Errorf("status: got %d, want %d", resp.StatusCode, tc.wantCode)
			}
		})
	}
}

func TestInternalAPI_RuntimeVersion_UnknownAgent(t *testing.T) {
	scheme := newRuntimeVersionScheme(t)
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		Build()

	srv := api.NewInternalServer(briefstore.NewMemoryStore(),
		api.WithKubeClient(fakeClient, "kyber-system"))
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	body := `{"version":"2.1.119"}`
	resp, err := http.Post(ts.URL+"/internal/agents/ghost/runtime-version",
		"application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status: got %d, want 404", resp.StatusCode)
	}
}

// --- kyber#379 (PR-E) — dual-shape body handler ---------------------------
//
// The runtime-image roll is staggered: some pods will be on PR-E sidecars
// emitting the extended body while others are still on pre-PR-E sidecars
// emitting only {version, reportedAt}. The handler must accept both, or
// the partial-upgrade window would silently drop status reports from the
// un-upgraded half of the fleet. These tests pin the dual-shape contract.

func TestInternalAPI_RuntimeVersion_AcceptsExtendedBody(t *testing.T) {
	scheme := newRuntimeVersionScheme(t)
	agent := newRuntimeVersionAgent("chewie")
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(agent).WithObjects(agent).Build()
	srv := api.NewInternalServer(briefstore.NewMemoryStore(), api.WithKubeClient(c, "kyber-system"))
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	body := `{
		"version":"2.0.99",
		"reportedAt":"2026-05-29T22:00:00Z",
		"requestedVersion":"2.1.119",
		"requestedSatisfied":false,
		"modelSupported":false
	}`
	resp, err := http.Post(ts.URL+"/internal/agents/chewie/runtime-version",
		"application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status: got %d, want 204", resp.StatusCode)
	}

	got := &kyberv1.Agent{}
	if err := c.Get(context.Background(),
		k8stypes.NamespacedName{Name: "chewie", Namespace: "kyber-system"}, got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	rs := got.Status.Runtime
	if rs.InstalledVersion != "2.0.99" {
		t.Errorf("InstalledVersion = %q", rs.InstalledVersion)
	}
	if rs.RequestedVersion != "2.1.119" {
		t.Errorf("RequestedVersion = %q", rs.RequestedVersion)
	}
	if rs.RequestedSatisfied == nil || *rs.RequestedSatisfied {
		t.Errorf("RequestedSatisfied = %v, want *false", rs.RequestedSatisfied)
	}
	if rs.ModelSupported == nil || *rs.ModelSupported {
		t.Errorf("ModelSupported = %v, want *false", rs.ModelSupported)
	}
}

func TestInternalAPI_RuntimeVersion_RecordsBrokenHarnessDiagnostic(t *testing.T) {
	scheme := newRuntimeVersionScheme(t)
	agent := newRuntimeVersionAgent("chewie")
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(agent).WithObjects(agent).Build()
	srv := api.NewInternalServer(briefstore.NewMemoryStore(), api.WithKubeClient(c, "kyber-system"))
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	body := `{"version":"unknown","runtime":"codex","usable":false,"probeMessage":"codex: text file busy"}`
	resp, err := http.Post(ts.URL+"/internal/agents/chewie/runtime-version", "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	got := &kyberv1.Agent{}
	if err := c.Get(context.Background(), k8stypes.NamespacedName{Name: "chewie", Namespace: "kyber-system"}, got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Runtime.Usable == nil || *got.Status.Runtime.Usable || got.Status.Runtime.Runtime != "codex" {
		t.Fatalf("runtime status = %#v", got.Status.Runtime)
	}
	cond := meta.FindStatusCondition(got.Status.Conditions, kyberv1.AgentConditionRuntimeUnusable)
	if cond == nil || cond.Status != metav1.ConditionTrue || !strings.Contains(cond.Message, "text file busy") {
		t.Fatalf("runtime condition = %#v", cond)
	}
}

func TestInternalAPI_RuntimeVersion_OldShapeStillAccepted(t *testing.T) {
	scheme := newRuntimeVersionScheme(t)
	agent := newRuntimeVersionAgent("chewie")
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(agent).WithObjects(agent).Build()
	srv := api.NewInternalServer(briefstore.NewMemoryStore(), api.WithKubeClient(c, "kyber-system"))
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// Pre-PR-E sidecar body — only the two original fields. Must still 204
	// and write InstalledVersion without crashing or leaving PR-E fields
	// in a corrupted state.
	body := `{"version":"2.1.119","reportedAt":"2026-04-24T02:15:00Z"}`
	resp, err := http.Post(ts.URL+"/internal/agents/chewie/runtime-version",
		"application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status: got %d, want 204", resp.StatusCode)
	}
	got := &kyberv1.Agent{}
	if err := c.Get(context.Background(),
		k8stypes.NamespacedName{Name: "chewie", Namespace: "kyber-system"}, got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	rs := got.Status.Runtime
	if rs.InstalledVersion != "2.1.119" {
		t.Errorf("InstalledVersion = %q", rs.InstalledVersion)
	}
	if rs.RequestedVersion != "" {
		t.Errorf("RequestedVersion should be empty on old-shape body; got %q", rs.RequestedVersion)
	}
	if rs.RequestedSatisfied != nil {
		t.Errorf("RequestedSatisfied should be nil on old-shape body; got %v", rs.RequestedSatisfied)
	}
	if rs.ModelSupported != nil {
		t.Errorf("ModelSupported should be nil on old-shape body; got %v", rs.ModelSupported)
	}
}

// TestInternalAPI_RuntimeVersion_OldShapePreservesPRE_Fields pins the
// staggered-roll safety property: an old-shape body MUST NOT wipe
// previously-reported PR-E fields. If sidecar A (post-PR-E) reports
// modelSupported=true, then sidecar B (pre-PR-E) reports the old shape,
// modelSupported on the status must remain *true — not be reset to nil.
// Otherwise the PWA badge would flap during the rollout.
func TestInternalAPI_RuntimeVersion_OldShapePreservesPRE_Fields(t *testing.T) {
	scheme := newRuntimeVersionScheme(t)
	agent := newRuntimeVersionAgent("chewie")
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(agent).WithObjects(agent).Build()
	srv := api.NewInternalServer(briefstore.NewMemoryStore(), api.WithKubeClient(c, "kyber-system"))
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// Extended report from a post-PR-E sidecar.
	body := `{"version":"2.1.119","reportedAt":"2026-05-29T22:00:00Z",
		"requestedVersion":"2.1.119","requestedSatisfied":true,"modelSupported":true}`
	if r, err := http.Post(ts.URL+"/internal/agents/chewie/runtime-version",
		"application/json", bytes.NewBufferString(body)); err != nil {
		t.Fatal(err)
	} else {
		r.Body.Close()
	}
	// Subsequent old-shape report (simulates the staggered roll).
	old := `{"version":"2.1.119","reportedAt":"2026-05-29T22:05:00Z"}`
	if r, err := http.Post(ts.URL+"/internal/agents/chewie/runtime-version",
		"application/json", bytes.NewBufferString(old)); err != nil {
		t.Fatal(err)
	} else {
		r.Body.Close()
	}

	got := &kyberv1.Agent{}
	if err := c.Get(context.Background(),
		k8stypes.NamespacedName{Name: "chewie", Namespace: "kyber-system"}, got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	rs := got.Status.Runtime
	if rs.RequestedVersion != "2.1.119" {
		t.Errorf("RequestedVersion clobbered by old-shape body: %q", rs.RequestedVersion)
	}
	if rs.RequestedSatisfied == nil || !*rs.RequestedSatisfied {
		t.Errorf("RequestedSatisfied clobbered by old-shape body: %v", rs.RequestedSatisfied)
	}
	if rs.ModelSupported == nil || !*rs.ModelSupported {
		t.Errorf("ModelSupported clobbered by old-shape body: %v", rs.ModelSupported)
	}
}

// TestInternalAPI_RuntimeVersion_ExtendedBodyClearsOnExplicitFalse pins
// the inverse: when a post-PR-E sidecar explicitly reports
// modelSupported=false, the status must reflect that immediately
// (the badge must light up). False is operationally different from nil.
func TestInternalAPI_RuntimeVersion_ExtendedBodyClearsOnExplicitFalse(t *testing.T) {
	scheme := newRuntimeVersionScheme(t)
	agent := newRuntimeVersionAgent("chewie")
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(agent).WithObjects(agent).Build()
	srv := api.NewInternalServer(briefstore.NewMemoryStore(), api.WithKubeClient(c, "kyber-system"))
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	for _, body := range []string{
		`{"version":"2.1.119","modelSupported":true}`,
		`{"version":"2.1.119","modelSupported":false}`,
	} {
		if r, err := http.Post(ts.URL+"/internal/agents/chewie/runtime-version",
			"application/json", bytes.NewBufferString(body)); err != nil {
			t.Fatal(err)
		} else {
			r.Body.Close()
		}
	}

	got := &kyberv1.Agent{}
	if err := c.Get(context.Background(),
		k8stypes.NamespacedName{Name: "chewie", Namespace: "kyber-system"}, got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status.Runtime.ModelSupported == nil || *got.Status.Runtime.ModelSupported {
		t.Errorf("ModelSupported should be *false after explicit false; got %v", got.Status.Runtime.ModelSupported)
	}
}

func TestInternalAPI_RuntimeVersion_MethodNotAllowed(t *testing.T) {
	scheme := newRuntimeVersionScheme(t)
	agent := newRuntimeVersionAgent("chewie")
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(agent).
		WithObjects(agent).
		Build()

	srv := api.NewInternalServer(briefstore.NewMemoryStore(),
		api.WithKubeClient(fakeClient, "kyber-system"))
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/internal/agents/chewie/runtime-version")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status: got %d, want 405", resp.StatusCode)
	}
}

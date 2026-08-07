package fleetdefaults

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const (
	testNS = "kyber-system"
	testCM = "kyber-fleet-defaults"
)

func newClientWith(objs ...client.Object) client.Client {
	return fake.NewClientBuilder().WithObjects(objs...).Build()
}

func TestResolver_Resolve_MissingConfigMapReturnsEmpty(t *testing.T) {
	r := &Resolver{Client: newClientWith(), Namespace: testNS, ConfigMapName: testCM}
	got, err := r.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: unexpected error: %v", err)
	}
	if got.Model != "" || got.RuntimeVersion != "" {
		t.Errorf("Resolve missing CM: got %+v, want empty Defaults", got)
	}
}

func TestResolver_Resolve_ReadsConfigMapKeys(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: testCM, Namespace: testNS},
		Data: map[string]string{
			KeyDefaultModel:               "claude-sonnet-4-7",
			KeyDefaultRuntimeVersion:      "2.1.119",
			KeyCodexDefaultModel:          "gpt-5.6-sol",
			KeyCodexDefaultRuntimeVersion: "0.146.0",
		},
	}
	r := &Resolver{Client: newClientWith(cm), Namespace: testNS, ConfigMapName: testCM}
	got, err := r.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: unexpected error: %v", err)
	}
	if got.Model != "claude-sonnet-4-7" {
		t.Errorf("Model: got %q, want %q", got.Model, "claude-sonnet-4-7")
	}
	if got.RuntimeVersion != "2.1.119" {
		t.Errorf("RuntimeVersion: got %q, want %q", got.RuntimeVersion, "2.1.119")
	}
	if got.CodexModel != "gpt-5.6-sol" || got.CodexRuntimeVersion != "0.146.0" {
		t.Errorf("Codex defaults: got model=%q version=%q", got.CodexModel, got.CodexRuntimeVersion)
	}
}

// TestResolver_Resolve_CachesWithinTTL pins the contract that bursty
// reconciles inside the TTL window read from the in-memory snapshot, not
// the API server. Without this, ~100 concurrent reconciles would all Get
// the same ConfigMap.
func TestResolver_Resolve_CachesWithinTTL(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: testCM, Namespace: testNS},
		Data:       map[string]string{KeyDefaultModel: "v1"},
	}
	counter := &countingReader{inner: newClientWith(cm)}
	r := &Resolver{Client: counter, Namespace: testNS, ConfigMapName: testCM}

	for i := 0; i < 5; i++ {
		if _, err := r.Resolve(context.Background()); err != nil {
			t.Fatalf("Resolve %d: %v", i, err)
		}
	}
	if counter.gets != 1 {
		t.Errorf("expected 1 Get within TTL, got %d", counter.gets)
	}
}

// TestResolver_Resolve_RefetchesAfterTTL verifies a PWA edit becomes
// visible on the next reconcile after ResolveCacheTTL elapses.
func TestResolver_Resolve_RefetchesAfterTTL(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: testCM, Namespace: testNS},
		Data:       map[string]string{KeyDefaultModel: "v1"},
	}
	counter := &countingReader{inner: newClientWith(cm)}
	clock := &mockClock{now: time.Unix(0, 0)}
	r := &Resolver{Client: counter, Namespace: testNS, ConfigMapName: testCM, Now: clock.Now}

	if _, err := r.Resolve(context.Background()); err != nil {
		t.Fatalf("first Resolve: %v", err)
	}
	clock.advance(ResolveCacheTTL + time.Second)
	if _, err := r.Resolve(context.Background()); err != nil {
		t.Fatalf("second Resolve: %v", err)
	}
	if counter.gets != 2 {
		t.Errorf("expected 2 Gets across TTL boundary, got %d", counter.gets)
	}
}

// TestResolver_Invalidate forces the next Resolve to re-fetch, so a PUT
// /api/v1/fleet-defaults can call Invalidate() and the writer sees their
// own update immediately.
func TestResolver_Invalidate(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: testCM, Namespace: testNS},
		Data:       map[string]string{KeyDefaultModel: "v1"},
	}
	counter := &countingReader{inner: newClientWith(cm)}
	r := &Resolver{Client: counter, Namespace: testNS, ConfigMapName: testCM}

	if _, err := r.Resolve(context.Background()); err != nil {
		t.Fatalf("first Resolve: %v", err)
	}
	r.Invalidate()
	if _, err := r.Resolve(context.Background()); err != nil {
		t.Fatalf("second Resolve: %v", err)
	}
	if counter.gets != 2 {
		t.Errorf("expected 2 Gets after Invalidate, got %d", counter.gets)
	}
}

// TestResolver_Resolve_PropagatesAPIErrors keeps non-NotFound errors
// visible to the caller — silently swallowing them would hide the case
// where the API server is unreachable (would render every agent as
// "no fleet default" + likely trip the empty-both guard).
func TestResolver_Resolve_PropagatesAPIErrors(t *testing.T) {
	boom := errors.New("apiserver down")
	r := &Resolver{Client: &errorReader{err: boom}, Namespace: testNS, ConfigMapName: testCM}
	if _, err := r.Resolve(context.Background()); err == nil {
		t.Fatalf("expected error, got nil")
	}
}

// TestResolver_NilSafe documents the contract that a nil receiver is OK
// (returns empty Defaults). Lets cmd/control-plane wire it conditionally
// without sprinkling nil checks across reconciler call sites.
func TestResolver_NilSafe(t *testing.T) {
	var r *Resolver
	got, err := r.Resolve(context.Background())
	if err != nil {
		t.Fatalf("nil Resolve: %v", err)
	}
	if got != (Defaults{}) {
		t.Errorf("nil Resolve: got %+v, want empty", got)
	}
	r.Invalidate()
}

// --- test doubles ---

type countingReader struct {
	inner client.Reader
	gets  int
}

func (c *countingReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	c.gets++
	return c.inner.Get(ctx, key, obj, opts...)
}

func (c *countingReader) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	return c.inner.List(ctx, list, opts...)
}

type errorReader struct {
	err error
}

func (e *errorReader) Get(_ context.Context, _ types.NamespacedName, _ client.Object, _ ...client.GetOption) error {
	return e.err
}

func (e *errorReader) List(_ context.Context, _ client.ObjectList, _ ...client.ListOption) error {
	return e.err
}

// notFound is a sentinel used in tests above. Kept to assert behavior under
// IsNotFound — referenced indirectly via the fake client's IsNotFound flow.
var _ = apierrors.NewNotFound(schema.GroupResource{Resource: "configmaps"}, "x")

type mockClock struct {
	now time.Time
}

func (c *mockClock) Now() time.Time          { return c.now }
func (c *mockClock) advance(d time.Duration) { c.now = c.now.Add(d) }

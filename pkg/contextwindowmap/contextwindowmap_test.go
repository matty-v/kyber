package contextwindowmap

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const (
	testNS = "kyber-system"
	testCM = "kyber-model-context-windows"
)

func newClientWith(objs ...client.Object) client.Client {
	return fake.NewClientBuilder().WithObjects(objs...).Build()
}

func TestLookupOr_NilResolver_ReturnsFloor(t *testing.T) {
	var r *Resolver
	cw, known := r.LookupOr(context.Background(), "claude-opus-4-7")
	if cw != DefaultContextWindowFloor || known {
		t.Fatalf("nil resolver: got (%d, %v), want (%d, false)", cw, known, DefaultContextWindowFloor)
	}
}

func TestLookupOr_UnmappedModel_ReturnsFloor(t *testing.T) {
	r := &Resolver{Client: newClientWith(), Namespace: testNS, ConfigMapName: testCM}
	cw, known := r.LookupOr(context.Background(), "claude-mystery-9")
	if cw != DefaultContextWindowFloor || known {
		t.Fatalf("unmapped: got (%d, %v), want (%d, false)", cw, known, DefaultContextWindowFloor)
	}
}

func TestLookupOr_MappedModel_ReturnsKnownValue(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: testCM, Namespace: testNS},
		Data: map[string]string{
			"claude-opus-4-7":   "1000000",
			"claude-sonnet-4-6": "1000000",
			"claude-haiku-4-5":  "200000",
		},
	}
	r := &Resolver{Client: newClientWith(cm), Namespace: testNS, ConfigMapName: testCM}
	cw, known := r.LookupOr(context.Background(), "claude-opus-4-7")
	if cw != 1_000_000 || !known {
		t.Fatalf("opus-4-7: got (%d, %v), want (1000000, true)", cw, known)
	}
	cw2, known2 := r.LookupOr(context.Background(), "claude-sonnet-4-6")
	if cw2 != 1_000_000 || !known2 {
		t.Fatalf("sonnet-4-6: got (%d, %v), want (1000000, true)", cw2, known2)
	}
}

// Operator typo on a single row must NOT blank the rest of the map.
func TestLookupOr_InvalidEntryDroppedSilently(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: testCM, Namespace: testNS},
		Data: map[string]string{
			"claude-opus-4-7": "1000000",
			"claude-bad":      "not-a-number",
			"claude-zero":     "0",
			"claude-neg":      "-100",
		},
	}
	r := &Resolver{Client: newClientWith(cm), Namespace: testNS, ConfigMapName: testCM}
	cw, known := r.LookupOr(context.Background(), "claude-opus-4-7")
	if cw != 1_000_000 || !known {
		t.Fatalf("valid entry survived: got (%d, %v)", cw, known)
	}
	for _, bad := range []string{"claude-bad", "claude-zero", "claude-neg"} {
		cwb, knownb := r.LookupOr(context.Background(), bad)
		if knownb {
			t.Errorf("expected %q to be dropped (not Known); got (%d, %v)", bad, cwb, knownb)
		}
	}
}

func TestLookupOr_EmptyConfigMapName_ReturnsFloor(t *testing.T) {
	// Dev/test mode: no ConfigMap configured. Resolver must not crash on
	// the Get call and must return the floor for every model.
	r := &Resolver{Client: newClientWith(), Namespace: testNS, ConfigMapName: ""}
	cw, known := r.LookupOr(context.Background(), "claude-opus-4-7")
	if cw != DefaultContextWindowFloor || known {
		t.Fatalf("empty CM name: got (%d, %v), want (%d, false)", cw, known, DefaultContextWindowFloor)
	}
}

func TestAll_ReturnsCopy(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: testCM, Namespace: testNS},
		Data:       map[string]string{"claude-opus-4-7": "1000000"},
	}
	r := &Resolver{Client: newClientWith(cm), Namespace: testNS, ConfigMapName: testCM}
	got, err := r.All(context.Background())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	// Mutate caller's map; subsequent All() must return the cached value
	// untouched (proving the resolver returned a copy).
	got["claude-opus-4-7"] = 42
	got2, _ := r.All(context.Background())
	if got2["claude-opus-4-7"] != 1_000_000 {
		t.Errorf("cache corrupted via returned map: %v", got2)
	}
}

func TestCacheTTL_ExpiresAndRefreshes(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: testCM, Namespace: testNS},
		Data:       map[string]string{"claude-opus-4-7": "500000"},
	}
	clk := time.Date(2026, 5, 29, 0, 0, 0, 0, time.UTC)
	r := &Resolver{
		Client: newClientWith(cm), Namespace: testNS, ConfigMapName: testCM,
		Now: func() time.Time { return clk },
	}
	if cw, _ := r.LookupOr(context.Background(), "claude-opus-4-7"); cw != 500_000 {
		t.Fatalf("first read: got %d", cw)
	}
	// Mutate the CM out from under the resolver. Within the TTL window the
	// resolver still returns the cached value.
	cm.Data["claude-opus-4-7"] = "1000000"
	r.Client = newClientWith(cm)
	clk = clk.Add(ResolveCacheTTL - 1*time.Second)
	if cw, _ := r.LookupOr(context.Background(), "claude-opus-4-7"); cw != 500_000 {
		t.Errorf("inside TTL: got %d, want cached 500000", cw)
	}
	// Past the TTL: next read picks up the new value.
	clk = clk.Add(2 * time.Second)
	if cw, _ := r.LookupOr(context.Background(), "claude-opus-4-7"); cw != 1_000_000 {
		t.Errorf("after TTL: got %d, want refreshed 1000000", cw)
	}
}

func TestInvalidate_ForcesRefreshOnNextRead(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: testCM, Namespace: testNS},
		Data:       map[string]string{"claude-opus-4-7": "500000"},
	}
	r := &Resolver{Client: newClientWith(cm), Namespace: testNS, ConfigMapName: testCM}
	if cw, _ := r.LookupOr(context.Background(), "claude-opus-4-7"); cw != 500_000 {
		t.Fatalf("first read: got %d", cw)
	}
	cm.Data["claude-opus-4-7"] = "1000000"
	r.Client = newClientWith(cm)
	r.Invalidate()
	if cw, _ := r.LookupOr(context.Background(), "claude-opus-4-7"); cw != 1_000_000 {
		t.Errorf("after Invalidate: got %d, want refreshed 1000000", cw)
	}
}

// TestLookupNormalized covers the #396 server-side normalization that the
// in-pod LimitFor used to do: strip the "[1m]" opt-in suffix before the
// ConfigMap lookup, fall back to the built-in family aliases, then the floor.
func TestLookupNormalized(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: testCM, Namespace: testNS},
		Data: map[string]string{
			"claude-opus-4-7":   "1000000",
			"claude-sonnet-4-5": "200000",
		},
	}
	r := &Resolver{Client: newClientWith(cm), Namespace: testNS, ConfigMapName: testCM}
	ctx := context.Background()

	cases := []struct {
		name      string
		model     string
		wantCW    int64
		wantKnown bool
	}{
		{"concrete in ConfigMap", "claude-opus-4-7", 1_000_000, true},
		{"[1m] suffix strips to same window", "claude-opus-4-7[1m]", 1_000_000, true},
		{"concrete 200k in ConfigMap", "claude-sonnet-4-5", 200_000, true},
		{"opus alias → built-in 1M", "opus", 1_000_000, true},
		{"sonnet alias → built-in 200K", "sonnet", 200_000, true},
		{"haiku alias → built-in 200K", "haiku", 200_000, true},
		{"unknown model → floor, not known", "claude-mystery-9", DefaultContextWindowFloor, false},
		{"empty model → floor, not known", "", DefaultContextWindowFloor, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cw, known := r.LookupNormalized(ctx, tc.model)
			if cw != tc.wantCW || known != tc.wantKnown {
				t.Errorf("LookupNormalized(%q) = (%d, %v), want (%d, %v)",
					tc.model, cw, known, tc.wantCW, tc.wantKnown)
			}
		})
	}
}

// A nil resolver must still normalize aliases (no ConfigMap needed) and floor
// the rest — handleTokenUsageGet guards nil but this keeps the method safe.
func TestLookupNormalized_NilResolver(t *testing.T) {
	var r *Resolver
	if cw, known := r.LookupNormalized(context.Background(), "opus"); cw != 1_000_000 || !known {
		t.Errorf("nil resolver opus: got (%d, %v), want (1000000, true)", cw, known)
	}
	if cw, known := r.LookupNormalized(context.Background(), "claude-opus-4-7"); cw != DefaultContextWindowFloor || known {
		t.Errorf("nil resolver concrete: got (%d, %v), want (%d, false)", cw, known, DefaultContextWindowFloor)
	}
}

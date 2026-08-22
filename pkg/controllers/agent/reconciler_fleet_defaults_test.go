package agent

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/fleetdefaults"
)

// These tests pin the kyber#376 / PR-B contract: the agent reconciler
// resolves spec.model against the kyber-fleet-defaults ConfigMap before
// building the pod, and surfaces a clear status condition when neither
// is set. They use a fake client (not envtest) — the resolution logic
// is pure-Go and doesn't need a real API server.

const (
	rfdNS        = "kyber-system"
	rfdAgentName = "han"
	rfdConfigMap = "kyber-fleet-defaults"
)

func newResolverScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("client-go AddToScheme: %v", err)
	}
	if err := kyberv1.AddToScheme(s); err != nil {
		t.Fatalf("kyberv1 AddToScheme: %v", err)
	}
	return s
}

func newResolverAgent(model string) *kyberv1.Agent {
	return &kyberv1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: rfdAgentName, Namespace: rfdNS},
		Spec: kyberv1.AgentSpec{
			Machine: "node-01",
			Runtime: "claude-code",
			Model:   model,
			Resources: kyberv1.AgentResources{
				CPU:    resource.MustParse("100m"),
				Memory: resource.MustParse("256Mi"),
				Disk:   resource.MustParse("1Gi"),
			},
			Secrets: kyberv1.AgentSecrets{AuthType: kyberv1.AgentAuthTypeOAuth},
		},
	}
}

func newResolverReconciler(t *testing.T, agent *kyberv1.Agent, cm *corev1.ConfigMap) (*AgentReconciler, client.Client) {
	t.Helper()
	scheme := newResolverScheme(t)
	objs := []client.Object{agent}
	if cm != nil {
		objs = append(objs, cm)
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&kyberv1.Agent{}).
		Build()
	r := &AgentReconciler{
		Client:   c,
		Scheme:   scheme,
		Recorder: record.NewFakeRecorder(10),
		FleetDefaults: &fleetdefaults.Resolver{
			Client:        c,
			Namespace:     rfdNS,
			ConfigMapName: rfdConfigMap,
		},
	}
	return r, c
}

// TestResolveAgentForPod_SpecModelWins is the regression guard: when
// spec.model is set, the fleet default is ignored. This is the existing
// behavior — every test in the suite predating kyber#376 relies on it.
func TestResolveAgentForPod_SpecModelWins(t *testing.T) {
	agent := newResolverAgent("claude-opus-4-7")
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: rfdConfigMap, Namespace: rfdNS},
		Data:       map[string]string{fleetdefaults.KeyDefaultModel: "claude-haiku-4-5"},
	}
	r, _ := newResolverReconciler(t, agent, cm)

	resolved, err := r.resolveAgentForPod(context.Background(), agent)
	if err != nil {
		t.Fatalf("resolveAgentForPod: %v", err)
	}
	if resolved.Spec.Model != "claude-opus-4-7" {
		t.Errorf("Spec.Model: got %q, want %q (spec must win over default)", resolved.Spec.Model, "claude-opus-4-7")
	}
	// And the original agent isn't mutated (it's the caller's view of the K8s object).
	if agent.Spec.Model != "claude-opus-4-7" {
		t.Errorf("original Spec.Model mutated: %q", agent.Spec.Model)
	}
}

// TestResolveAgentForPod_EmptySpecUsesDefault is the new behavior:
// spec.model empty + defaultModel populated → resolved Spec.Model carries
// the fleet default into the pod build.
func TestResolveAgentForPod_EmptySpecUsesDefault(t *testing.T) {
	agent := newResolverAgent("")
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: rfdConfigMap, Namespace: rfdNS},
		Data:       map[string]string{fleetdefaults.KeyDefaultModel: "claude-sonnet-4-7"},
	}
	r, _ := newResolverReconciler(t, agent, cm)

	resolved, err := r.resolveAgentForPod(context.Background(), agent)
	if err != nil {
		t.Fatalf("resolveAgentForPod: %v", err)
	}
	if resolved.Spec.Model != "claude-sonnet-4-7" {
		t.Errorf("resolved Spec.Model: got %q, want %q", resolved.Spec.Model, "claude-sonnet-4-7")
	}
	// Mutation must be on the copy only.
	if agent.Spec.Model != "" {
		t.Errorf("original Spec.Model mutated to %q (should remain empty)", agent.Spec.Model)
	}
}

// Empty model values deliberately delegate selection to the runtime.
func TestResolveAgentForPod_EmptyBothUsesRuntimeDefault(t *testing.T) {
	agent := newResolverAgent("")
	r, c := newResolverReconciler(t, agent, nil)

	resolved, err := r.resolveAgentForPod(context.Background(), agent)
	if err != nil {
		t.Fatalf("resolveAgentForPod: %v", err)
	}
	if resolved.Spec.Model != "" {
		t.Fatalf("resolved model = %q, want empty runtime default", resolved.Spec.Model)
	}

	fresh := &kyberv1.Agent{}
	if getErr := c.Get(context.Background(), types.NamespacedName{Namespace: rfdNS, Name: rfdAgentName}, fresh); getErr != nil {
		t.Fatalf("get: %v", getErr)
	}
	if cond := meta.FindStatusCondition(fresh.Status.Conditions, kyberv1.AgentConditionModelUnresolved); cond != nil {
		t.Fatalf("unexpected ModelUnresolved condition: %+v", cond)
	}
}

// TestResolveAgentForPod_ClearsConditionOnceResolved ensures a previously-
// blocked agent that gets a model (via PWA edit or spec patch) sheds its
// ModelUnresolved condition cleanly on the next resolve — otherwise the
// PWA would render a perpetual stale warning.
func TestResolveAgentForPod_ClearsConditionOnceResolved(t *testing.T) {
	agent := newResolverAgent("claude-sonnet-4")
	// Seed the condition as if a previous reconcile had set it.
	meta.SetStatusCondition(&agent.Status.Conditions, metav1.Condition{
		Type:    kyberv1.AgentConditionModelUnresolved,
		Status:  metav1.ConditionTrue,
		Reason:  "NoModelConfigured",
		Message: "stale",
	})
	r, c := newResolverReconciler(t, agent, nil)

	if _, err := r.resolveAgentForPod(context.Background(), agent); err != nil {
		t.Fatalf("resolveAgentForPod: %v", err)
	}

	fresh := &kyberv1.Agent{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: rfdNS, Name: rfdAgentName}, fresh); err != nil {
		t.Fatalf("get: %v", err)
	}
	if cond := meta.FindStatusCondition(fresh.Status.Conditions, kyberv1.AgentConditionModelUnresolved); cond != nil {
		t.Errorf("condition still present after resolution: %+v", cond)
	}
}

// TestResolveAgentForPod_ResolverErrorPropagates ensures a transient
// ConfigMap read failure is reported (so the reconciler retries on the
// next pass) rather than masquerading as "no default configured".
func TestResolveAgentForPod_ResolverErrorPropagates(t *testing.T) {
	agent := newResolverAgent("")
	scheme := newResolverScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(agent).Build()
	r := &AgentReconciler{
		Client:   c,
		Scheme:   scheme,
		Recorder: record.NewFakeRecorder(10),
		FleetDefaults: &fleetdefaults.Resolver{
			Client:        &errReader{err: errors.New("boom")},
			Namespace:     rfdNS,
			ConfigMapName: rfdConfigMap,
		},
	}
	if _, err := r.resolveAgentForPod(context.Background(), agent); err == nil {
		t.Fatal("expected resolver error to surface")
	}
}

// TestResolveAgentForPod_RuntimeVersion_SpecWins pins the kyber#377
// contract: when spec.runtimeVersion is set, the fleet default is
// ignored. Mirrors the spec.model wins behavior so per-agent overrides
// are honored.
func TestResolveAgentForPod_RuntimeVersion_SpecWins(t *testing.T) {
	agent := newResolverAgent("claude-sonnet-4")
	agent.Spec.RuntimeVersion = "2.1.119"
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: rfdConfigMap, Namespace: rfdNS},
		Data: map[string]string{
			fleetdefaults.KeyDefaultRuntimeVersion: "2.0.99",
		},
	}
	r, _ := newResolverReconciler(t, agent, cm)

	resolved, err := r.resolveAgentForPod(context.Background(), agent)
	if err != nil {
		t.Fatalf("resolveAgentForPod: %v", err)
	}
	if resolved.Spec.RuntimeVersion != "2.1.119" {
		t.Errorf("RuntimeVersion: got %q, want %q (spec must win over default)", resolved.Spec.RuntimeVersion, "2.1.119")
	}
}

// TestResolveAgentForPod_RuntimeVersion_EmptySpecUsesDefault verifies
// the fallback path: spec empty + defaultRuntimeVersion populated →
// resolved Spec.RuntimeVersion carries the fleet default. PR-C consumes
// this in start-claude.sh's install branch.
func TestResolveAgentForPod_RuntimeVersion_EmptySpecUsesDefault(t *testing.T) {
	agent := newResolverAgent("claude-sonnet-4")
	agent.Spec.Runtime = "claude-code"
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: rfdConfigMap, Namespace: rfdNS},
		Data: map[string]string{
			fleetdefaults.KeyDefaultRuntimeVersion: "2.1.119",
		},
	}
	r, _ := newResolverReconciler(t, agent, cm)

	resolved, err := r.resolveAgentForPod(context.Background(), agent)
	if err != nil {
		t.Fatalf("resolveAgentForPod: %v", err)
	}
	if resolved.Spec.RuntimeVersion != "2.1.119" {
		t.Errorf("RuntimeVersion: got %q, want %q (default should fill in)", resolved.Spec.RuntimeVersion, "2.1.119")
	}
}

func TestResolveAgentForPod_RuntimeVersion_ClaudeDefaultDoesNotLeakIntoCodex(t *testing.T) {
	agent := newResolverAgent("gpt-5.6-sol")
	agent.Spec.Runtime = "codex"
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: rfdConfigMap, Namespace: rfdNS},
		Data:       map[string]string{fleetdefaults.KeyDefaultRuntimeVersion: "2.1.119"},
	}
	r, _ := newResolverReconciler(t, agent, cm)
	resolved, err := r.resolveAgentForPod(context.Background(), agent)
	if err != nil {
		t.Fatalf("resolveAgentForPod: %v", err)
	}
	if resolved.Spec.RuntimeVersion != "" {
		t.Fatalf("Codex RuntimeVersion = %q, want empty", resolved.Spec.RuntimeVersion)
	}
}

func TestResolveAgentForPod_CodexUsesCodexDefaults(t *testing.T) {
	agent := newResolverAgent("")
	agent.Spec.Runtime = "codex"
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: rfdConfigMap, Namespace: rfdNS},
		Data: map[string]string{
			fleetdefaults.KeyDefaultModel:               "claude-sonnet-4-7",
			fleetdefaults.KeyDefaultRuntimeVersion:      "2.1.119",
			fleetdefaults.KeyCodexDefaultModel:          "gpt-5.6-sol",
			fleetdefaults.KeyCodexDefaultRuntimeVersion: "0.146.0",
		},
	}
	r, _ := newResolverReconciler(t, agent, cm)
	resolved, err := r.resolveAgentForPod(context.Background(), agent)
	if err != nil {
		t.Fatalf("resolveAgentForPod: %v", err)
	}
	if resolved.Spec.Model != "gpt-5.6-sol" || resolved.Spec.RuntimeVersion != "0.146.0" {
		t.Fatalf("Codex resolved model=%q version=%q", resolved.Spec.Model, resolved.Spec.RuntimeVersion)
	}
}

func TestResolveAgentForPod_ClaudeIgnoresCodexDefaults(t *testing.T) {
	agent := newResolverAgent("")
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: rfdConfigMap, Namespace: rfdNS},
		Data: map[string]string{
			fleetdefaults.KeyDefaultModel:               "claude-sonnet-4-7",
			fleetdefaults.KeyDefaultRuntimeVersion:      "2.1.119",
			fleetdefaults.KeyCodexDefaultModel:          "gpt-5.6-sol",
			fleetdefaults.KeyCodexDefaultRuntimeVersion: "0.146.0",
		},
	}
	r, _ := newResolverReconciler(t, agent, cm)
	resolved, err := r.resolveAgentForPod(context.Background(), agent)
	if err != nil {
		t.Fatalf("resolveAgentForPod: %v", err)
	}
	if resolved.Spec.Model != "claude-sonnet-4-7" || resolved.Spec.RuntimeVersion != "2.1.119" {
		t.Fatalf("Claude resolved model=%q version=%q", resolved.Spec.Model, resolved.Spec.RuntimeVersion)
	}
}

// TestResolveAgentForPod_RuntimeVersion_EmptyBothIsValid documents the
// crucial difference from Model: an empty resolved runtimeVersion does
// NOT raise a condition or fail pod creation. Empty means
// "use the baked-in CC version" — the byte-equivalent boot path that
// existing installs run today. Only Model is required.
func TestResolveAgentForPod_RuntimeVersion_EmptyBothIsValid(t *testing.T) {
	agent := newResolverAgent("claude-sonnet-4") // model set; runtimeVersion empty
	r, c := newResolverReconciler(t, agent, nil) // no ConfigMap → empty defaults

	resolved, err := r.resolveAgentForPod(context.Background(), agent)
	if err != nil {
		t.Fatalf("resolveAgentForPod: %v", err)
	}
	if resolved.Spec.RuntimeVersion != "" {
		t.Errorf("RuntimeVersion: got %q, want empty (no version configured)", resolved.Spec.RuntimeVersion)
	}
	// No ModelUnresolved condition should be set — model is fine.
	fresh := &kyberv1.Agent{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: rfdNS, Name: rfdAgentName}, fresh); err != nil {
		t.Fatalf("get: %v", err)
	}
	if cond := meta.FindStatusCondition(fresh.Status.Conditions, kyberv1.AgentConditionModelUnresolved); cond != nil {
		t.Errorf("ModelUnresolved condition should not fire when only runtimeVersion is empty: got %+v", cond)
	}
}

// TestResolveAgentForPod_NilResolverSafe documents that wiring the
// reconciler without a Resolver (e.g., a unit test that doesn't care)
// still works: spec.model wins, empty spec.model errors as "no resolvable
// model" (because there's no default source to consult).
func TestResolveAgentForPod_NilResolverSafe(t *testing.T) {
	agent := newResolverAgent("claude-sonnet-4")
	scheme := newResolverScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(agent).Build()
	r := &AgentReconciler{
		Client:   c,
		Scheme:   scheme,
		Recorder: record.NewFakeRecorder(10),
		// FleetDefaults left nil.
	}
	resolved, err := r.resolveAgentForPod(context.Background(), agent)
	if err != nil {
		t.Fatalf("resolveAgentForPod: %v", err)
	}
	if resolved.Spec.Model != "claude-sonnet-4" {
		t.Errorf("resolved Spec.Model = %q", resolved.Spec.Model)
	}
}

// errReader is a client.Reader that always errors — used to inject a
// transient API failure into the Resolver's underlying read.
type errReader struct{ err error }

func (e *errReader) Get(_ context.Context, _ types.NamespacedName, _ client.Object, _ ...client.GetOption) error {
	return e.err
}

func (e *errReader) List(_ context.Context, _ client.ObjectList, _ ...client.ListOption) error {
	return e.err
}

// The model switch has a default: branch; the runtimeVersion switch must too.
// Listing only "codex" and "claude-code" would silently drop fleet-default
// version resolution for any runtime added later — a failure that surfaces
// only as an agent quietly running its image's baked-in harness version.
func TestResolveAgentForPod_RuntimeVersionFallsBackForNonCodexRuntimes(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: rfdConfigMap, Namespace: rfdNS},
		Data: map[string]string{
			fleetdefaults.KeyDefaultModel:               "claude-sonnet-4",
			fleetdefaults.KeyDefaultRuntimeVersion:      "2.1.119",
			fleetdefaults.KeyCodexDefaultModel:          "gpt-5.6-sol",
			fleetdefaults.KeyCodexDefaultRuntimeVersion: "0.146.0",
		},
	}

	for _, tc := range []struct {
		runtime     string
		wantModel   string
		wantVersion string
	}{
		{"claude-code", "claude-sonnet-4", "2.1.119"},
		{"codex", "gpt-5.6-sol", "0.146.0"},
		// A hypothetical future runtime: it must still pick up the legacy
		// defaults rather than silently resolving to empty.
		{"some-future-runtime", "claude-sonnet-4", "2.1.119"},
	} {
		t.Run(tc.runtime, func(t *testing.T) {
			agent := newResolverAgent("")
			agent.Spec.Runtime = tc.runtime
			r, _ := newResolverReconciler(t, agent, cm.DeepCopy())

			resolved, err := r.resolveAgentForPod(context.Background(), agent)
			if err != nil {
				t.Fatalf("resolveAgentForPod: %v", err)
			}
			if resolved.Spec.Model != tc.wantModel {
				t.Errorf("Model = %q, want %q", resolved.Spec.Model, tc.wantModel)
			}
			if resolved.Spec.RuntimeVersion != tc.wantVersion {
				t.Errorf("RuntimeVersion = %q, want %q", resolved.Spec.RuntimeVersion, tc.wantVersion)
			}
		})
	}
}

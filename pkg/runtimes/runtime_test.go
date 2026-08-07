package runtimes

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
)

// fakeRuntime is a minimal Runtime impl used to exercise the registry.
type fakeRuntime struct{ t string }

func (f *fakeRuntime) Type() string     { return f.t }
func (f *fakeRuntime) Adapter() Adapter { return NewStubAdapter("", nil, nil, nil, nil, nil, 0, "", "", "") }
func (f *fakeRuntime) Probe() Probe     { return nil }

func TestRegistry_RegisterAndGet(t *testing.T) {
	t.Cleanup(reset)
	reset()

	rt := &fakeRuntime{t: "test-runtime"}
	Register(rt)

	got, ok := Get("test-runtime")
	if !ok {
		t.Fatal("Get returned false for registered runtime")
	}
	if got != rt {
		t.Errorf("Get returned wrong runtime: got %v, want %v", got, rt)
	}
}

func TestRegistry_GetUnknown(t *testing.T) {
	t.Cleanup(reset)
	reset()

	if _, ok := Get("nope"); ok {
		t.Error("Get returned true for unregistered runtime")
	}
}

func TestRegistry_All(t *testing.T) {
	t.Cleanup(reset)
	reset()

	Register(&fakeRuntime{t: "a"})
	Register(&fakeRuntime{t: "b"})

	all := All()
	if len(all) != 2 {
		t.Fatalf("All returned %d runtimes, want 2", len(all))
	}
	seen := map[string]bool{}
	for _, rt := range all {
		seen[rt.Type()] = true
	}
	if !seen["a"] || !seen["b"] {
		t.Errorf("All missing entries; saw: %v", seen)
	}
}

func TestRegistry_RegisterDuplicate_Panics(t *testing.T) {
	t.Cleanup(reset)
	reset()

	Register(&fakeRuntime{t: "dupe"})

	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic on duplicate registration; got nil")
		}
	}()
	Register(&fakeRuntime{t: "dupe"})
}

func TestRegistry_RegisterEmptyType_Panics(t *testing.T) {
	t.Cleanup(reset)
	reset()

	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic on empty Type; got nil")
		}
	}()
	Register(&fakeRuntime{t: ""})
}

func TestNewStubAdapter_ImplementsAdapter(t *testing.T) {
	a := NewStubAdapter(
		"img:v1",
		[]string{"--flag"},
		[]corev1.EnvVar{{Name: "K", Value: "V"}},
		[]SecretMount{{Name: "sec", MountPath: "/mnt", ProviderClass: "spc"}},
		nil, nil, 30,
		"/brief", "/state", "MODEL_ENV",
	)
	if a.Image() != "img:v1" {
		t.Errorf("Image: got %q, want %q", a.Image(), "img:v1")
	}
	if got := a.SecretMounts(&kyberv1.Agent{}); len(got) != 1 || got[0].Name != "sec" {
		t.Errorf("SecretMounts: got %+v", got)
	}
	if a.GracefulShutdownSeconds() != 30 {
		t.Errorf("GracefulShutdownSeconds: got %d, want 30", a.GracefulShutdownSeconds())
	}
	if a.Type() != "stub" {
		t.Errorf("Type: got %q, want stub", a.Type())
	}
	if a.RestartSessionCommand() != nil {
		t.Errorf("RestartSessionCommand should be nil; got %v", a.RestartSessionCommand())
	}
}

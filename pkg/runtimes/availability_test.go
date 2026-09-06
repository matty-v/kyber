package runtimes_test

import (
	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/runtimes"
	_ "github.com/matty-v/kyber/pkg/runtimes/claudecode"
	_ "github.com/matty-v/kyber/pkg/runtimes/codex"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"testing"
	"time"
)

func TestAvailabilityRequiresCurrentEvidence(t *testing.T) {
	now := time.Now()
	for _, id := range []string{"claude-code", "codex"} {
		for _, tc := range []struct {
			name, want string
			mutate     func(*kyberv1.Agent)
		}{
			{"ready", "available", func(*kyberv1.Agent) {}},
			{"missing", "unknown", func(a *kyberv1.Agent) { a.Status.Runtime.Capabilities = nil }},
			{"stale", "unknown", func(a *kyberv1.Agent) {
				a.Status.Runtime.Capabilities.ObservedAt = metav1.NewTime(now.Add(-2 * time.Minute))
			}},
			{"different version", "unknown", func(a *kyberv1.Agent) { a.Status.Runtime.Capabilities.InstalledVersion = "different" }},
			{"future contract", "unknown", func(a *kyberv1.Agent) { a.Status.Runtime.Capabilities.ContractVersion = "2.0" }},
			{"not wired", "unavailable", func(a *kyberv1.Agent) { a.Status.Runtime.Capabilities.Features[string(runtimes.JobTurnHooks)] = false }},
			{"stopped", "unavailable", func(a *kyberv1.Agent) { a.Status.Phase = kyberv1.AgentPhaseStopped }},
		} {
			t.Run(id+"/"+tc.name, func(t *testing.T) {
				a := &kyberv1.Agent{}
				a.Spec.Runtime = id
				a.Status.Phase = kyberv1.AgentPhaseRunning
				a.Status.Runtime.InstalledVersion = "fixture-version"
				a.Status.Runtime.Capabilities = &kyberv1.RuntimeCapabilitiesObservation{ContractVersion: "1.0", InstalledVersion: "fixture-version", ObservedAt: metav1.NewTime(now), Features: map[string]bool{string(runtimes.JobTurnHooks): true}}
				tc.mutate(a)
				if got := runtimes.AvailabilityFor(a, runtimes.JobTurnHooks, now); got.State != tc.want {
					t.Fatalf("got %+v, want %s", got, tc.want)
				}
			})
		}
	}
}
func TestDescriptorCopiesAndUnsupportedRuntime(t *testing.T) {
	first, ok := runtimes.Describe("codex")
	if !ok {
		t.Fatal("descriptor missing")
	}
	first.AuthModes[0].Name = "changed"
	first.Features[0] = "made-up"
	second, _ := runtimes.Describe("codex")
	if second.AuthModes[0].Name == "changed" || second.Supports("made-up") {
		t.Fatal("mutable shared descriptor")
	}
	a := &kyberv1.Agent{}
	a.Spec.Runtime = "unknown"
	if got := runtimes.AvailabilityFor(a, runtimes.TaskReceipts, time.Now()); got.Supported || got.State != "unavailable" {
		t.Fatalf("unknown runtime advertised: %+v", got)
	}
}

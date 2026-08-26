package machine

import (
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/tools/record"

	"github.com/matty-v/kyber/pkg/adapters"
	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
)

func TestRecordFallbackEventsEmitsTransitionsOnce(t *testing.T) {
	recorder := record.NewFakeRecorder(10)
	r := &MachineReconciler{Recorder: recorder}
	machine := &kyberv1.Machine{}
	now := time.Now().UTC()
	observation := adapters.CapacityObservation{
		State:                         adapters.CapacityAvailable,
		EffectiveAvailabilityClass:    string(kyberv1.MachineAvailabilityReliable),
		CostOptimizedUnavailableSince: now.Add(-5 * time.Minute),
		FallbackSince:                 now,
	}

	r.recordFallbackEvents(machine, kyberv1.MachineStatus{}, observation)
	assertRecordedReasons(t, recorder, "CostOptimizedUnavailable", "ReliableFallbackStarted", "ReliableFallbackReady")

	previous := kyberv1.MachineStatus{
		CostOptimizedUnavailableSince: optionalMetaTime(observation.CostOptimizedUnavailableSince),
		FallbackSince:                 optionalMetaTime(observation.FallbackSince),
	}
	r.recordFallbackEvents(machine, previous, observation)
	assertNoRecordedEvent(t, recorder)
}

func TestRecordFallbackEventsReportsRetryResult(t *testing.T) {
	for _, test := range []struct {
		name           string
		effectiveClass string
		terminalReason string
	}{
		{name: "ready", effectiveClass: string(kyberv1.MachineAvailabilityCostOptimized), terminalReason: "CostOptimizedRetryReady"},
		{name: "rolled back", effectiveClass: string(kyberv1.MachineAvailabilityReliable), terminalReason: "CostOptimizedRetryRolledBack"},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := record.NewFakeRecorder(10)
			r := &MachineReconciler{Recorder: recorder}
			r.recordFallbackEvents(&kyberv1.Machine{}, kyberv1.MachineStatus{}, adapters.CapacityObservation{
				EffectiveAvailabilityClass: test.effectiveClass,
				CostOptimizedRetryObserved: "retry-1",
			})
			assertRecordedReasons(t, recorder, "CostOptimizedRetryStarted", test.terminalReason)
		})
	}
}

func assertRecordedReasons(t *testing.T, recorder *record.FakeRecorder, reasons ...string) {
	t.Helper()
	for _, reason := range reasons {
		select {
		case event := <-recorder.Events:
			if !strings.Contains(event, reason) {
				t.Fatalf("event = %q, want reason %q", event, reason)
			}
		default:
			t.Fatalf("missing event reason %q", reason)
		}
	}
}

func assertNoRecordedEvent(t *testing.T, recorder *record.FakeRecorder) {
	t.Helper()
	select {
	case event := <-recorder.Events:
		t.Fatalf("unexpected duplicate event %q", event)
	default:
	}
}

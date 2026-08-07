package api

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
)

// TestIsHeartbeatOnlyUpdate verifies the heartbeat-filter predicate behind
// OnUpdate's eventbus suppression (#263). Three cases per the issue spec:
// (a) heartbeat-only diff → suppressed; (b) phase change → published;
// (c) activity.state change → published.
func TestIsHeartbeatOnlyUpdate(t *testing.T) {
	t0 := metav1.NewTime(time.Now())
	t1 := metav1.NewTime(t0.Add(5 * time.Second))

	baseAgent := func() *kyberv1.Agent {
		return &kyberv1.Agent{
			Spec: kyberv1.AgentSpec{Machine: "worker-1", Runtime: "claude-code"},
			Status: kyberv1.AgentStatus{
				Phase: kyberv1.AgentPhaseRunning,
				Activity: &kyberv1.ActivityStatus{
					LastHeartbeatAt: &t0,
					State:           "idle",
				},
			},
		}
	}

	t.Run("heartbeat-only update is suppressed", func(t *testing.T) {
		oldA := baseAgent()
		newA := baseAgent()
		newA.Status.Activity.LastHeartbeatAt = &t1
		if !isHeartbeatOnlyUpdate(oldA, newA) {
			t.Error("want true (suppress) when only LastHeartbeatAt changes")
		}
	})

	t.Run("phase change is published", func(t *testing.T) {
		oldA := baseAgent()
		newA := baseAgent()
		newA.Status.Phase = kyberv1.AgentPhaseStopped
		if isHeartbeatOnlyUpdate(oldA, newA) {
			t.Error("want false (publish) when status.phase changes")
		}
	})

	t.Run("activity state change is published", func(t *testing.T) {
		oldA := baseAgent()
		newA := baseAgent()
		newA.Status.Activity.State = "working"
		if isHeartbeatOnlyUpdate(oldA, newA) {
			t.Error("want false (publish) when status.activity.state changes")
		}
	})

	// Defensive corners worth pinning so future refactors don't accidentally
	// regress the suppression contract.
	t.Run("non-Agent object is published", func(t *testing.T) {
		if isHeartbeatOnlyUpdate(&kyberv1.Machine{}, &kyberv1.Machine{}) {
			t.Error("want false (publish) when objects are not *Agent")
		}
	})

	t.Run("spec change is published", func(t *testing.T) {
		oldA := baseAgent()
		newA := baseAgent()
		newA.Spec.Model = "claude-opus-4"
		if isHeartbeatOnlyUpdate(oldA, newA) {
			t.Error("want false (publish) when spec changes")
		}
	})
}

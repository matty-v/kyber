package agent

// reconciler_pod_status_test.go — Unit tests for the kyber#355 pod-status
// mirror helpers. The full Reconcile() integration of these (the step 5e
// branch + the diff-gated patch) is exercised by the envtest-backed
// reconciler tests; this file covers the pure helpers so the regression
// surface for the panel-population fix is small and self-contained.

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
)

// agentWith builds an Agent with a specific Status payload — minimal helper
// so the table-driven tests below stay readable.
func agentWith(status kyberv1.AgentStatus) *kyberv1.Agent {
	return &kyberv1.Agent{Status: status}
}

// podWith builds a Pod with the four pod-derived fields populated.
func podWith(name, ip, node string, start *metav1.Time) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       corev1.PodSpec{NodeName: node},
		Status:     corev1.PodStatus{PodIP: ip, StartTime: start},
	}
}

// TestPodDerivedStatusDiffers_AllFieldsMatch is the steady-state case:
// after the patch lands once, every subsequent reconcile sees an exact
// match and produces zero patches. This is the property #355 is designed
// around — diff-gated writes, no churn.
func TestPodDerivedStatusDiffers_AllFieldsMatch(t *testing.T) {
	start := metav1.NewTime(time.Date(2026, 5, 27, 10, 0, 0, 0, time.UTC))
	agent := agentWith(kyberv1.AgentStatus{
		PodName:   "boba-fett-xyz",
		PodIP:     "10.244.3.21",
		NodeName:  "gke-node-7",
		StartTime: &start,
	})
	pod := podWith("boba-fett-xyz", "10.244.3.21", "gke-node-7", &start)
	if podDerivedStatusDiffers(agent, pod) {
		t.Error("expected no diff when every pod-derived field already matches")
	}
}

// TestPodDerivedStatusDiffers_EmptyAgentStatus is the populate-on-first-
// reconcile case. Before the kyber#355 fix this was the steady state for
// every Running agent — agent.Status only had Phase set.
func TestPodDerivedStatusDiffers_EmptyAgentStatus(t *testing.T) {
	start := metav1.NewTime(time.Now())
	agent := agentWith(kyberv1.AgentStatus{Phase: kyberv1.AgentPhaseRunning})
	pod := podWith("boba-fett-xyz", "10.244.3.21", "gke-node-7", &start)
	if !podDerivedStatusDiffers(agent, pod) {
		t.Error("expected diff when agent.Status has no pod-derived fields yet")
	}
}

// TestPodDerivedStatusDiffers_PerFieldChanges walks each field
// individually so a future refactor can't silently drop one of them.
func TestPodDerivedStatusDiffers_PerFieldChanges(t *testing.T) {
	start := metav1.NewTime(time.Now())
	other := metav1.NewTime(start.Add(time.Minute))
	base := kyberv1.AgentStatus{
		PodName:   "boba-fett-xyz",
		PodIP:     "10.244.3.21",
		NodeName:  "gke-node-7",
		StartTime: &start,
	}
	cases := []struct {
		name string
		pod  *corev1.Pod
	}{
		{"PodName differs", podWith("boba-fett-new", "10.244.3.21", "gke-node-7", &start)},
		{"PodIP differs", podWith("boba-fett-xyz", "10.244.3.22", "gke-node-7", &start)},
		{"NodeName differs", podWith("boba-fett-xyz", "10.244.3.21", "gke-node-99", &start)},
		{"StartTime differs", podWith("boba-fett-xyz", "10.244.3.21", "gke-node-7", &other)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			agent := agentWith(base)
			if !podDerivedStatusDiffers(agent, tc.pod) {
				t.Errorf("%s: expected diff but got equal", tc.name)
			}
		})
	}
}

// TestPodDerivedStatusDiffers_StartTimeNilOnBothSides covers the Pending-
// pod case. A pod that hasn't been acknowledged by kubelet yet has nil
// StartTime; agent.Status.StartTime will also be nil at that point. We
// should not generate a spurious diff.
func TestPodDerivedStatusDiffers_StartTimeNilOnBothSides(t *testing.T) {
	agent := agentWith(kyberv1.AgentStatus{
		PodName:  "boba-fett-xyz",
		PodIP:    "10.244.3.21",
		NodeName: "gke-node-7",
		// StartTime nil
	})
	pod := podWith("boba-fett-xyz", "10.244.3.21", "gke-node-7", nil)
	if podDerivedStatusDiffers(agent, pod) {
		t.Error("expected no diff when both StartTime values are nil")
	}
}

// TestPodDerivedStatusDiffers_PendingPodNoPodIP covers the "pod scheduled
// but no IP yet" intermediate state. Status.PodIP="" matches pod.PodIP=""
// so no diff is reported for that field alone — but NodeName likely
// becomes set first, so the helper should detect that diff. This locks
// the partial-update behavior.
func TestPodDerivedStatusDiffers_PendingPodNoPodIP(t *testing.T) {
	agent := agentWith(kyberv1.AgentStatus{
		PodName:  "boba-fett-xyz",
		PodIP:    "", // not assigned yet
		NodeName: "",
	})
	pod := podWith("boba-fett-xyz", "", "gke-node-7", nil) // node assigned, IP not yet
	if !podDerivedStatusDiffers(agent, pod) {
		t.Error("expected diff on partial pod state (NodeName set, PodIP still empty)")
	}
}

// TestApplyPodDerivedStatus_OverwritesAllFour locks the contract on the
// write side: every pod-derived field is mirrored into agent.Status.
func TestApplyPodDerivedStatus_OverwritesAllFour(t *testing.T) {
	start := metav1.NewTime(time.Date(2026, 5, 27, 10, 0, 0, 0, time.UTC))
	agent := agentWith(kyberv1.AgentStatus{Phase: kyberv1.AgentPhaseRunning})
	pod := podWith("boba-fett-xyz", "10.244.3.21", "gke-node-7", &start)
	applyPodDerivedStatus(agent, pod)
	if agent.Status.PodName != "boba-fett-xyz" {
		t.Errorf("PodName: got %q, want boba-fett-xyz", agent.Status.PodName)
	}
	if agent.Status.PodIP != "10.244.3.21" {
		t.Errorf("PodIP: got %q, want 10.244.3.21", agent.Status.PodIP)
	}
	if agent.Status.NodeName != "gke-node-7" {
		t.Errorf("NodeName: got %q, want gke-node-7", agent.Status.NodeName)
	}
	if agent.Status.StartTime == nil || !agent.Status.StartTime.Equal(&start) {
		t.Errorf("StartTime: got %v, want %v", agent.Status.StartTime, &start)
	}
}

// TestApplyPodDerivedStatus_PreservesStartTimeWhenPodHasNone covers the
// pending-pod case: when the pod hasn't been acknowledged by kubelet yet
// (StartTime nil), don't clobber an existing agent.Status.StartTime that
// updatePhase set on the Starting→Running transition. The agent value
// stays until the pod actually has one.
func TestApplyPodDerivedStatus_PreservesStartTimeWhenPodHasNone(t *testing.T) {
	existing := metav1.NewTime(time.Now())
	agent := agentWith(kyberv1.AgentStatus{
		Phase:     kyberv1.AgentPhaseRunning,
		StartTime: &existing,
	})
	pod := podWith("boba-fett-xyz", "10.244.3.21", "gke-node-7", nil)
	applyPodDerivedStatus(agent, pod)
	if agent.Status.StartTime == nil {
		t.Fatal("StartTime was cleared when pod has nil StartTime; should preserve")
	}
	if !agent.Status.StartTime.Equal(&existing) {
		t.Errorf("StartTime: got %v, want preserved %v", agent.Status.StartTime, existing)
	}
}

// TestApplyPodDerivedStatus_DoesNotTouchRestartCount asserts Decision 1
// from the design pass: RestartCount is the agent-retry counter, not a
// k8s container-restart count. The pod-status mirror must NEVER write
// RestartCount — that field is owned by ActionResetRetryAndCreatePod.
// Repurposing it would silently invert behavior for anyone watching the
// field.
func TestApplyPodDerivedStatus_DoesNotTouchRestartCount(t *testing.T) {
	start := metav1.NewTime(time.Now())
	agent := agentWith(kyberv1.AgentStatus{
		Phase:        kyberv1.AgentPhaseRunning,
		RestartCount: 3, // mid-retry
	})
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "boba-fett-xyz"},
		Spec:       corev1.PodSpec{NodeName: "gke-node-7"},
		Status: corev1.PodStatus{
			PodIP:     "10.244.3.21",
			StartTime: &start,
			// k8s container restart count: emphatically must not propagate.
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "agent", RestartCount: 7},
			},
		},
	}
	applyPodDerivedStatus(agent, pod)
	if agent.Status.RestartCount != 3 {
		t.Errorf("RestartCount: got %d, want 3 (must NOT be repurposed from container restarts)",
			agent.Status.RestartCount)
	}
}

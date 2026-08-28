package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stypes "k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
)

func TestClassifySchedulingCategory(t *testing.T) {
	tests := []struct {
		name    string
		reason  string
		message string
		want    string
	}{
		{
			name:    "FailedScheduling insufficient memory → Capacity",
			reason:  "FailedScheduling",
			message: "0/1 nodes are available: 1 Insufficient memory.",
			want:    "Capacity",
		},
		{
			name:    "FailedScheduling insufficient cpu → Capacity",
			reason:  "FailedScheduling",
			message: "0/1 nodes are available: 1 Insufficient cpu.",
			want:    "Capacity",
		},
		{
			name:    "FailedScheduling insufficient ephemeral-storage → Capacity",
			reason:  "FailedScheduling",
			message: "0/1 nodes are available: 1 Insufficient ephemeral-storage.",
			want:    "Capacity",
		},
		{
			name:    "FailedScheduling untolerated taint → Placement",
			reason:  "FailedScheduling",
			message: "0/3 nodes are available: 3 node(s) had untolerated taint {gpu: }.",
			want:    "Placement",
		},
		{
			name:    "FailedScheduling node selector mismatch → Placement",
			reason:  "FailedScheduling",
			message: "0/2 nodes are available: 2 node(s) didn't match Pod's node affinity/selector.",
			want:    "Placement",
		},
		{
			name:    "FailedScheduling volume node affinity → Storage",
			reason:  "FailedScheduling",
			message: "0/1 nodes are available: 1 had volume node affinity conflict.",
			want:    "Storage",
		},
		{
			name:    "FailedScheduling unbound PVC → Storage",
			reason:  "FailedScheduling",
			message: "pod has unbound immediate PersistentVolumeClaims",
			want:    "Storage",
		},
		{
			name:    "ImagePullBackOff → Image",
			reason:  "ImagePullBackOff",
			message: `Back-off pulling image "ghcr.io/matty-v/kyber-claude-code:latest"`,
			want:    "Image",
		},
		{
			name:    "ErrImagePull → Image",
			reason:  "ErrImagePull",
			message: `Failed to pull image "ghcr.io/matty-v/missing": not found`,
			want:    "Image",
		},
		{
			name:    "FailedMount → Storage",
			reason:  "FailedMount",
			message: `MountVolume.SetUp failed for volume "kube-api-access": no such file`,
			want:    "Storage",
		},
		{
			name:    "Unknown reason + unclassifiable message → Other",
			reason:  "FailedScheduling",
			message: "0/1 nodes are available: 1 something completely new.",
			want:    "Other",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifySchedulingCategory(tt.reason, tt.message)
			if got != tt.want {
				t.Errorf("classifySchedulingCategory(%q, %q) = %q, want %q",
					tt.reason, tt.message, got, tt.want)
			}
		})
	}
}

// schedulingTestScheme assembles only the schemes populateSchedulingStatus
// needs (corev1.Event + kyberv1.Agent). Avoids pulling in the full
// envtest harness for what is otherwise a unit test.
func schedulingTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("add clientgo scheme: %v", err)
	}
	if err := kyberv1.AddToScheme(s); err != nil {
		t.Fatalf("add kyber scheme: %v", err)
	}
	return s
}

func newPendingPod(uid string, age time.Duration) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "agent-alice",
			Namespace:         "kyber",
			UID:               k8stypes.UID(uid),
			CreationTimestamp: metav1.NewTime(time.Now().Add(-age)),
		},
		Status: corev1.PodStatus{Phase: corev1.PodPending},
	}
}

func newSchedulingEvent(podUID, reason, message string, observedAt time.Time) *corev1.Event {
	return &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "evt-" + reason,
			Namespace: "kyber",
		},
		InvolvedObject: corev1.ObjectReference{
			Kind:      "Pod",
			Name:      "agent-alice",
			Namespace: "kyber",
			UID:       k8stypes.UID(podUID),
		},
		Reason:        reason,
		Message:       message,
		LastTimestamp: metav1.NewTime(observedAt),
	}
}

func TestPopulateSchedulingStatus_WithinGraceWindow(t *testing.T) {
	pod := newPendingPod("pod-uid", 5*time.Second) // < 30s grace
	agent := &kyberv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "alice", Namespace: "kyber"}}
	c := fake.NewClientBuilder().WithScheme(schedulingTestScheme(t)).Build()
	if changed := populateSchedulingStatus(context.Background(), c, pod, agent); changed {
		t.Fatalf("expected no change within grace window, got changed=true")
	}
	if agent.Status.Scheduling != nil {
		t.Errorf("expected Scheduling unset, got %+v", agent.Status.Scheduling)
	}
}

func TestPopulateSchedulingStatus_NoMatchingEvents(t *testing.T) {
	pod := newPendingPod("pod-uid", time.Minute)
	agent := &kyberv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "alice", Namespace: "kyber"}}
	// Seed an event for a different pod UID (should be ignored).
	other := newSchedulingEvent("other-uid", "FailedScheduling",
		"0/1 nodes are available: 1 Insufficient memory.", time.Now())
	c := fake.NewClientBuilder().WithScheme(schedulingTestScheme(t)).
		WithObjects(other).Build()
	if changed := populateSchedulingStatus(context.Background(), c, pod, agent); changed {
		t.Fatalf("expected no change when no events match this pod, got changed=true")
	}
	if agent.Status.Scheduling != nil {
		t.Errorf("expected Scheduling unset, got %+v", agent.Status.Scheduling)
	}
}

func TestPopulateSchedulingStatus_CapacityFailure(t *testing.T) {
	pod := newPendingPod("pod-uid", time.Minute)
	agent := &kyberv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "alice", Namespace: "kyber"}}
	ev := newSchedulingEvent("pod-uid", "FailedScheduling",
		"0/1 nodes are available: 1 Insufficient memory.", time.Now())
	c := fake.NewClientBuilder().WithScheme(schedulingTestScheme(t)).
		WithObjects(ev).Build()

	if !populateSchedulingStatus(context.Background(), c, pod, agent) {
		t.Fatalf("expected change=true")
	}
	if agent.Status.Scheduling == nil {
		t.Fatalf("expected Scheduling populated")
	}
	if agent.Status.Scheduling.Category != "Capacity" {
		t.Errorf("Category = %q, want Capacity", agent.Status.Scheduling.Category)
	}
	if agent.Status.Scheduling.LastError == "" {
		t.Errorf("LastError empty")
	}
	if agent.Status.Scheduling.FirstObservedAt == nil {
		t.Errorf("FirstObservedAt nil")
	}
}

func TestPopulateSchedulingStatus_ImagePullBackOff(t *testing.T) {
	pod := newPendingPod("pod-uid", time.Minute)
	agent := &kyberv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "alice", Namespace: "kyber"}}
	ev := newSchedulingEvent("pod-uid", "ImagePullBackOff",
		`Back-off pulling image "ghcr.io/matty-v/kyber-claude-code:latest"`, time.Now())
	c := fake.NewClientBuilder().WithScheme(schedulingTestScheme(t)).
		WithObjects(ev).Build()

	populateSchedulingStatus(context.Background(), c, pod, agent)
	if agent.Status.Scheduling == nil || agent.Status.Scheduling.Category != "Image" {
		t.Errorf("expected Image category, got %+v", agent.Status.Scheduling)
	}
}

func TestPopulateSchedulingStatus_PreservesFirstObservedAtAcrossMessageChange(t *testing.T) {
	pod := newPendingPod("pod-uid", time.Minute)
	originallyObserved := metav1.NewTime(time.Now().Add(-5 * time.Minute))
	agent := &kyberv1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "alice", Namespace: "kyber"},
		Status: kyberv1.AgentStatus{
			Scheduling: &kyberv1.AgentSchedulingStatus{
				Category:        "Capacity",
				LastError:       "old message",
				FirstObservedAt: &originallyObserved,
			},
		},
	}
	// New event with a different message but same category → should
	// overwrite LastError but keep the original FirstObservedAt.
	ev := newSchedulingEvent("pod-uid", "FailedScheduling",
		"0/1 nodes are available: 1 Insufficient cpu.", time.Now())
	c := fake.NewClientBuilder().WithScheme(schedulingTestScheme(t)).
		WithObjects(ev).Build()

	populateSchedulingStatus(context.Background(), c, pod, agent)
	if agent.Status.Scheduling == nil {
		t.Fatalf("expected Scheduling still populated")
	}
	if !agent.Status.Scheduling.FirstObservedAt.Equal(&originallyObserved) {
		t.Errorf("FirstObservedAt drifted: got %v, want %v",
			agent.Status.Scheduling.FirstObservedAt, originallyObserved)
	}
	if agent.Status.Scheduling.LastError != "0/1 nodes are available: 1 Insufficient cpu." {
		t.Errorf("LastError not updated: %q", agent.Status.Scheduling.LastError)
	}
}

func TestPopulateSchedulingStatus_PreservesFirstObservedAtAcrossCategoryChange(t *testing.T) {
	// Operator's pod was stuck for 5 min on Capacity, then they pushed a
	// new image — now it's stuck on ImagePullBackOff. The "first observed"
	// timestamp should still anchor to when stuckness STARTED, not to when
	// the controller most-recently noticed a category swap.
	pod := newPendingPod("pod-uid", time.Minute)
	originallyObserved := metav1.NewTime(time.Now().Add(-5 * time.Minute))
	agent := &kyberv1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "alice", Namespace: "kyber"},
		Status: kyberv1.AgentStatus{
			Scheduling: &kyberv1.AgentSchedulingStatus{
				Category:        "Capacity",
				LastError:       "0/1 nodes are available: 1 Insufficient memory.",
				FirstObservedAt: &originallyObserved,
			},
		},
	}
	ev := newSchedulingEvent("pod-uid", "ImagePullBackOff",
		`Back-off pulling image "ghcr.io/matty-v/missing:latest"`, time.Now())
	c := fake.NewClientBuilder().WithScheme(schedulingTestScheme(t)).
		WithObjects(ev).Build()

	if !populateSchedulingStatus(context.Background(), c, pod, agent) {
		t.Fatalf("expected change=true on category swap")
	}
	if agent.Status.Scheduling.Category != "Image" {
		t.Errorf("Category did not update: got %q, want Image",
			agent.Status.Scheduling.Category)
	}
	if !agent.Status.Scheduling.FirstObservedAt.Equal(&originallyObserved) {
		t.Errorf("FirstObservedAt should be preserved across category change: got %v, want %v",
			agent.Status.Scheduling.FirstObservedAt, originallyObserved)
	}
}

func TestPopulateSchedulingStatus_PicksMostRecentEvent(t *testing.T) {
	// Pod hits FailedScheduling first (memory), then ImagePullBackOff after
	// scheduling onto a node where the image is missing. The classifier
	// should pick the more-recent event so the operator sees the CURRENT
	// problem, not the first one.
	pod := newPendingPod("pod-uid", time.Minute)
	agent := &kyberv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "alice", Namespace: "kyber"}}
	older := newSchedulingEvent("pod-uid", "FailedScheduling",
		"0/1 nodes are available: 1 Insufficient memory.",
		time.Now().Add(-2*time.Minute))
	newer := newSchedulingEvent("pod-uid", "ImagePullBackOff",
		`Back-off pulling image "ghcr.io/matty-v/missing:latest"`,
		time.Now())
	older.Name = "evt-older"
	newer.Name = "evt-newer"
	c := fake.NewClientBuilder().WithScheme(schedulingTestScheme(t)).
		WithObjects(older, newer).Build()

	populateSchedulingStatus(context.Background(), c, pod, agent)
	if agent.Status.Scheduling == nil {
		t.Fatalf("expected Scheduling populated")
	}
	if agent.Status.Scheduling.Category != "Image" {
		t.Errorf("expected the more-recent event to win (Image), got %q",
			agent.Status.Scheduling.Category)
	}
}

func TestPopulateSchedulingStatus_IdempotentOnSameEvent(t *testing.T) {
	pod := newPendingPod("pod-uid", time.Minute)
	agent := &kyberv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "alice", Namespace: "kyber"}}
	ev := newSchedulingEvent("pod-uid", "FailedScheduling",
		"0/1 nodes are available: 1 Insufficient memory.", time.Now())
	c := fake.NewClientBuilder().WithScheme(schedulingTestScheme(t)).
		WithObjects(ev).Build()

	if !populateSchedulingStatus(context.Background(), c, pod, agent) {
		t.Fatalf("first call should report change")
	}
	if populateSchedulingStatus(context.Background(), c, pod, agent) {
		t.Errorf("second call should be a no-op (same category + message)")
	}
}

func TestClearSchedulingStatus(t *testing.T) {
	t.Run("clears when set", func(t *testing.T) {
		ts := metav1.NewTime(time.Now())
		agent := &kyberv1.Agent{
			Status: kyberv1.AgentStatus{
				Scheduling: &kyberv1.AgentSchedulingStatus{
					Category: "Capacity", LastError: "x", FirstObservedAt: &ts,
				},
			},
		}
		if !clearSchedulingStatus(agent) {
			t.Errorf("expected change=true")
		}
		if agent.Status.Scheduling != nil {
			t.Errorf("expected nil after clear")
		}
	})
	t.Run("no-op when already nil", func(t *testing.T) {
		agent := &kyberv1.Agent{}
		if clearSchedulingStatus(agent) {
			t.Errorf("expected change=false on already-nil")
		}
	})
}

// stalledPod is a Pending pod whose sidecars are up but whose runtime
// container never started — the shape a Kyber agent pod takes when it hangs
// in Creating. No scheduler or kubelet failure event is ever emitted for it,
// so none of the six classified reasons apply.
func stalledPod(uid string, age time.Duration) *corev1.Pod {
	pod := newPendingPod(uid, age)
	pod.Status.InitContainerStatuses = []corev1.ContainerStatus{
		{Name: "kyber-status-sidecar", State: corev1.ContainerState{
			Running: &corev1.ContainerStateRunning{}}},
	}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{
		{Name: "agent", State: corev1.ContainerState{
			Waiting: &corev1.ContainerStateWaiting{Reason: "PodInitializing"}}},
	}
	return pod
}

// A pod stuck for hours with no classified event must still say something.
// Before this, an operator got an unchanging phase and an empty reason, and
// the only ground truth was `kubectl describe pod` on the box. Observed on
// kyber-canary: an agent sat in Creating for eleven hours reporting nothing.
func TestPopulateSchedulingStatus_StalledPodWithNoEventsReportsWhatItCanSee(t *testing.T) {
	pod := stalledPod("pod-uid", 11*time.Hour)
	agent := &kyberv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "alice", Namespace: "kyber"}}
	c := fake.NewClientBuilder().WithScheme(schedulingTestScheme(t)).Build()

	if !populateSchedulingStatus(context.Background(), c, pod, agent) {
		t.Fatal("a pod stalled for 11h with no events reported nothing")
	}
	got := agent.Status.Scheduling
	if got == nil {
		t.Fatal("Scheduling still nil")
	}
	if got.Category != "Other" {
		t.Errorf("Category = %q, want Other", got.Category)
	}
	// The message has to name the container that never started; "something is
	// wrong" with no pointer is barely better than the blank it replaces.
	if !strings.Contains(got.LastError, "agent") ||
		!strings.Contains(got.LastError, "PodInitializing") {
		t.Errorf("LastError does not name the stalled container or its state: %q", got.LastError)
	}
	if got.FirstObservedAt == nil {
		t.Error("FirstObservedAt nil")
	}
}

// A cold node pulling a large runtime image is Pending with no events for
// minutes. That is a healthy boot, not a stall, and must not raise a banner.
func TestPopulateSchedulingStatus_StalledFallbackWaitsOutASlowBoot(t *testing.T) {
	pod := stalledPod("pod-uid", 5*time.Minute)
	agent := &kyberv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "alice", Namespace: "kyber"}}
	c := fake.NewClientBuilder().WithScheme(schedulingTestScheme(t)).Build()

	if populateSchedulingStatus(context.Background(), c, pod, agent) {
		t.Fatalf("reported a stall during a normal slow boot: %+v", agent.Status.Scheduling)
	}
	if agent.Status.Scheduling != nil {
		t.Errorf("expected Scheduling unset, got %+v", agent.Status.Scheduling)
	}
}

// A real classified reason is always better than the descriptive fallback.
func TestPopulateSchedulingStatus_ClassifiedEventBeatsTheStalledFallback(t *testing.T) {
	pod := stalledPod("pod-uid", 11*time.Hour)
	agent := &kyberv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "alice", Namespace: "kyber"}}
	ev := newSchedulingEvent("pod-uid", "FailedScheduling",
		"0/1 nodes are available: 1 Insufficient memory.", time.Now())
	c := fake.NewClientBuilder().WithScheme(schedulingTestScheme(t)).
		WithObjects(ev).Build()

	populateSchedulingStatus(context.Background(), c, pod, agent)
	if agent.Status.Scheduling == nil || agent.Status.Scheduling.Category != "Capacity" {
		t.Fatalf("the fallback displaced a classified reason: %+v", agent.Status.Scheduling)
	}
}

// The fallback must never overwrite an entry another call site is managing.
func TestPopulateSchedulingStatus_StalledFallbackDoesNotOverwrite(t *testing.T) {
	pod := stalledPod("pod-uid", 11*time.Hour)
	existing := metav1.NewTime(time.Now().Add(-12 * time.Hour))
	agent := &kyberv1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "alice", Namespace: "kyber"},
		Status: kyberv1.AgentStatus{Scheduling: &kyberv1.AgentSchedulingStatus{
			Category:        "Placement",
			LastError:       "0/4 nodes are available: 3 node(s) didn't match Pod's node affinity/selector.",
			FirstObservedAt: &existing,
		}},
	}
	c := fake.NewClientBuilder().WithScheme(schedulingTestScheme(t)).Build()

	if populateSchedulingStatus(context.Background(), c, pod, agent) {
		t.Fatal("the fallback overwrote an existing scheduling entry")
	}
	if agent.Status.Scheduling.Category != "Placement" {
		t.Errorf("Category = %q, want the original Placement", agent.Status.Scheduling.Category)
	}
}

package agent

import (
	"context"
	"testing"
	"time"

	"github.com/matty-v/kyber/pkg/activity"
	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/podtoken"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestActivityObserverCredentialIsolation(t *testing.T) {
	scheme := buildTestScheme()
	agent := newTestAgent("fixture", "test")
	agent.UID = "agent-life"
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(agent).Build()
	r := &AgentReconciler{Client: c, Scheme: scheme, PodTokenKey: []byte("unit-test-observer-key")}
	if err := r.ensurePodTokenSecret(context.Background(), agent); err != nil {
		t.Fatal(err)
	}
	var secret corev1.Secret
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "test", Name: "fixture-activity-observer"}, &secret); err != nil {
		t.Fatal(err)
	}
	identity, err := podtoken.Parse(string(secret.Data["token"]), r.PodTokenKey)
	if err != nil || identity != activity.ObserverIdentity(agent.Name, string(agent.UID)) {
		t.Fatal("observer identity not bound to agent life")
	}
	if owner := metav1.GetControllerOf(&secret); owner == nil || owner.UID != agent.UID {
		t.Fatal("observer credential unowned")
	}
	spec := corev1.PodSpec{Containers: []corev1.Container{{Name: "agent"}}}
	AppendStatusSidecar(&spec, SidecarConfig{AgentName: agent.Name, Image: "fixture"})
	for _, runtime := range spec.Containers {
		for _, mount := range runtime.VolumeMounts {
			if mount.Name == observerVolume {
				t.Fatal("runtime received observer credential")
			}
		}
	}
	found := false
	for _, mount := range spec.InitContainers[0].VolumeMounts {
		if mount.Name == observerVolume && mount.ReadOnly {
			found = true
		}
	}
	if !found {
		t.Fatal("observer credential missing from sidecar")
	}
}

func TestActivityObserverClearsOnNativeSidecarReplacement(t *testing.T) {
	agent := newTestAgent("fixture", "test")
	agent.Status.Activity = &kyberv1.ActivityStatus{Work: &kyberv1.WorkObservation{PodUID: "pod", ContainerID: "first"}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "agent-fixture", UID: "pod"}, Status: corev1.PodStatus{InitContainerStatuses: []corev1.ContainerStatus{{Name: StatusSidecarContainerName, ContainerID: "second", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(time.Now())}}}}}}
	if !podDerivedStatusDiffers(agent, pod) {
		t.Fatal("replacement did not trigger refresh")
	}
	applyPodDerivedStatus(agent, pod)
	if agent.Status.Activity.Work == nil || agent.Status.Activity.Work.Epoch != "" || agent.Status.Activity.Work.Complete {
		t.Fatal("old native work survived container replacement")
	}
}

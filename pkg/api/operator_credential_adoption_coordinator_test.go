package api

import (
	"context"
	"github.com/google/uuid"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"testing"
	"time"
)

func TestNativeAdoptionRecoveryWaitsForEveryKnownLaunch(t *testing.T) {
	for _, scenario := range []string{"no-launch", "stopped", "replacement-pending", "missing-process", "node-lost"} {
		t.Run(scenario, func(t *testing.T) {
			execution, _, _, _, _ := adoptionExecutionFixture(t)
			intent := execution.intent
			scheme := runtime.NewScheme()
			_ = corev1.AddToScheme(scheme)
			_ = batchv1.AddToScheme(scheme)
			objects := []client.Object{}
			if scenario != "no-launch" {
				job, err := buildCredentialAdoptionJob(intent, intent.Adoption.ControlPlaneImage, "native-worker", "", "")
				if err != nil {
					t.Fatal(err)
				}
				job.UID = types.UID(uuid.NewString())
				job.ResourceVersion = "10"
				job.Spec.Selector = &metav1.LabelSelector{MatchLabels: map[string]string{"batch.kubernetes.io/controller-uid": string(job.UID)}}
				job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
				yes := true
				pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: job.Namespace, Name: "original-process", UID: types.UID(uuid.NewString()), Labels: map[string]string{"batch.kubernetes.io/controller-uid": string(job.UID)}, OwnerReferences: []metav1.OwnerReference{{APIVersion: "batch/v1", Kind: "Job", Name: job.Name, UID: job.UID, Controller: &yes}}}, Spec: *job.Spec.Template.Spec.DeepCopy()}
				pod.Spec.NodeName = "worker-node"
				pod.Status = corev1.PodStatus{Phase: corev1.PodSucceeded, ContainerStatuses: []corev1.ContainerStatus{{Name: "adopt-credentials", ContainerID: "containerd://original", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Completed", ContainerID: "containerd://original", FinishedAt: metav1.NewTime(time.Now())}}}}}
				objects = append(objects, job)
				if scenario != "missing-process" {
					objects = append(objects, pod)
				}
				if scenario == "node-lost" {
					pod.Status.Reason = "NodeLost"
				}
				if scenario == "replacement-pending" {
					replacement, err := buildCredentialAdoptionJob(intent, intent.Adoption.ControlPlaneImage, "native-worker", uuid.NewString(), uuid.NewString())
					if err != nil {
						t.Fatal(err)
					}
					replacement.UID = types.UID(uuid.NewString())
					replacement.ResourceVersion = "11"
					objects = append(objects, replacement)
				}
			}
			reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
			coordinator := &OperatorCredentialAdoptionCoordinator{reader: reader, namespace: intent.Admission.Namespace}
			stopped, err := coordinator.adoptionJobsStopped(context.Background(), intent.Admission.OperationID)
			if err != nil || stopped != (scenario == "no-launch" || scenario == "stopped") {
				t.Fatal("incorrect recovery eligibility", scenario, stopped, err)
			}
		})
	}
}

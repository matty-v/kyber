package operatorcredentials

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func writerFixture(t *testing.T) (*MaintenanceStore, client.Client, *corev1.Pod, *batchv1.Job) {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = batchv1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1.Pod{}, &batchv1.Job{}).Build()
	direct, err := newSecretStore(c, maintenanceClient{Client: c}, "kyber-system", "registry-fixture")
	if err != nil {
		t.Fatal(err)
	}
	store, err := newMaintenanceStore(direct, "maintenance-fixture")
	if err != nil {
		t.Fatal(err)
	}
	jobUID, podUID := types.UID(uuid.NewString()), types.UID(uuid.NewString())
	zero := int32(0)
	controller := true
	labels := map[string]string{"batch.kubernetes.io/controller-uid": string(jobUID)}
	spec := corev1.PodSpec{NodeName: "fixture-node", RestartPolicy: corev1.RestartPolicyNever, Containers: []corev1.Container{{Name: "upgrade", Image: "fixture-upgrade-image", Command: []string{"/usr/local/bin/kyber-upgrade"}}}}
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Namespace: "kyber-system", Name: "upgrade-fixture", UID: jobUID}, Spec: batchv1.JobSpec{BackoffLimit: &zero, Selector: &metav1.LabelSelector{MatchLabels: labels}, Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}, Spec: spec}}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "kyber-system", Name: "upgrade-pod-fixture", UID: podUID, Labels: labels, OwnerReferences: []metav1.OwnerReference{{APIVersion: "batch/v1", Kind: "Job", Name: job.Name, UID: jobUID, Controller: &controller}}}, Spec: spec, Status: corev1.PodStatus{Phase: corev1.PodRunning}}
	for _, object := range []client.Object{job, pod} {
		if err := c.Create(context.Background(), object); err != nil {
			t.Fatal(err)
		}
	}
	return store, c, pod, job
}
func stopWriterFixture(t *testing.T, c client.Client, pod *corev1.Pod, job *batchv1.Job) {
	t.Helper()
	ctx := context.Background()
	pod.Status.Phase = corev1.PodFailed
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "upgrade", ContainerID: "containerd://fixture", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Error", ExitCode: 1, ContainerID: "containerd://fixture", FinishedAt: metav1.NewTime(time.Now())}}}}
	if err := c.Status().Update(ctx, pod); err != nil {
		t.Fatal(err)
	}
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue}}
	if err := c.Status().Update(ctx, job); err != nil {
		t.Fatal(err)
	}
}
func TestMaintenanceWriterBindsExactNonRetryingJob(t *testing.T) {
	s, c, pod, job := writerFixture(t)
	ctx := context.Background()
	reservation, err := s.AcquireForPod(ctx, MaintenanceUpgrade, pod.Name, string(pod.UID))
	if err != nil {
		t.Fatal(err)
	}
	status, err := s.Inspect(ctx)
	if err != nil || status.Writer == nil || status.Writer.PodUID != string(pod.UID) || status.Writer.JobUID != string(job.UID) || status.Writer.Container != "upgrade" {
		t.Fatal("writer identity missing", err)
	}
	if state, err := s.WriterState(ctx, status.UID, status.Version); err != nil || state != MaintenanceWriterRunning {
		t.Fatal("active writer looked stopped", state, err)
	}
	stopWriterFixture(t, c, pod, job)
	if state, err := s.WriterState(ctx, status.UID, status.Version); err != nil || state != MaintenanceWriterStopped {
		t.Fatal("known termination not recognized", state, err)
	}
	if err := reservation.Check(ctx, MaintenanceUpgrade); err != nil {
		t.Fatal("inspection changed reservation", err)
	}
	if err := reservation.Release(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.WriterState(ctx, status.UID, status.Version); !errors.Is(err, ErrConflict) {
		t.Fatal("old inspection survived release", err)
	}
}
func TestMaintenanceWriterNeverTrustsAgeMissingOrUnknownTermination(t *testing.T) {
	for _, scenario := range []string{"missing-pod", "replaced-pod", "node-loss", "missing-container-status", "still-active-job", "extra-running-pod"} {
		t.Run(scenario, func(t *testing.T) {
			s, c, pod, job := writerFixture(t)
			ctx := context.Background()
			if _, err := s.AcquireForPod(ctx, MaintenanceUpgrade, pod.Name, string(pod.UID)); err != nil {
				t.Fatal(err)
			}
			status, _ := s.Inspect(ctx)
			stopWriterFixture(t, c, pod, job)
			switch scenario {
			case "missing-pod":
				if err := c.Delete(ctx, pod); err != nil {
					t.Fatal(err)
				}
			case "replaced-pod":
				if err := c.Delete(ctx, pod); err != nil {
					t.Fatal(err)
				}
				pod.ResourceVersion = ""
				pod.UID = types.UID(uuid.NewString())
				if err := c.Create(ctx, pod); err != nil {
					t.Fatal(err)
				}
			case "node-loss":
				pod.Status.ContainerStatuses[0].State.Terminated.Reason = "ContainerStatusUnknown"
				if err := c.Status().Update(ctx, pod); err != nil {
					t.Fatal(err)
				}
			case "missing-container-status":
				pod.Status.ContainerStatuses = nil
				if err := c.Status().Update(ctx, pod); err != nil {
					t.Fatal(err)
				}
			case "still-active-job":
				job.Status.Active = 1
				if err := c.Status().Update(ctx, job); err != nil {
					t.Fatal(err)
				}
			case "extra-running-pod":
				other := pod.DeepCopy()
				other.Name = "other-writer"
				other.UID = types.UID(uuid.NewString())
				other.ResourceVersion = ""
				other.Status = corev1.PodStatus{Phase: corev1.PodRunning}
				if err := c.Create(ctx, other); err != nil {
					t.Fatal(err)
				}
			}
			state, _ := s.WriterState(ctx, status.UID, status.Version)
			if state == MaintenanceWriterStopped {
				t.Fatal("uncertain writer considered stopped")
			}
			current, err := s.Inspect(ctx)
			if err != nil || !current.Present || current.Version != status.Version {
				t.Fatal("evidence check changed block", err)
			}
		})
	}
}
func TestMaintenanceWriterRejectsWrongIdentityAndRetries(t *testing.T) {
	for _, scenario := range []string{"wrong-pod-uid", "wrong-job-uid", "retrying-job", "parallel-job", "restarting-pod", "wrong-container", "missing-selector", "container-restart", "custom-controller", "wrapper-command"} {
		t.Run(scenario, func(t *testing.T) {
			s, c, pod, job := writerFixture(t)
			ctx := context.Background()
			uid := string(pod.UID)
			switch scenario {
			case "wrong-pod-uid":
				uid = uuid.NewString()
			case "wrong-job-uid":
				pod.OwnerReferences[0].UID = types.UID(uuid.NewString())
				if err := c.Update(ctx, pod); err != nil {
					t.Fatal(err)
				}
			case "retrying-job":
				one := int32(1)
				job.Spec.BackoffLimit = &one
				if err := c.Update(ctx, job); err != nil {
					t.Fatal(err)
				}
			case "parallel-job":
				two := int32(2)
				job.Spec.Parallelism = &two
				if err := c.Update(ctx, job); err != nil {
					t.Fatal(err)
				}
			case "restarting-pod":
				pod.Spec.RestartPolicy = corev1.RestartPolicyAlways
				if err := c.Update(ctx, pod); err != nil {
					t.Fatal(err)
				}
			case "wrong-container":
				pod.Spec.Containers[0].Name = "other"
				if err := c.Update(ctx, pod); err != nil {
					t.Fatal(err)
				}
			case "container-restart":
				policy := corev1.ContainerRestartPolicyAlways
				pod.Spec.Containers[0].RestartPolicy = &policy
				if err := c.Update(ctx, pod); err != nil {
					t.Fatal(err)
				}
			case "custom-controller":
				controller := "example.com/custom-job-controller"
				job.Spec.ManagedBy = &controller
				if err := c.Update(ctx, job); err != nil {
					t.Fatal(err)
				}
			case "wrapper-command":
				pod.Spec.Containers[0].Command = []string{"sh", "-c", "start-upgrade"}
				if err := c.Update(ctx, pod); err != nil {
					t.Fatal(err)
				}
			case "missing-selector":
				job.Spec.Selector = nil
				if err := c.Update(ctx, job); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.AcquireForPod(ctx, MaintenanceUpgrade, pod.Name, uid); err == nil {
				t.Fatal("invalid writer acquired reservation")
			}
			status, err := s.Inspect(ctx)
			if err != nil || status.Present {
				t.Fatal("refusal created reservation", err)
			}
		})
	}
}
func TestMaintenanceWriterWithoutReceiptCannotGainRecoveryEvidence(t *testing.T) {
	s, _ := fixtureMaintenance(t, false)
	ctx := context.Background()
	if _, err := s.Acquire(ctx, MaintenanceUpgrade); err != nil {
		t.Fatal(err)
	}
	status, err := s.Inspect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if state, err := s.WriterState(ctx, status.UID, status.Version); err != nil || state != MaintenanceWriterUnverified {
		t.Fatal("unidentified operation gained shutdown proof", state, err)
	}
}
func TestMaintenanceWriterCodecRejectsAmbiguousMetadata(t *testing.T) {
	writer := MaintenanceWriter{PodName: "writer-pod", PodUID: uuid.NewString(), JobName: "writer-job", JobUID: uuid.NewString(), Container: "upgrade"}
	raw, err := encodeMaintenanceWriter(writer)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeMaintenanceWriter(raw); err != nil {
		t.Fatal(err)
	}
	for _, bad := range [][]byte{append(raw, []byte(" ")...), []byte(`{"podName":"a","podName":"b"}`), []byte(`{"private":"must-not-appear"}`)} {
		if _, err := decodeMaintenanceWriter(bad); !errors.Is(err, ErrUnsupported) {
			t.Fatal("ambiguous writer metadata accepted", err)
		}
	}
}

func TestMaintenanceWriterBindsAdoptionJobSeparately(t *testing.T) {
	store, c, pod, job := writerFixture(t)
	ctx := context.Background()
	for _, spec := range []*corev1.PodSpec{&pod.Spec, &job.Spec.Template.Spec} {
		spec.Containers[0].Name = "adopt-credentials"
		spec.Containers[0].Command = []string{"/usr/local/bin/kyber-credential-adopt"}
	}
	if err := c.Update(ctx, pod); err != nil {
		t.Fatal(err)
	}
	if err := c.Update(ctx, job); err != nil {
		t.Fatal(err)
	}
	authority, err := newAuthorityStore(store.direct, "authority-fixture")
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := authority.ReserveAdoptionForPod(ctx, pod.Name, string(pod.UID))
	if err != nil {
		t.Fatal(err)
	}
	if err := reservation.Check(ctx, MaintenanceAdoption); err != nil {
		t.Fatal(err)
	}
	if err := reservation.Check(ctx, MaintenanceUpgrade); !errors.Is(err, ErrInvalid) {
		t.Fatal("writer kind boundary lost", err)
	}
	if err := reservation.Release(ctx); err != nil {
		t.Fatal(err)
	}
}

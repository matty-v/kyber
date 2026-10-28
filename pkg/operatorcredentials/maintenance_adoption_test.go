package operatorcredentials

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync"
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

func adoptionWriterPair(t *testing.T, c client.Client, operation, suffix string) (*corev1.Pod, *batchv1.Job) {
	t.Helper()
	ctx := context.Background()
	jobID, podID := types.UID(uuid.NewString()), types.UID(uuid.NewString())
	zero := int32(0)
	controller := true
	labels := map[string]string{"batch.kubernetes.io/controller-uid": string(jobID)}
	spec := corev1.PodSpec{NodeName: "fixture-node", RestartPolicy: corev1.RestartPolicyNever, Containers: []corev1.Container{{Name: "adopt-credentials", Image: "adoption-test-image", Command: []string{"/usr/local/bin/kyber-credential-adopt"}}}}
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Namespace: "kyber-system", Name: "adopt-" + suffix, UID: jobID, Labels: map[string]string{AdoptionOperationLabel: operation}}, Spec: batchv1.JobSpec{BackoffLimit: &zero, Selector: &metav1.LabelSelector{MatchLabels: labels}, Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}, Spec: spec}}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "kyber-system", Name: "adopt-pod-" + suffix, UID: podID, Labels: labels, OwnerReferences: []metav1.OwnerReference{{APIVersion: "batch/v1", Kind: "Job", Name: job.Name, UID: jobID, Controller: &controller}}}, Spec: spec, Status: corev1.PodStatus{Phase: corev1.PodRunning}}
	for _, object := range []client.Object{job, pod} {
		if c.Create(ctx, object) != nil {
			t.Fatal("writer fixture create failed")
		}
	}
	return pod, job
}
func adoptionResumeFixture(t *testing.T) (*MaintenanceStore, client.Client, string, *corev1.Pod, *batchv1.Job, *MaintenanceReservation) {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = batchv1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1.Pod{}, &batchv1.Job{}).Build()
	direct, _ := newSecretStore(c, maintenanceClient{Client: c}, "kyber-system", "registry-fixture")
	store, _ := newMaintenanceStore(direct, "maintenance-fixture")
	operation := uuid.NewString()
	pod, job := adoptionWriterPair(t, c, operation, "original")
	original, err := store.AcquireAdoptionForPod(context.Background(), operation, pod.Name, string(pod.UID))
	if err != nil {
		t.Fatal(err)
	}
	return store, c, operation, pod, job, original
}
func stopAdoptionWriter(t *testing.T, c client.Client, pod *corev1.Pod, job *batchv1.Job) {
	t.Helper()
	ctx := context.Background()
	pod.Status.Phase = corev1.PodFailed
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "adopt-credentials", ContainerID: "containerd://test", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Error", ExitCode: 1, ContainerID: "containerd://test", FinishedAt: metav1.NewTime(time.Now())}}}}
	if c.Status().Update(ctx, pod) != nil {
		t.Fatal("writer status fixture failed")
	}
	if c.Get(ctx, client.ObjectKeyFromObject(job), job) != nil {
		t.Fatal("Job fixture unavailable")
	}
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue}}
	if c.Status().Update(ctx, job) != nil {
		t.Fatal("Job status fixture failed")
	}
}
func TestAdoptionResumeFencesOriginalAndTracksLiveReplacement(t *testing.T) {
	s, c, operation, pod, job, original := adoptionResumeFixture(t)
	ctx := context.Background()
	status, _ := s.Inspect(ctx)
	next, nextJob := adoptionWriterPair(t, c, operation, "replacement")
	if _, err := s.ResumeAdoptionForPod(ctx, status.UID, status.Version, operation, next.Name, string(next.UID)); !errors.Is(err, ErrConflict) {
		t.Fatal("running writer displaced", err)
	}
	before := &corev1.Secret{}
	if c.Get(ctx, s.key, before) != nil {
		t.Fatal("reservation missing")
	}
	stopAdoptionWriter(t, c, pod, job)
	resumed, err := s.ResumeAdoptionForPod(ctx, status.UID, status.Version, operation, next.Name, string(next.UID))
	if err != nil {
		t.Fatal(err)
	}
	after := &corev1.Secret{}
	if c.Get(ctx, s.key, after) != nil || after.UID != before.UID || !reflect.DeepEqual(after.Data, before.Data) {
		t.Fatal("immutable reservation data changed")
	}
	if err := original.Check(ctx, MaintenanceAdoption); !errors.Is(err, ErrConflict) {
		t.Fatal("original writer was not fenced", err)
	}
	if resumed.Check(ctx, MaintenanceAdoption) != nil {
		t.Fatal("replacement did not own exact reservation")
	}
	current, err := s.Inspect(ctx)
	if err != nil || current.Writer == nil || current.Writer.PodUID != string(next.UID) || current.ExecutionID == status.ExecutionID || current.RecoveryOperationID != operation || current.AdoptionAdmissionVersion != status.Version {
		t.Fatal("active writer projection stale", err)
	}
	if state, err := s.WriterState(ctx, current.UID, current.Version); err != nil || state != MaintenanceWriterRunning {
		t.Fatal("replacement falsely appears stopped", state, err)
	}
	third, _ := adoptionWriterPair(t, c, operation, "third")
	if _, err := s.ResumeAdoptionForPod(ctx, current.UID, current.Version, operation, third.Name, string(third.UID)); !errors.Is(err, ErrConflict) {
		t.Fatal("live replacement displaced", err)
	}
	if _, err := json.Marshal(resumed); !errors.Is(err, ErrSensitive) {
		t.Fatal("private replacement handle serialized")
	}
	if err := resumed.Release(ctx); err != nil {
		t.Fatal(err)
	}
	for _, j := range []*batchv1.Job{job, nextJob} {
		got := &batchv1.Job{}
		if c.Get(ctx, client.ObjectKeyFromObject(j), got) != nil || got.Spec.TTLSecondsAfterFinished == nil || *got.Spec.TTLSecondsAfterFinished != maintenanceWriterCleanupTTL {
			t.Fatal("writer evidence cleanup not scheduled after confirmed release")
		}
	}
}
func TestAdoptionResumeReconcilesOnlyOwnLostAcknowledgements(t *testing.T) {
	s, c, operation, pod, job, _ := adoptionResumeFixture(t)
	ctx := context.Background()
	stopAdoptionWriter(t, c, pod, job)
	status, _ := s.Inspect(ctx)
	next, _ := adoptionWriterPair(t, c, operation, "replacement")
	writer := &checkpointUpdateClient{Client: c, lostAck: true}
	s.direct.writer = writer
	resumed, err := s.ResumeAdoptionForPod(ctx, status.UID, status.Version, operation, next.Name, string(next.UID))
	if err != nil || resumed == nil || writer.writes != 2 {
		t.Fatal("lost claim/fence acknowledgement replayed or unowned", err)
	}
	if resumed.Check(ctx, MaintenanceAdoption) != nil {
		t.Fatal("reconciled handle unusable")
	}
}
func TestAdoptionResumeConcurrentReplacementsHaveOneWinner(t *testing.T) {
	s, c, operation, pod, job, _ := adoptionResumeFixture(t)
	ctx := context.Background()
	stopAdoptionWriter(t, c, pod, job)
	status, _ := s.Inspect(ctx)
	one, _ := adoptionWriterPair(t, c, operation, "one")
	two, _ := adoptionWriterPair(t, c, operation, "two")
	var workers sync.WaitGroup
	results := make(chan error, 2)
	for _, next := range []*corev1.Pod{one, two} {
		workers.Go(func() {
			_, err := s.ResumeAdoptionForPod(ctx, status.UID, status.Version, operation, next.Name, string(next.UID))
			results <- err
		})
	}
	workers.Wait()
	close(results)
	winners := 0
	for err := range results {
		if err == nil {
			winners++
		} else if !errors.Is(err, ErrConflict) {
			t.Fatal("unexpected replacement conflict", err)
		}
	}
	if winners != 1 {
		t.Fatal("concurrent replacements did not have one winner")
	}
}
func TestAdoptionResumeRejectsChangedOperationStaleVersionAndMissingWriter(t *testing.T) {
	for _, scenario := range []string{"operation", "version", "missing", "node-lost"} {
		t.Run(scenario, func(t *testing.T) {
			s, c, operation, pod, job, _ := adoptionResumeFixture(t)
			ctx := context.Background()
			stopAdoptionWriter(t, c, pod, job)
			status, _ := s.Inspect(ctx)
			next, _ := adoptionWriterPair(t, c, operation, "replacement")
			switch scenario {
			case "operation":
				operation = uuid.NewString()
			case "version":
				status.Version = "stale"
			case "missing":
				if c.Delete(ctx, pod) != nil {
					t.Fatal("fixture delete failed")
				}
			case "node-lost":
				pod.Status.Reason = "NodeLost"
				if c.Status().Update(ctx, pod) != nil {
					t.Fatal("fixture status failed")
				}
			}
			if _, err := s.ResumeAdoptionForPod(ctx, status.UID, status.Version, operation, next.Name, string(next.UID)); err == nil {
				t.Fatal("unverified recovery permitted")
			}
		})
	}
}

func TestAdoptionRepeatedRecoveryKeepsOriginalAdmissionVersion(t *testing.T) {
	s, c, operation, pod, job, _ := adoptionResumeFixture(t)
	ctx := context.Background()
	initial, _ := s.Inspect(ctx)
	stopAdoptionWriter(t, c, pod, job)
	one, oneJob := adoptionWriterPair(t, c, operation, "one")
	first, err := s.ResumeAdoptionForPod(ctx, initial.UID, initial.Version, operation, one.Name, string(one.UID))
	if err != nil {
		t.Fatal(err)
	}
	stopAdoptionWriter(t, c, one, oneJob)
	middle, _ := s.Inspect(ctx)
	two, _ := adoptionWriterPair(t, c, operation, "two")
	if _, err := s.ResumeAdoptionForPod(ctx, middle.UID, middle.Version, operation, two.Name, string(two.UID)); err != nil {
		t.Fatal(err)
	}
	latest, err := s.Inspect(ctx)
	if err != nil || latest.AdoptionAdmissionVersion != initial.Version || latest.AdoptionAdmissionVersion == latest.Version {
		t.Fatal("recovery rebased the original journal admission version", err)
	}
	if !errors.Is(first.Check(ctx, MaintenanceAdoption), ErrConflict) {
		t.Fatal("previous replacement retained ownership")
	}
}

func TestAdoptionWriterReceiptRejectsUnknownAndIncompleteFields(t *testing.T) {
	record := adoptionWriterRecord{AdmissionVersion: "original-version", OperationID: uuid.NewString(), ExecutionID: uuid.NewString(), Writer: MaintenanceWriter{PodName: "pod-test", PodUID: uuid.NewString(), JobName: "job-test", JobUID: uuid.NewString(), Container: "adopt-credentials"}}
	raw, _ := json.Marshal(record)
	if _, err := decodeAdoptionWriter(string(raw)); err != nil {
		t.Fatal("valid receipt rejected")
	}
	for _, bad := range []string{string(raw) + " ", string(raw[:len(raw)-1]) + `,"unknown":true}`, `{}`, `{"admissionVersion":""}`} {
		if _, err := decodeAdoptionWriter(bad); !errors.Is(err, ErrUnsupported) {
			t.Fatal("ambiguous writer receipt accepted")
		}
	}
	record.AdmissionVersion = ""
	raw, _ = json.Marshal(record)
	if _, err := decodeAdoptionWriter(string(raw)); !errors.Is(err, ErrUnsupported) {
		t.Fatal("missing original admission version accepted")
	}
}

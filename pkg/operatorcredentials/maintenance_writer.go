package operatorcredentials

import (
	"bytes"
	"context"
	"encoding/json"

	"github.com/google/uuid"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	validation "k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// MaintenanceWriter identifies a supervised execution, never an authorization
// capability. Namespace comes exclusively from the installation-bound store.
type MaintenanceWriter struct {
	PodName   string `json:"podName"`
	PodUID    string `json:"podUid"`
	JobName   string `json:"jobName"`
	JobUID    string `json:"jobUid"`
	Container string `json:"container"`
}

func canonicalWriterUID(value string) bool {
	id, err := uuid.Parse(value)
	return err == nil && id != uuid.Nil && id.String() == value
}
func (w MaintenanceWriter) valid() bool {
	return len(validation.IsDNS1123Subdomain(w.PodName)) == 0 && len(validation.IsDNS1123Subdomain(w.JobName)) == 0 && canonicalWriterUID(w.PodUID) && canonicalWriterUID(w.JobUID) && (w.Container == "upgrade" || w.Container == "adopt-credentials")
}
func encodeMaintenanceWriter(writer MaintenanceWriter) ([]byte, error) {
	if !writer.valid() {
		return nil, ErrInvalid
	}
	return json.Marshal(writer)
}
func decodeMaintenanceWriter(raw []byte) (MaintenanceWriter, error) {
	var writer MaintenanceWriter
	if len(raw) == 0 || len(raw) > 1024 || json.Unmarshal(raw, &writer) != nil {
		return MaintenanceWriter{}, ErrUnsupported
	}
	canonical, err := encodeMaintenanceWriter(writer)
	// Exact canonical encoding also rejects duplicate/unknown fields and trailing
	// data rather than allowing different readers to interpret one receipt twice.
	if err != nil || !bytes.Equal(canonical, raw) {
		return MaintenanceWriter{}, ErrUnsupported
	}
	return writer, nil
}
func maintenanceWriterContainer(kind string) string {
	switch kind {
	case MaintenanceUpgrade:
		return "upgrade"
	case MaintenanceAdoption:
		return "adopt-credentials"
	default:
		return ""
	}
}
func singleRunJob(job *batchv1.Job) bool {
	return job.Spec.BackoffLimit != nil && *job.Spec.BackoffLimit == 0 && job.Spec.BackoffLimitPerIndex == nil && job.Spec.PodFailurePolicy == nil && job.Spec.SuccessPolicy == nil && (job.Spec.ManagedBy == nil || *job.Spec.ManagedBy == "kubernetes.io/job-controller") && (job.Spec.CompletionMode == nil || *job.Spec.CompletionMode == batchv1.NonIndexedCompletion) && (job.Spec.Parallelism == nil || *job.Spec.Parallelism == 1) && (job.Spec.Completions == nil || *job.Spec.Completions == 1) && (job.Spec.Suspend == nil || !*job.Spec.Suspend) && job.Spec.Template.Spec.RestartPolicy == corev1.RestartPolicyNever
}
func writerPodSpec(spec corev1.PodSpec, container string) bool {
	if spec.RestartPolicy != corev1.RestartPolicyNever || len(spec.Containers) != 1 || len(spec.InitContainers) != 0 || len(spec.EphemeralContainers) != 0 {
		return false
	}
	c := spec.Containers[0]
	command := ""
	switch container {
	case "upgrade":
		command = "/usr/local/bin/kyber-upgrade"
	case "adopt-credentials":
		command = "/usr/local/bin/kyber-credential-adopt"
	default:
		return false
	}
	return c.Name == container && c.RestartPolicy == nil && len(c.RestartPolicyRules) == 0 && len(c.Command) == 1 && c.Command[0] == command && len(c.Args) == 0
}

func writerJobOwner(pod *corev1.Pod) *metav1.OwnerReference {
	if len(pod.OwnerReferences) != 1 {
		return nil
	}
	owner := &pod.OwnerReferences[0]
	if owner.APIVersion != "batch/v1" || owner.Kind != "Job" || owner.Controller == nil || !*owner.Controller || !canonicalWriterUID(string(owner.UID)) || len(validation.IsDNS1123Subdomain(owner.Name)) != 0 {
		return nil
	}
	return owner
}

// AcquireForPod records the exact non-retrying Job writer from direct reads.
// Caller-supplied names/UIDs locate its own downward-API identity; Job ownership
// and execution shape are verified, never copied from request metadata.
func (s *MaintenanceStore) AcquireForPod(ctx context.Context, kind, podName, podUID string) (*MaintenanceReservation, error) {
	container := maintenanceWriterContainer(kind)
	if s == nil || s.direct == nil || container == "" || len(validation.IsDNS1123Subdomain(podName)) != 0 || !canonicalWriterUID(podUID) {
		return nil, ErrInvalid
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	pod := &corev1.Pod{}
	if err := s.direct.reader.Get(ctx, types.NamespacedName{Namespace: s.key.Namespace, Name: podName}, pod); err != nil {
		return nil, authorityUnavailable(ctx, err)
	}
	owner := writerJobOwner(pod)
	if string(pod.UID) != podUID || pod.Namespace != s.key.Namespace || pod.Spec.NodeName == "" || pod.DeletionTimestamp != nil || owner == nil || !writerPodSpec(pod.Spec, container) || pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
		return nil, ErrUnsupported
	}
	job := &batchv1.Job{}
	if err := s.direct.reader.Get(ctx, types.NamespacedName{Namespace: s.key.Namespace, Name: owner.Name}, job); err != nil {
		return nil, authorityUnavailable(ctx, err)
	}
	if job.UID != owner.UID || job.Namespace != s.key.Namespace || job.DeletionTimestamp != nil || job.Spec.Selector == nil || len(job.Spec.Selector.MatchExpressions) != 0 || job.Spec.Selector.MatchLabels["batch.kubernetes.io/controller-uid"] != string(job.UID) || pod.Labels["batch.kubernetes.io/controller-uid"] != string(job.UID) || !singleRunJob(job) || !writerPodSpec(job.Spec.Template.Spec, container) || job.Spec.Template.Spec.Containers[0].Image != pod.Spec.Containers[0].Image {
		return nil, ErrUnsupported
	}
	for _, condition := range job.Status.Conditions {
		if condition.Status == corev1.ConditionTrue && (condition.Type == batchv1.JobComplete || condition.Type == batchv1.JobFailed) {
			return nil, ErrUnsupported
		}
	}
	writer := MaintenanceWriter{PodName: pod.Name, PodUID: podUID, JobName: job.Name, JobUID: string(job.UID), Container: container}
	return s.acquire(ctx, kind, &writer)
}

const (
	MaintenanceWriterUnverified = "unverified"
	MaintenanceWriterRunning    = "running"
	MaintenanceWriterStopped    = "stopped"
)

// WriterState is only shutdown evidence. It cannot release maintenance or prove
// credential/Helm effects reconciled. Missing/replaced objects remain unverified.
// The inspected reservation UID/RV must still match before any evidence is used.
func (s *MaintenanceStore) WriterState(ctx context.Context, uid, version string) (string, error) {
	if s == nil || uid == "" || version == "" {
		return MaintenanceWriterUnverified, ErrInvalid
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	current, err := s.Inspect(ctx)
	if err != nil {
		return MaintenanceWriterUnverified, err
	}
	if !current.Present || current.UID != uid || current.Version != version {
		return MaintenanceWriterUnverified, ErrConflict
	}
	if current.Writer == nil {
		return MaintenanceWriterUnverified, nil
	}
	writer := current.Writer
	pod := &corev1.Pod{}
	if err := s.direct.reader.Get(ctx, types.NamespacedName{Namespace: s.key.Namespace, Name: writer.PodName}, pod); err != nil {
		return MaintenanceWriterUnverified, authorityUnavailable(ctx, err)
	}
	owner := writerJobOwner(pod)
	if string(pod.UID) != writer.PodUID || owner == nil || owner.Name != writer.JobName || string(owner.UID) != writer.JobUID {
		return MaintenanceWriterUnverified, nil
	}
	if !knownTerminatedWriterPod(pod, writer.Container) {
		if pod.Status.Phase == corev1.PodRunning || pod.Status.Phase == corev1.PodPending {
			return MaintenanceWriterRunning, nil
		}
		return MaintenanceWriterUnverified, nil
	}
	job := &batchv1.Job{}
	if err := s.direct.reader.Get(ctx, types.NamespacedName{Namespace: s.key.Namespace, Name: writer.JobName}, job); err != nil {
		return MaintenanceWriterUnverified, authorityUnavailable(ctx, err)
	}
	if string(job.UID) != writer.JobUID || !singleRunJob(job) || job.Status.Active != 0 {
		return MaintenanceWriterUnverified, nil
	}
	terminal := false
	for _, condition := range job.Status.Conditions {
		if condition.Status == corev1.ConditionTrue && (condition.Type == batchv1.JobComplete || condition.Type == batchv1.JobFailed) {
			terminal = true
		}
	}
	if !terminal || job.Spec.Selector == nil || len(job.Spec.Selector.MatchExpressions) != 0 || job.Spec.Selector.MatchLabels["batch.kubernetes.io/controller-uid"] != writer.JobUID {
		return MaintenanceWriterUnverified, nil
	}
	pods := &corev1.PodList{}
	if err := s.direct.reader.List(ctx, pods, client.InNamespace(s.key.Namespace), client.MatchingLabels{"batch.kubernetes.io/controller-uid": writer.JobUID}, client.Limit(257)); err != nil {
		return MaintenanceWriterUnverified, authorityUnavailable(ctx, err)
	}
	if len(pods.Items) > 256 || pods.Continue != "" {
		return MaintenanceWriterUnverified, nil
	}
	found := false
	for i := range pods.Items {
		candidate := &pods.Items[i]
		candidateOwner := writerJobOwner(candidate)
		if candidateOwner == nil || string(candidateOwner.UID) != writer.JobUID || !knownTerminatedWriterPod(candidate, writer.Container) {
			return MaintenanceWriterUnverified, nil
		}
		found = found || string(candidate.UID) == writer.PodUID
	}
	if !found {
		return MaintenanceWriterUnverified, nil
	}
	// Recheck the inspected reservation after the bounded writer observations.
	latest, err := s.Inspect(ctx)
	if err != nil {
		return MaintenanceWriterUnverified, err
	}
	if !latest.Present || latest.UID != uid || latest.Version != version {
		return MaintenanceWriterUnverified, ErrConflict
	}
	return MaintenanceWriterStopped, nil
}
func knownTerminatedWriterPod(pod *corev1.Pod, container string) bool {
	if pod.Status.Reason == "NodeLost" || pod.Status.Reason == "NodeShutdown" || pod.Status.Reason == "Shutdown" {
		return false
	}
	if pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed {
		return false
	}
	if !writerPodSpec(pod.Spec, container) || len(pod.Status.ContainerStatuses) != 1 {
		return false
	}
	status := pod.Status.ContainerStatuses[0]
	terminated := status.State.Terminated
	if status.Name != container || status.RestartCount != 0 || terminated == nil || terminated.FinishedAt.IsZero() || terminated.ContainerID == "" || terminated.ContainerID != status.ContainerID {
		return false
	}
	// Node-loss/controller-invented unknown states are not process-death proof.
	switch terminated.Reason {
	case "Completed", "Error", "OOMKilled":
		return true
	default:
		return false
	}
}

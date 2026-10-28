package operatorcredentials

import (
	"context"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// SupervisedJobStopped is read-only process evidence for adoption preflight.
// The caller supplies a direct reader and separately discovers the complete
// installation job/pod set. Missing evidence and node-loss states are not proof.
// Unlike reservation-bound WriterState, a pre-protocol finished upgrade need not
// have an execution annotation; all of its exact non-retrying Pods must be known
// terminated and its unchanged Job must be terminal.
func SupervisedJobStopped(ctx context.Context, reader client.Reader, job batchv1.Job, container string) (bool, error) {
	if reader == nil || job.Namespace == "" || job.Name == "" || !canonicalWriterUID(string(job.UID)) || job.ResourceVersion == "" || job.DeletionTimestamp != nil || !singleRunJob(&job) || !writerPodSpec(job.Spec.Template.Spec, container) || job.Status.Active != 0 || job.Spec.Selector == nil || len(job.Spec.Selector.MatchExpressions) != 0 || job.Spec.Selector.MatchLabels["batch.kubernetes.io/controller-uid"] != string(job.UID) {
		return false, nil
	}
	terminal := false
	for _, condition := range job.Status.Conditions {
		if condition.Status == corev1.ConditionTrue && (condition.Type == batchv1.JobComplete || condition.Type == batchv1.JobFailed) {
			terminal = true
		}
	}
	if !terminal {
		return false, nil
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	var pods corev1.PodList
	if err := reader.List(ctx, &pods, client.InNamespace(job.Namespace), client.MatchingLabels{"batch.kubernetes.io/controller-uid": string(job.UID)}, client.Limit(257)); err != nil {
		return false, ErrUnavailable
	}
	if len(pods.Items) == 0 || len(pods.Items) > 256 || pods.Continue != "" {
		return false, nil
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		owner := metav1.GetControllerOf(pod)
		if owner == nil || owner.APIVersion != "batch/v1" || owner.Kind != "Job" || owner.UID != job.UID || owner.Name != job.Name || pod.Namespace != job.Namespace || !canonicalWriterUID(string(pod.UID)) || pod.Spec.NodeName == "" || !knownTerminatedWriterPod(pod, container) || pod.Spec.Containers[0].Image != job.Spec.Template.Spec.Containers[0].Image {
			return false, nil
		}
	}
	latest := &batchv1.Job{}
	if err := reader.Get(ctx, client.ObjectKeyFromObject(&job), latest); err != nil {
		return false, ErrUnavailable
	}
	if ctx.Err() != nil || latest.UID != job.UID || latest.ResourceVersion != job.ResourceVersion {
		return false, nil
	}
	return true, nil
}

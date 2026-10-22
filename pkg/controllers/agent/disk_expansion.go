package agent

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"time"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const diskExpansionPoll = 10 * time.Second
const diskGiB = int64(1 << 30)

type diskCapacityMeasurement struct {
	TotalBytes     int64 `json:"totalBytes"`
	AvailableBytes int64 `json:"availableBytes"`
}

// filesystemExpanded accounts for filesystem metadata, which makes statfs
// total smaller than the block device. GKE ext4 reported 11,516,715,008
// bytes on a successfully expanded 11 GiB PVC (11,811,160,064 bytes).
// Callers also require PVC status.capacity to reach the target: this
// measurement alone cannot prove a small increase on a large disk.
func filesystemExpanded(total, target int64) bool {
	return total > 0 && target > 0 && total >= target-max(384*1024*1024, target*3/100)
}

func (r *AgentReconciler) updateDiskExpansion(ctx context.Context, agent *kyberv1.Agent, op *kyberv1.AgentDiskExpansionStatus) error {
	if reflect.DeepEqual(agent.Status.DiskExpansion, op) {
		return nil
	}
	before := agent.DeepCopy()
	agent.Status.DiskExpansion = op
	return r.Status().Patch(ctx, agent, client.MergeFrom(before))
}

func (r *AgentReconciler) reconcileDiskExpansion(ctx context.Context, agent *kyberv1.Agent, pod *corev1.Pod) (time.Duration, error) {
	op := agent.Status.DiskExpansion
	// The API writes spec and status through separate Kubernetes subresources.
	// Recover if reconciliation observes the spec first or the status write
	// loses a race; never let an accepted target depend on an API process.
	if op == nil || ((op.Stage == "Blocked" || op.Stage == "Complete") && op.TargetGiB*diskGiB != agent.Spec.Resources.Disk.Value()) {
		if agent.Status.Phase != "" && agent.Status.Phase != kyberv1.AgentPhaseCreating {
			pvc := &corev1.PersistentVolumeClaim{}
			if err := r.Get(ctx, types.NamespacedName{Namespace: agent.Namespace, Name: PVCName(agent.Name)}, pvc); err == nil && pvc.Status.Phase == corev1.ClaimBound {
				capacity := pvc.Status.Capacity[corev1.ResourceStorage]
				if agent.Spec.Resources.Disk.Cmp(capacity) > 0 {
					pv := &corev1.PersistentVolume{}
					if err := r.Get(ctx, types.NamespacedName{Name: pvc.Spec.VolumeName}, pv); err == nil && pv.Spec.ClaimRef != nil && pv.Spec.ClaimRef.UID == pvc.UID {
						now := metav1.Now()
						op = &kyberv1.AgentDiskExpansionStatus{RequestedAt: &now, TargetGiB: agent.Spec.Resources.Disk.Value() / diskGiB, PVCUID: pvc.UID, PVUID: pv.UID, Stage: "Requested"}
						if err := r.updateDiskExpansion(ctx, agent, op); err != nil {
							return diskExpansionPoll, err
						}
					}
				}
			}
		}
	}
	if op == nil || op.Stage == "Complete" || op.Stage == "Blocked" {
		return 0, nil
	}
	pvc := &corev1.PersistentVolumeClaim{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: agent.Namespace, Name: PVCName(agent.Name)}, pvc); err != nil {
		if apierrors.IsNotFound(err) {
			return diskExpansionPoll, r.blockDiskExpansion(ctx, agent, "Original Agent persist PVC is missing; no replacement was created")
		}
		return 0, err
	}
	if pvc.UID != op.PVCUID || pvc.Spec.VolumeName == "" {
		return diskExpansionPoll, r.blockDiskExpansion(ctx, agent, "Agent persist PVC changed; expansion stopped to protect data")
	}
	owned := false
	for _, owner := range pvc.OwnerReferences {
		if owner.Kind == "Agent" && owner.Name == agent.Name && owner.UID == agent.UID {
			owned = true
		}
	}
	if !owned {
		return 0, r.blockDiskExpansion(ctx, agent, "Agent persist PVC ownership changed; expansion stopped to protect data")
	}
	pv := &corev1.PersistentVolume{}
	if err := r.Get(ctx, types.NamespacedName{Name: pvc.Spec.VolumeName}, pv); err != nil {
		if apierrors.IsNotFound(err) {
			return diskExpansionPoll, r.blockDiskExpansion(ctx, agent, "Original persistent volume is missing")
		}
		return 0, err
	}
	if pv.UID != op.PVUID || pv.Spec.ClaimRef == nil || pv.Spec.ClaimRef.UID != pvc.UID {
		return diskExpansionPoll, r.blockDiskExpansion(ctx, agent, "Persistent volume identity changed; expansion stopped to protect data")
	}
	if pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName == "" {
		return 0, r.blockDiskExpansion(ctx, agent, "Agent volume has no expandable StorageClass")
	}
	storageClass := &storagev1.StorageClass{}
	if err := r.Get(ctx, types.NamespacedName{Name: *pvc.Spec.StorageClassName}, storageClass); err != nil {
		return diskExpansionPoll, r.blockDiskExpansion(ctx, agent, "Agent StorageClass is unavailable")
	}
	if storageClass.AllowVolumeExpansion == nil || !*storageClass.AllowVolumeExpansion ||
		(storageClass.Provisioner != "pd.csi.storage.gke.io" && storageClass.Provisioner != "ebs.csi.aws.com") ||
		pv.Spec.CSI == nil || pv.Spec.CSI.Driver != storageClass.Provisioner {
		return 0, r.blockDiskExpansion(ctx, agent, "Agent volume driver does not support verified in-place expansion")
	}
	targetBytes := op.TargetGiB * diskGiB
	if targetBytes <= 0 || targetBytes != agent.Spec.Resources.Disk.Value() {
		return diskExpansionPoll, r.blockDiskExpansion(ctx, agent, "Disk target no longer matches Agent allocation")
	}
	updated := *op
	request := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
	updated.PVCRequestBytes = request.Value()
	pvCapacity := pv.Spec.Capacity[corev1.ResourceStorage]
	updated.PVCapacityBytes = pvCapacity.Value()
	if request.Value() != targetBytes {
		claimCapacity := pvc.Status.Capacity[corev1.ResourceStorage]
		allocated := pvc.Status.AllocatedResources[corev1.ResourceStorage]
		if targetBytes <= max(pvCapacity.Value(), claimCapacity.Value(), allocated.Value()) {
			return 0, r.blockDiskExpansion(ctx, agent, "Requested target is below already allocated disk capacity")
		}
		before := pvc.DeepCopy()
		pvc.Spec.Resources.Requests[corev1.ResourceStorage] = *resource.NewQuantity(targetBytes, resource.BinarySI)
		if err := r.Patch(ctx, pvc, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			if apierrors.IsConflict(err) {
				return diskExpansionPoll, err
			}
			return diskExpansionPoll, r.blockDiskExpansion(ctx, agent, "Kubernetes rejected the PVC increase; inspect PVC events and storage quota")
		}
		updated.PVCRequestBytes = targetBytes
		updated.Stage = "Provisioning"
		return diskExpansionPoll, r.updateDiskExpansion(ctx, agent, &updated)
	}
	if (op.RequestedAt == nil || time.Since(op.RequestedAt.Time) > 30*time.Second) && diskResizeFailure(pvc) != "" {
		updated.Stage = "Blocked"
		updated.Reason = diskResizeFailure(pvc)
		return 0, r.updateDiskExpansion(ctx, agent, &updated)
	}
	capacity := pvc.Status.Capacity[corev1.ResourceStorage]
	// For an idle RWO volume, Kubernetes may leave PVC status.capacity at the
	// old size until a pod mounts it and kubelet performs the node resize.
	// Once the PV has grown, start the capacity helper below to provide that
	// mount; waiting for PVC status first would deadlock a stopped Agent.
	if updated.PVCapacityBytes < targetBytes || (pod != nil && capacity.Value() < targetBytes) {
		updated.Stage = "Provisioning"
		return diskExpansionPoll, r.updateDiskExpansion(ctx, agent, &updated)
	}
	updated.Stage = "Filesystem"
	if op.Stage == "Verifying" {
		updated.Stage = "Verifying"
	}
	if pod == nil && op.Stage == "Verifying" {
		if agent.Status.Phase == kyberv1.AgentPhaseDiskExhausted && agent.Spec.DesiredPhase != kyberv1.AgentPhaseRestarting {
			if _, err := r.requestDiskExpansionRestart(ctx, agent); err != nil {
				return diskExpansionPoll, err
			}
		}
		return diskExpansionPoll, nil
	}
	if pod == nil && (agent.Status.Phase == kyberv1.AgentPhaseStopped || agent.Status.Phase == kyberv1.AgentPhaseDiskExhausted ||
		agent.Status.Phase == kyberv1.AgentPhaseNeedsAuth || agent.Status.Phase == kyberv1.AgentPhaseFailed) &&
		(agent.Status.Phase != kyberv1.AgentPhaseStopped || agent.Spec.DesiredPhase != kyberv1.AgentPhaseRunning) {
		return r.reconcileStoppedDiskFilesystem(ctx, agent, pvc, &updated)
	}
	usage := agent.Status.Activity
	if usage != nil && usage.Resources != nil && usage.Resources.DiskUsageMethod == "statfs" &&
		(op.RequestedAt == nil || usage.Resources.SampledAt.After(op.RequestedAt.Time)) &&
		(pod == nil || pod.Status.StartTime == nil || usage.Resources.SampledAt.After(pod.Status.StartTime.Time)) {
		measured := usage.Resources
		updated.FilesystemTotalBytes = measured.DiskBackingTotalBytes
		updated.FilesystemAvailableBytes = measured.DiskBackingAvailableBytes
		when := measured.SampledAt
		updated.FilesystemSampledAt = &when
		if filesystemExpanded(measured.DiskBackingTotalBytes, targetBytes) {
			if pod != nil && (agent.Status.Phase == kyberv1.AgentPhaseRunning || agent.Status.Phase == kyberv1.AgentPhaseDiskExhausted) && measured.DiskTotalBytes < targetBytes {
				if op.RestartRequestedAt == nil {
					requested, err := r.requestDiskExpansionRestart(ctx, agent)
					if err != nil {
						return diskExpansionPoll, err
					}
					if requested {
						now := metav1.Now()
						updated.RestartRequestedAt = &now
					}
				}
				updated.Stage = "Restarting"
				return diskExpansionPoll, r.updateDiskExpansion(ctx, agent, &updated)
			}
			if pod != nil && agent.Status.Phase == kyberv1.AgentPhaseRunning && measured.DiskTotalBytes >= targetBytes &&
				(op.RestartRequestedAt == nil || measured.SampledAt.After(op.RestartRequestedAt.Time)) {
				updated.Stage = "Complete"
				updated.Reason = ""
				return 0, r.updateDiskExpansion(ctx, agent, &updated)
			}
		}
	}
	if op.RestartRequestedAt != nil {
		updated.Stage = "Restarting"
	}
	return diskExpansionPoll, r.updateDiskExpansion(ctx, agent, &updated)
}

func diskResizeFailure(pvc *corev1.PersistentVolumeClaim) string {
	switch pvc.Status.AllocatedResourceStatuses[corev1.ResourceStorage] {
	case corev1.PersistentVolumeClaimControllerResizeInfeasible:
		return "CSI driver reported that the volume cannot be expanded; inspect PVC events"
	case corev1.PersistentVolumeClaimNodeResizeInfeasible:
		return "Filesystem resize failed on the node; inspect PVC events"
	}
	for _, condition := range pvc.Status.Conditions {
		if condition.Status != corev1.ConditionTrue {
			continue
		}
		switch condition.Type {
		case corev1.PersistentVolumeClaimControllerResizeError:
			return "CSI driver reported a volume resize error; inspect PVC events"
		case corev1.PersistentVolumeClaimNodeResizeError:
			return "Filesystem resize reported an error; inspect PVC events"
		}
	}
	return ""
}

func (r *AgentReconciler) requestDiskExpansionRestart(ctx context.Context, agent *kyberv1.Agent) (bool, error) {
	if agent.Status.Phase == kyberv1.AgentPhaseRunning {
		return r.requestIntentionalRestart(ctx, agent)
	}
	if agent.Status.Phase != kyberv1.AgentPhaseDiskExhausted {
		return false, nil
	}
	current := &kyberv1.Agent{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(agent), current); err != nil {
		return false, err
	}
	if current.Status.Phase != kyberv1.AgentPhaseDiskExhausted ||
		(current.Spec.DesiredPhase != "" && current.Spec.DesiredPhase != kyberv1.AgentPhaseRunning) {
		return false, nil
	}
	before := current.DeepCopy()
	current.Spec.DesiredPhase = kyberv1.AgentPhaseRestarting
	if err := r.Patch(ctx, current, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
		return false, err
	}
	agent.Spec.DesiredPhase = kyberv1.AgentPhaseRestarting
	return true, nil
}

func (r *AgentReconciler) blockDiskExpansion(ctx context.Context, agent *kyberv1.Agent, reason string) error {
	if agent.Status.DiskExpansion == nil {
		return nil
	}
	updated := *agent.Status.DiskExpansion
	updated.Stage = "Blocked"
	updated.Reason = reason
	return r.updateDiskExpansion(ctx, agent, &updated)
}

func diskCapacityPodName(name string) string {
	const prefix = "disk-capacity-"
	if len(name) > 63-len(prefix) {
		name = strings.TrimRight(name[:63-len(prefix)], "-")
	}
	return prefix + name
}

func (r *AgentReconciler) reconcileStoppedDiskFilesystem(ctx context.Context, agent *kyberv1.Agent, pvc *corev1.PersistentVolumeClaim, op *kyberv1.AgentDiskExpansionStatus) (time.Duration, error) {
	if r.DiskMaintenanceImage == "" {
		op.Stage = "Blocked"
		op.Reason = "Disk capacity helper image is not configured"
		return 0, r.updateDiskExpansion(ctx, agent, op)
	}
	name := diskCapacityPodName(agent.Name)
	helper := &corev1.Pod{}
	err := r.Get(ctx, types.NamespacedName{Namespace: agent.Namespace, Name: name}, helper)
	if apierrors.IsNotFound(err) {
		created := diskCapacityPod(agent, pvc, name, r.DiskMaintenanceImage)
		if err := ctrl.SetControllerReference(agent, created, r.Scheme); err != nil {
			return diskExpansionPoll, err
		}
		if err := r.Create(ctx, created); err != nil && !apierrors.IsAlreadyExists(err) {
			return diskExpansionPoll, err
		}
		return diskExpansionPoll, r.updateDiskExpansion(ctx, agent, op)
	}
	if err != nil {
		return diskExpansionPoll, err
	}
	if helper.Annotations["kyber.io/disk-pvc-uid"] != string(pvc.UID) || !metav1.IsControlledBy(helper, agent) {
		op.Stage = "Blocked"
		op.Reason = "Disk capacity helper identity changed"
		return 0, r.updateDiskExpansion(ctx, agent, op)
	}
	if op.RequestedAt != nil && !helper.CreationTimestamp.IsZero() && helper.CreationTimestamp.Before(op.RequestedAt) {
		// A completed helper from the previous increase may still be
		// terminating when a new target is accepted. Never reuse its sample.
		if err := r.Delete(ctx, helper); err != nil && !apierrors.IsNotFound(err) {
			return diskExpansionPoll, err
		}
		return diskExpansionPoll, nil
	}
	if helper.Status.Phase == corev1.PodFailed {
		op.Stage = "Blocked"
		op.Reason = "Disk filesystem measurement failed; inspect helper Pod events"
		return 0, r.updateDiskExpansion(ctx, agent, op)
	}
	if helper.Status.Phase != corev1.PodSucceeded {
		return diskExpansionPoll, r.updateDiskExpansion(ctx, agent, op)
	}
	// The helper mount triggers kubelet's node resize. Its exit can be
	// observed before the PVC status update reaches the controller cache.
	// PVC capacity is the Kubernetes confirmation that filesystem resize
	// finished; wait for it before accepting the helper's statfs sample.
	capacity := pvc.Status.Capacity[corev1.ResourceStorage]
	if capacity.Value() < op.TargetGiB*diskGiB {
		return diskExpansionPoll, r.updateDiskExpansion(ctx, agent, op)
	}
	var report diskCapacityMeasurement
	for _, state := range helper.Status.ContainerStatuses {
		if state.Name == "capacity" && state.State.Terminated != nil {
			if err := json.Unmarshal([]byte(state.State.Terminated.Message), &report); err != nil {
				op.Stage = "Blocked"
				op.Reason = "Disk capacity helper returned an invalid measurement"
				return 0, r.updateDiskExpansion(ctx, agent, op)
			}
		}
	}
	if !filesystemExpanded(report.TotalBytes, op.TargetGiB*diskGiB) {
		op.Stage = "Blocked"
		op.Reason = "PVC grew but mounted filesystem has not reached the requested capacity"
		return 0, r.updateDiskExpansion(ctx, agent, op)
	}
	op.FilesystemTotalBytes = report.TotalBytes
	op.FilesystemAvailableBytes = report.AvailableBytes
	now := metav1.Now()
	op.FilesystemSampledAt = &now
	op.Stage = "Complete"
	if agent.Status.Phase == kyberv1.AgentPhaseDiskExhausted &&
		(agent.Spec.DesiredPhase == "" || agent.Spec.DesiredPhase == kyberv1.AgentPhaseRunning) {
		// The existing one-shot recovery gate starts a fresh Agent pod after
		// the filesystem is ready. Verify that pod's new sidecar before success.
		op.Stage = "Verifying"
	}
	op.Reason = ""
	if err := r.updateDiskExpansion(ctx, agent, op); err != nil {
		return diskExpansionPoll, err
	}
	if err := r.Delete(ctx, helper); err != nil && !apierrors.IsNotFound(err) {
		return diskExpansionPoll, err
	}
	return 0, nil
}

func diskCapacityPod(agent *kyberv1.Agent, pvc *corev1.PersistentVolumeClaim, name, image string) *corev1.Pod {
	nonRoot := int64(65532)
	no := false
	yes := true
	deadline := int64(180)
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: agent.Namespace, Annotations: map[string]string{"kyber.io/disk-pvc-uid": string(pvc.UID)}},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever, ActiveDeadlineSeconds: &deadline, AutomountServiceAccountToken: &no,
			Volumes: []corev1.Volume{{Name: "persist", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: pvc.Name}}}},
			Containers: []corev1.Container{{
				Name: "capacity", Image: image, Command: []string{"/usr/local/bin/kyber-disk-capacity", "/persist"},
				VolumeMounts:    []corev1.VolumeMount{{Name: "persist", MountPath: "/persist"}},
				SecurityContext: &corev1.SecurityContext{RunAsUser: &nonRoot, RunAsNonRoot: &yes, ReadOnlyRootFilesystem: &yes, AllowPrivilegeEscalation: &no, Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}},
				Resources:       corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("25m"), corev1.ResourceMemory: resource.MustParse("32Mi")}, Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("64Mi")}},
			}},
		},
	}
}

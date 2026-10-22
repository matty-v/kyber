package agent

import (
	"context"
	"testing"
	"time"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func diskExpansionClass() *storagev1.StorageClass {
	allow := true
	return &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "expandable-pd"}, Provisioner: "pd.csi.storage.gke.io", AllowVolumeExpansion: &allow}
}

func TestDiskExpansion_StoppedPreservesOriginalVolumeAndIntent(t *testing.T) {
	ctx := context.Background()
	scheme := buildTestScheme()
	agent := newTestAgent("echo", "kyber-system")
	agent.UID = types.UID("agent-original")
	agent.Spec.Resources.Disk = resource.MustParse("50Gi")
	agent.Spec.DesiredPhase = kyberv1.AgentPhaseStopped
	agent.Status.Phase = kyberv1.AgentPhaseStopped
	requested := metav1.Now()
	agent.Status.DiskExpansion = &kyberv1.AgentDiskExpansionStatus{TargetGiB: 50, PVCUID: types.UID("claim-original"), PVUID: types.UID("volume-original"), Stage: "Requested", RequestedAt: &requested}
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: PVCName(agent.Name), Namespace: agent.Namespace, UID: agent.Status.DiskExpansion.PVCUID},
		Spec:       corev1.PersistentVolumeClaimSpec{VolumeName: "pv-echo", StorageClassName: new(string), Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("20Gi")}}},
		Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound, Capacity: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("20Gi")}},
	}
	*pvc.Spec.StorageClassName = "expandable-pd"
	pv := &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: pvc.Spec.VolumeName, UID: agent.Status.DiskExpansion.PVUID},
		Spec: corev1.PersistentVolumeSpec{ClaimRef: &corev1.ObjectReference{UID: pvc.UID}, Capacity: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("20Gi")}, PersistentVolumeSource: corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{Driver: "pd.csi.storage.gke.io"}}}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(agent, pvc).WithObjects(agent, pvc, pv, diskExpansionClass()).Build()
	r := &AgentReconciler{Client: c, Scheme: scheme, DiskMaintenanceImage: "kyber-control-plane:test"}
	if _, err := r.reconcileDiskExpansion(ctx, agent, nil); err != nil {
		t.Fatal(err)
	}
	claim := &corev1.PersistentVolumeClaim{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(pvc), claim); err != nil {
		t.Fatal(err)
	}
	claimSize := claim.Spec.Resources.Requests[corev1.ResourceStorage]
	if claim.UID != pvc.UID || claim.Spec.VolumeName != pv.Name || claimSize.Cmp(resource.MustParse("50Gi")) != 0 {
		t.Fatalf("original PVC was not expanded in place: %+v", claim.Spec)
	}
	pv.Spec.Capacity[corev1.ResourceStorage] = resource.MustParse("50Gi")
	if err := c.Update(ctx, pv); err != nil {
		t.Fatal(err)
	}
	if _, err := r.reconcileDiskExpansion(ctx, agent, nil); err != nil {
		t.Fatal(err)
	}
	helper := &corev1.Pod{}
	if err := c.Get(ctx, types.NamespacedName{Name: diskCapacityPodName(agent.Name), Namespace: agent.Namespace}, helper); err != nil {
		t.Fatal(err)
	}
	if helper.Spec.AutomountServiceAccountToken == nil || *helper.Spec.AutomountServiceAccountToken ||
		helper.Spec.Volumes[0].PersistentVolumeClaim.ClaimName != pvc.Name ||
		helper.Spec.Containers[0].SecurityContext.RunAsNonRoot == nil || !*helper.Spec.Containers[0].SecurityContext.RunAsNonRoot {
		t.Fatalf("unsafe capacity helper: %+v", helper.Spec)
	}
	helper.Status.Phase = corev1.PodSucceeded
	helper.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "capacity", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, Message: `{"totalBytes":53687091200,"availableBytes":40000000000}`}}}}
	if err := c.Status().Update(ctx, helper); err != nil {
		t.Fatal(err)
	}
	if _, err := r.reconcileDiskExpansion(ctx, agent, nil); err != nil {
		t.Fatal(err)
	}
	if agent.Status.DiskExpansion.Stage != "Complete" || agent.Spec.DesiredPhase != kyberv1.AgentPhaseStopped || agent.Status.DiskExpansion.FilesystemTotalBytes < 50*diskGiB {
		t.Fatalf("stopped Agent did not complete safely: %+v", agent.Status.DiskExpansion)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(helper), helper); err == nil {
		t.Fatal("capacity helper was not cleaned up")
	}
}

func TestDiskExpansion_RunningRestartsOnlyAfterFreshFilesystemSample(t *testing.T) {
	ctx := context.Background()
	scheme := buildTestScheme()
	agent := newTestAgent("echo", "kyber-system")
	agent.UID = types.UID("agent-original")
	agent.Spec.Resources.Disk = resource.MustParse("50Gi")
	agent.Spec.DesiredPhase = kyberv1.AgentPhaseRunning
	agent.Status.Phase = kyberv1.AgentPhaseRunning
	requested := metav1.NewTime(time.Now().Add(-time.Minute))
	agent.Status.DiskExpansion = &kyberv1.AgentDiskExpansionStatus{TargetGiB: 50, PVCUID: types.UID("claim-original"), PVUID: types.UID("volume-original"), Stage: "Provisioning", RequestedAt: &requested}
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: PVCName(agent.Name), Namespace: agent.Namespace, UID: agent.Status.DiskExpansion.PVCUID},
		Spec:       corev1.PersistentVolumeClaimSpec{VolumeName: "pv-echo", StorageClassName: new(string), Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("50Gi")}}},
		Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound, Capacity: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("50Gi")}},
	}
	*pvc.Spec.StorageClassName = "expandable-pd"
	pv := &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: pvc.Spec.VolumeName, UID: agent.Status.DiskExpansion.PVUID},
		Spec: corev1.PersistentVolumeSpec{ClaimRef: &corev1.ObjectReference{UID: pvc.UID}, Capacity: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("50Gi")}, PersistentVolumeSource: corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{Driver: "pd.csi.storage.gke.io"}}}}
	stale := metav1.NewTime(time.Now().Add(-2 * time.Minute))
	agent.Status.Activity = &kyberv1.ActivityStatus{Resources: &kyberv1.AgentResourceUsage{SampledAt: stale, DiskUsageMethod: "statfs", DiskTotalBytes: 20 * diskGiB, DiskBackingTotalBytes: 20 * diskGiB}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(agent, pvc).WithObjects(agent, pvc, pv, diskExpansionClass()).Build()
	r := &AgentReconciler{Client: c, Scheme: scheme}
	pod := &corev1.Pod{Status: corev1.PodStatus{StartTime: &requested}}
	if _, err := r.reconcileDiskExpansion(ctx, agent, pod); err != nil {
		t.Fatal(err)
	}
	if agent.Spec.DesiredPhase != kyberv1.AgentPhaseRunning {
		t.Fatal("stale sidecar sample triggered a restart")
	}
	fresh := metav1.NewTime(time.Now())
	agent.Status.Activity.Resources.SampledAt = fresh
	agent.Status.Activity.Resources.DiskBackingTotalBytes = 50 * diskGiB
	if err := c.Status().Update(ctx, agent); err != nil {
		t.Fatal(err)
	}
	if _, err := r.reconcileDiskExpansion(ctx, agent, pod); err != nil {
		t.Fatal(err)
	}
	if agent.Spec.DesiredPhase != kyberv1.AgentPhaseRestarting || agent.Status.DiskExpansion.Stage != "Restarting" {
		t.Fatalf("expanded live Agent did not request an intentional restart: phase=%s op=%+v", agent.Spec.DesiredPhase, agent.Status.DiskExpansion)
	}
	newSample := metav1.NewTime(time.Now().Add(2 * time.Second))
	agent.Status.Activity.Resources.SampledAt = newSample
	agent.Status.Activity.Resources.DiskTotalBytes = 50 * diskGiB
	agent.Status.Phase = kyberv1.AgentPhaseRunning
	newPodStart := metav1.NewTime(time.Now().Add(time.Second))
	newPod := &corev1.Pod{Status: corev1.PodStatus{StartTime: &newPodStart}}
	if _, err := r.reconcileDiskExpansion(ctx, agent, newPod); err != nil {
		t.Fatal(err)
	}
	if agent.Status.DiskExpansion.Stage != "Complete" {
		t.Fatalf("fresh new-sidecar sample did not complete expansion: %+v", agent.Status.DiskExpansion)
	}
}

func TestDiskExpansion_FilesystemThreshold(t *testing.T) {
	if filesystemExpanded(0, 50*diskGiB) || filesystemExpanded(20*diskGiB, 50*diskGiB) || !filesystemExpanded(50*diskGiB-100*1024*1024, 50*diskGiB) {
		t.Fatal("filesystem capacity threshold is incorrect")
	}
}

func TestDiskExpansion_BlockedRequestRecoversAtLowerUnallocatedTarget(t *testing.T) {
	ctx := context.Background()
	scheme := buildTestScheme()
	agent := newTestAgent("echo", "kyber-system")
	agent.UID = types.UID("agent-original")
	agent.Spec.Resources.Disk = resource.MustParse("50Gi")
	agent.Status.Phase = kyberv1.AgentPhaseStopped
	agent.Spec.DesiredPhase = kyberv1.AgentPhaseStopped
	requested := metav1.Now()
	agent.Status.DiskExpansion = &kyberv1.AgentDiskExpansionStatus{TargetGiB: 50, PVCUID: types.UID("claim-original"), PVUID: types.UID("volume-original"), Stage: "Requested", RequestedAt: &requested}
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: PVCName(agent.Name), Namespace: agent.Namespace, UID: agent.Status.DiskExpansion.PVCUID},
		Spec:       corev1.PersistentVolumeClaimSpec{VolumeName: "pv-echo", StorageClassName: new(string), Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("100Gi")}}},
		Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound, Capacity: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("20Gi")}},
	}
	*pvc.Spec.StorageClassName = "expandable-pd"
	pv := &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: pvc.Spec.VolumeName, UID: agent.Status.DiskExpansion.PVUID},
		Spec: corev1.PersistentVolumeSpec{ClaimRef: &corev1.ObjectReference{UID: pvc.UID}, Capacity: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("20Gi")}, PersistentVolumeSource: corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{Driver: "pd.csi.storage.gke.io"}}}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(agent, pvc).WithObjects(agent, pvc, pv, diskExpansionClass()).Build()
	r := &AgentReconciler{Client: c, Scheme: scheme}
	if _, err := r.reconcileDiskExpansion(ctx, agent, nil); err != nil {
		t.Fatal(err)
	}
	claim := &corev1.PersistentVolumeClaim{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(pvc), claim); err != nil {
		t.Fatal(err)
	}
	claimSize := claim.Spec.Resources.Requests[corev1.ResourceStorage]
	if claimSize.Cmp(resource.MustParse("50Gi")) != 0 || claim.UID != pvc.UID {
		t.Fatalf("unallocated PVC target was not corrected in place: %+v", claim.Spec)
	}
}

func TestDiskExpansion_DiskExhaustedAfterHelperRequestsRecovery(t *testing.T) {
	ctx := context.Background()
	scheme := buildTestScheme()
	agent := newTestAgent("echo", "kyber-system")
	agent.UID = types.UID("agent-original")
	agent.Spec.Resources.Disk = resource.MustParse("50Gi")
	agent.Spec.DesiredPhase = kyberv1.AgentPhaseRunning
	agent.Status.Phase = kyberv1.AgentPhaseDiskExhausted
	requested := metav1.Now()
	agent.Status.DiskExpansion = &kyberv1.AgentDiskExpansionStatus{TargetGiB: 50, PVCUID: types.UID("claim-original"), PVUID: types.UID("volume-original"), Stage: "Verifying", RequestedAt: &requested}
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: PVCName(agent.Name), Namespace: agent.Namespace, UID: agent.Status.DiskExpansion.PVCUID},
		Spec:       corev1.PersistentVolumeClaimSpec{VolumeName: "pv-echo", StorageClassName: new(string), Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("50Gi")}}},
		Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound, Capacity: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("50Gi")}},
	}
	*pvc.Spec.StorageClassName = "expandable-pd"
	pv := &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: pvc.Spec.VolumeName, UID: agent.Status.DiskExpansion.PVUID},
		Spec: corev1.PersistentVolumeSpec{ClaimRef: &corev1.ObjectReference{UID: pvc.UID}, Capacity: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("50Gi")}, PersistentVolumeSource: corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{Driver: "pd.csi.storage.gke.io"}}}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(agent, pvc).WithObjects(agent, pvc, pv, diskExpansionClass()).Build()
	r := &AgentReconciler{Client: c, Scheme: scheme}
	if _, err := r.reconcileDiskExpansion(ctx, agent, nil); err != nil {
		t.Fatal(err)
	}
	if agent.Spec.DesiredPhase != kyberv1.AgentPhaseRestarting {
		t.Fatalf("disk exhaustion recovery was not requested: %s", agent.Spec.DesiredPhase)
	}
	event, err := r.classifyEvent(ctx, agent, nil)
	if err != nil || event != EventDesiredRestarting {
		t.Fatalf("disk exhaustion restart was not classified: %s %v", event, err)
	}
}

func TestDiskExpansion_BlockedOperationCannotGrowPVCThroughLegacyRecovery(t *testing.T) {
	ctx := context.Background()
	scheme := buildTestScheme()
	agent := newTestAgent("echo", "kyber-system")
	agent.Spec.Resources.Disk = resource.MustParse("50Gi")
	agent.Status.DiskExpansion = &kyberv1.AgentDiskExpansionStatus{TargetGiB: 50, Stage: "Blocked"}
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: PVCName(agent.Name), Namespace: agent.Namespace},
		Spec: corev1.PersistentVolumeClaimSpec{Resources: corev1.VolumeResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("20Gi")},
		}},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(agent, pvc).Build()
	r := &AgentReconciler{Client: c, Scheme: scheme}
	if err := r.ensurePVC(ctx, agent); err != nil {
		t.Fatal(err)
	}
	stored := &corev1.PersistentVolumeClaim{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(pvc), stored); err != nil {
		t.Fatal(err)
	}
	size := stored.Spec.Resources.Requests[corev1.ResourceStorage]
	if size.Cmp(resource.MustParse("20Gi")) != 0 {
		t.Fatalf("blocked expansion bypassed preflight: %s", size.String())
	}
}

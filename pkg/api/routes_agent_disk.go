package api

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"slices"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	agentcontroller "github.com/matty-v/kyber/pkg/controllers/agent"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const diskGiB int64 = 1 << 30

// agentDiskOptions is deliberately provider-neutral. The bound PVC and PV are
// read by the server for preflight, but their cloud volume handles stay private.
type agentDiskOptions struct {
	Supported                bool                   `json:"supported"`
	Reason                   string                 `json:"reason,omitempty"`
	Driver                   string                 `json:"driver,omitempty"`
	StorageClass             string                 `json:"storageClass,omitempty"`
	CurrentTargetGiB         int64                  `json:"currentTargetGiB"`
	PVCRequestGiB            int64                  `json:"pvcRequestGiB,omitempty"`
	PVCapacityGiB            int64                  `json:"pvCapacityGiB,omitempty"`
	FilesystemTotalBytes     int64                  `json:"filesystemTotalBytes,omitempty"`
	FilesystemAvailableBytes int64                  `json:"filesystemAvailableBytes,omitempty"`
	FilesystemSampledAt      string                 `json:"filesystemSampledAt,omitempty"`
	StepGiB                  int64                  `json:"stepGiB"`
	MinimumTargetGiB         int64                  `json:"minimumTargetGiB"`
	SuggestedTargetsGiB      []int64                `json:"suggestedTargetsGiB"`
	Operation                *diskOperationResponse `json:"operation,omitempty"`
}

type diskOperationResponse struct {
	TargetGiB                int64  `json:"targetGiB"`
	Stage                    string `json:"stage"`
	Reason                   string `json:"reason,omitempty"`
	PVCRequestBytes          int64  `json:"pvcRequestBytes,omitempty"`
	PVCapacityBytes          int64  `json:"pvCapacityBytes,omitempty"`
	FilesystemTotalBytes     int64  `json:"filesystemTotalBytes,omitempty"`
	FilesystemAvailableBytes int64  `json:"filesystemAvailableBytes,omitempty"`
	FilesystemSampledAt      string `json:"filesystemSampledAt,omitempty"`
}

func diskOperationForAPI(op *kyberv1.AgentDiskExpansionStatus) *diskOperationResponse {
	if op == nil {
		return nil
	}
	response := &diskOperationResponse{TargetGiB: op.TargetGiB, Stage: op.Stage, Reason: op.Reason,
		PVCRequestBytes: op.PVCRequestBytes, PVCapacityBytes: op.PVCapacityBytes,
		FilesystemTotalBytes: op.FilesystemTotalBytes, FilesystemAvailableBytes: op.FilesystemAvailableBytes}
	if op.FilesystemSampledAt != nil {
		response.FilesystemSampledAt = op.FilesystemSampledAt.UTC().Format("2006-01-02T15:04:05Z")
	}
	return response
}

type diskPreflight struct {
	options agentDiskOptions
	agent   *kyberv1.Agent
	pvc     *corev1.PersistentVolumeClaim
	pv      *corev1.PersistentVolume
}

func quantityGiBCeil(q resource.Quantity) int64 {
	if q.Value() <= 0 {
		return 0
	}
	return (q.Value() + diskGiB - 1) / diskGiB
}

func (s *Server) readDiskPreflight(r *http.Request, name string) (*diskPreflight, error) {
	reader := s.APIReader
	if reader == nil {
		reader = s.K8sClient
	}
	p := &diskPreflight{options: agentDiskOptions{StepGiB: 1, SuggestedTargetsGiB: []int64{}}}
	p.agent = &kyberv1.Agent{}
	if err := reader.Get(r.Context(), types.NamespacedName{Namespace: s.Namespace, Name: name}, p.agent); err != nil {
		return nil, err
	}
	p.options.CurrentTargetGiB = quantityGiBCeil(p.agent.Spec.Resources.Disk)
	p.options.Operation = diskOperationForAPI(p.agent.Status.DiskExpansion)
	if usage := p.agent.Status.Activity; usage != nil && usage.Resources != nil {
		if usage.Resources.DiskUsageMethod == "statfs" {
			p.options.FilesystemTotalBytes = usage.Resources.DiskBackingTotalBytes
			p.options.FilesystemAvailableBytes = usage.Resources.DiskBackingAvailableBytes
			p.options.FilesystemSampledAt = usage.Resources.SampledAt.UTC().Format("2006-01-02T15:04:05Z")
		}
	}
	defer func() {
		base := max(p.options.CurrentTargetGiB, p.options.PVCRequestGiB, p.options.PVCapacityGiB)
		if p.pvc != nil {
			claimCapacity := p.pvc.Status.Capacity[corev1.ResourceStorage]
			allocated := p.pvc.Status.AllocatedResources[corev1.ResourceStorage]
			known := max(p.options.PVCapacityGiB, quantityGiBCeil(claimCapacity), quantityGiBCeil(allocated))
			if p.agent.Status.DiskExpansion != nil && p.agent.Status.DiskExpansion.Stage == "Blocked" {
				base = known
			}
			p.options.MinimumTargetGiB = max(base, known) + 1
		} else {
			p.options.MinimumTargetGiB = base + 1
		}
		for _, candidate := range []int64{20, 30, 50, 100, 200, 500, 1000} {
			if candidate >= p.options.MinimumTargetGiB {
				p.options.SuggestedTargetsGiB = append(p.options.SuggestedTargetsGiB, candidate)
			}
		}
	}()
	p.pvc = &corev1.PersistentVolumeClaim{}
	if err := reader.Get(r.Context(), types.NamespacedName{Namespace: s.Namespace, Name: agentcontroller.PVCName(name)}, p.pvc); err != nil {
		p.options.Reason = "Agent persist volume is unavailable"
		return p, nil
	}
	p.options.PVCRequestGiB = quantityGiBCeil(p.pvc.Spec.Resources.Requests[corev1.ResourceStorage])
	if p.pvc.Status.Phase != corev1.ClaimBound || p.pvc.Spec.VolumeName == "" {
		p.options.Reason = "Agent persist volume is not bound"
		return p, nil
	}
	owned := false
	for _, owner := range p.pvc.GetOwnerReferences() {
		if owner.Kind == "Agent" && owner.Name == p.agent.Name && owner.UID == p.agent.UID {
			owned = true
		}
	}
	if !owned {
		p.options.Reason = "Agent persist volume ownership does not match"
		return p, nil
	}
	if p.pvc.Spec.VolumeMode != nil && *p.pvc.Spec.VolumeMode != corev1.PersistentVolumeFilesystem {
		p.options.Reason = "Only filesystem volumes can be expanded"
		return p, nil
	}
	if !slices.Contains(p.pvc.Spec.AccessModes, corev1.ReadWriteOnce) {
		p.options.Reason = "Only read-write-once Agent volumes are supported"
		return p, nil
	}
	p.pv = &corev1.PersistentVolume{}
	if err := reader.Get(r.Context(), types.NamespacedName{Name: p.pvc.Spec.VolumeName}, p.pv); err != nil {
		p.options.Reason = "Bound persistent volume is unavailable"
		return p, nil
	}
	if p.pv.Spec.ClaimRef == nil || p.pv.Spec.ClaimRef.UID != p.pvc.UID {
		p.options.Reason = "Persistent volume binding changed"
		return p, nil
	}
	p.options.PVCapacityGiB = quantityGiBCeil(p.pv.Spec.Capacity[corev1.ResourceStorage])
	if p.pvc.Spec.StorageClassName == nil || *p.pvc.Spec.StorageClassName == "" {
		p.options.Reason = "Agent volume has no expandable StorageClass"
		return p, nil
	}
	p.options.StorageClass = *p.pvc.Spec.StorageClassName
	storageClass := &storagev1.StorageClass{}
	if err := reader.Get(r.Context(), types.NamespacedName{Name: p.options.StorageClass}, storageClass); err != nil {
		p.options.Reason = "StorageClass is unavailable"
		return p, nil
	}
	p.options.Driver = storageClass.Provisioner
	if storageClass.AllowVolumeExpansion == nil || !*storageClass.AllowVolumeExpansion {
		p.options.Reason = "StorageClass does not allow expansion"
		return p, nil
	}
	if storageClass.Provisioner != "pd.csi.storage.gke.io" && storageClass.Provisioner != "ebs.csi.aws.com" {
		p.options.Reason = "Disk expansion is not verified for this storage driver"
		return p, nil
	}
	if p.pv.Spec.CSI == nil || p.pv.Spec.CSI.Driver != storageClass.Provisioner {
		p.options.Reason = "Bound volume driver does not match its StorageClass"
		return p, nil
	}
	p.options.Supported = true
	return p, nil
}

func (s *Server) getAgentDiskOptions(w http.ResponseWriter, r *http.Request, name string) {
	p, err := s.readDiskPreflight(r, name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			writeJSONError(w, http.StatusNotFound, "not_found", "agent not found")
		} else {
			writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to read Agent disk options")
		}
		return
	}
	writeJSON(w, http.StatusOK, p.options)
}

type increaseDiskRequest struct {
	SizeGiB int64 `json:"sizeGiB"`
}

func (s *Server) increaseAgentDisk(w http.ResponseWriter, r *http.Request, name string) {
	if !s.requireScope(w, r, name, "increase-disk", ScopeLifecycleWrite) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var req increaseDiskRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil || req.SizeGiB < 1 || req.SizeGiB > 65536 {
		writeJSONErrorWithField(w, http.StatusBadRequest, "VALIDATION_ERROR", "sizeGiB must be an integer from 1 to 65536", "sizeGiB")
		return
	}
	p, err := s.readDiskPreflight(r, name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			writeJSONError(w, http.StatusNotFound, "not_found", "agent not found")
		} else {
			writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to inspect Agent disk")
		}
		return
	}
	if !p.options.Supported {
		writeJSONError(w, http.StatusConflict, "DISK_EXPANSION_UNSUPPORTED", p.options.Reason)
		return
	}
	if req.SizeGiB == p.options.CurrentTargetGiB && p.agent.Status.DiskExpansion != nil && p.agent.Status.DiskExpansion.Stage != "Blocked" && p.agent.Status.DiskExpansion.TargetGiB == req.SizeGiB {
		writeJSON(w, http.StatusAccepted, diskOperationForAPI(p.agent.Status.DiskExpansion))
		return
	}
	blockedRetry := p.agent.Status.DiskExpansion != nil && p.agent.Status.DiskExpansion.Stage == "Blocked"
	targetBytes := req.SizeGiB * diskGiB
	pvCapacity := p.pv.Spec.Capacity[corev1.ResourceStorage]
	claimCapacity := p.pvc.Status.Capacity[corev1.ResourceStorage]
	allocated := p.pvc.Status.AllocatedResources[corev1.ResourceStorage]
	if targetBytes <= max(pvCapacity.Value(), claimCapacity.Value(), allocated.Value()) || (!blockedRetry && req.SizeGiB <= p.options.PVCRequestGiB) {
		writeJSONErrorWithField(w, http.StatusBadRequest, "VALIDATION_ERROR", "disk size must exceed the existing provisioned size; shrinking is not supported", "sizeGiB")
		return
	}
	if op := p.agent.Status.DiskExpansion; op != nil && op.Stage != "Complete" && op.Stage != "Blocked" {
		writeJSONError(w, http.StatusConflict, "DISK_EXPANSION_BUSY", "wait for the active disk expansion to settle")
		return
	}
	if req.SizeGiB <= p.options.CurrentTargetGiB && !blockedRetry {
		writeJSONErrorWithField(w, http.StatusBadRequest, "VALIDATION_ERROR", "disk size must increase", "sizeGiB")
		return
	}
	if err := s.checkDiskQuota(r, p.pvc, req.SizeGiB); err != nil {
		writeJSONErrorWithField(w, http.StatusConflict, "DISK_QUOTA_EXCEEDED", err.Error(), "sizeGiB")
		return
	}
	previous := p.agent.DeepCopy()
	p.agent.Spec.Resources.Disk = resource.MustParse(fmt.Sprintf("%dGi", req.SizeGiB))
	if err := s.K8sClient.Patch(r.Context(), p.agent, client.MergeFromWithOptions(previous, client.MergeFromWithOptimisticLock{})); err != nil {
		writeJSONError(w, http.StatusConflict, "DISK_EXPANSION_CONFLICT", "Agent changed; retry disk increase")
		return
	}
	now := metav1.Now()
	op := &kyberv1.AgentDiskExpansionStatus{RequestedAt: &now, TargetGiB: req.SizeGiB, PVCUID: p.pvc.UID, PVUID: p.pv.UID, Stage: "Requested"}
	current := &kyberv1.Agent{}
	reader := s.APIReader
	if reader == nil {
		reader = s.K8sClient
	}
	if err := reader.Get(r.Context(), types.NamespacedName{Namespace: s.Namespace, Name: name}, current); err != nil {
		slog.Warn("Agent disk target accepted but status fetch failed; controller will recover", "agent", name, "error", err)
		writeJSON(w, http.StatusAccepted, diskOperationForAPI(op))
		return
	}
	if current.Spec.Resources.Disk.Value() != targetBytes {
		slog.Warn("Agent disk target changed before status write; controller will recover", "agent", name, "currentBytes", current.Spec.Resources.Disk.Value(), "targetBytes", targetBytes)
		writeJSON(w, http.StatusAccepted, diskOperationForAPI(op))
		return
	}
	current.Status.DiskExpansion = op
	if err := s.K8sClient.Status().Update(r.Context(), current); err != nil {
		// Reconciliation can recover from spec > PVC even if this status write
		// loses a race; never report completion on this response.
		slog.Warn("Agent disk target accepted but status write failed; controller will recover", "agent", name, "error", err)
		writeJSON(w, http.StatusAccepted, diskOperationForAPI(op))
		return
	}
	writeJSON(w, http.StatusAccepted, diskOperationForAPI(op))
}

func (s *Server) checkDiskQuota(r *http.Request, pvc *corev1.PersistentVolumeClaim, targetGiB int64) error {
	reader := s.APIReader
	if reader == nil {
		reader = s.K8sClient
	}
	var quotas corev1.ResourceQuotaList
	if err := reader.List(r.Context(), &quotas, client.InNamespace(s.Namespace)); err != nil {
		return fmt.Errorf("cannot verify namespace storage quota")
	}
	current := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
	delta := resource.MustParse(fmt.Sprintf("%dGi", targetGiB))
	delta.Sub(current)
	for _, quota := range quotas.Items {
		for _, name := range []corev1.ResourceName{corev1.ResourceRequestsStorage, corev1.ResourceName(*pvc.Spec.StorageClassName + ".storageclass.storage.k8s.io/requests.storage")} {
			hard, ok := quota.Status.Hard[name]
			if !ok {
				continue
			}
			used := quota.Status.Used[name]
			used.Add(delta)
			if used.Cmp(hard) > 0 {
				return fmt.Errorf("namespace storage quota would be exceeded")
			}
		}
	}
	return nil
}

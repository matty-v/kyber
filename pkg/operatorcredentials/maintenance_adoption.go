package operatorcredentials

import (
	"bytes"
	"context"
	"encoding/json"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
)

const AdoptionOperationLabel = "kyber.io/operator-credential-adoption"
const adoptionWriterAnnotation = "kyber.io/operator-credential-adoption-writer"

type adoptionWriterRecord struct {
	AdmissionVersion string            `json:"admissionVersion"`
	OperationID      string            `json:"operationId"`
	ExecutionID      string            `json:"executionId"`
	Writer           MaintenanceWriter `json:"writer"`
}

func decodeAdoptionWriter(raw string) (adoptionWriterRecord, error) {
	var record adoptionWriterRecord
	if len(raw) == 0 || len(raw) > 2048 || json.Unmarshal([]byte(raw), &record) != nil || !opaque(record.AdmissionVersion) || !canonicalWriterUID(record.OperationID) || !canonicalWriterUID(record.ExecutionID) || !record.Writer.valid() || record.Writer.Container != "adopt-credentials" {
		return adoptionWriterRecord{}, ErrUnsupported
	}
	encoded, _ := json.Marshal(record)
	if !bytes.Equal(encoded, []byte(raw)) {
		return adoptionWriterRecord{}, ErrUnsupported
	}
	return record, nil
}

// AcquireAdoptionForPod also verifies the original operation label before the
// one-execution Job claim. It does not authorize adoption or admit audit.
func (s *MaintenanceStore) AcquireAdoptionForPod(ctx context.Context, operationID, podName, podUID string) (*MaintenanceReservation, error) {
	if !canonicalWriterUID(operationID) {
		return nil, ErrInvalid
	}
	writer, owner, err := s.claimWriterForPod(ctx, MaintenanceAdoption, podName, podUID, operationID)
	if err != nil {
		return nil, err
	}
	return s.acquireOwned(ctx, MaintenanceAdoption, writer, owner)
}

// ResumeAdoptionForPod fences a proven stopped writer with a fresh, exact Job
// execution. The caller must separately require current admin authorization,
// original immutable admission, source/authority reconciliation and checkpoints.
// This never releases maintenance or chooses a different operation.
func (s *MaintenanceStore) ResumeAdoptionForPod(ctx context.Context, uid, version, operationID, podName, podUID string) (*MaintenanceReservation, error) {
	if s == nil || s.direct == nil || !canonicalWriterUID(uid) || version == "" || !canonicalWriterUID(operationID) {
		return nil, ErrInvalid
	}
	ctx, cancel := context.WithTimeout(ctx, 8*requestTimeout)
	defer cancel()
	status, err := s.Inspect(ctx)
	if err != nil {
		return nil, err
	}
	if !status.Present || status.Kind != MaintenanceAdoption || status.UID != uid || status.Version != version || status.Writer == nil {
		return nil, ErrConflict
	}
	if status.RecoveryOperationID != "" && status.RecoveryOperationID != operationID {
		return nil, ErrConflict
	}
	stopped, err := s.WriterState(ctx, uid, version)
	if err != nil {
		return nil, err
	}
	if stopped != MaintenanceWriterStopped {
		return nil, ErrConflict
	}
	priorJob := &batchv1.Job{}
	if err := s.direct.reader.Get(ctx, types.NamespacedName{Namespace: s.key.Namespace, Name: status.Writer.JobName}, priorJob); err != nil {
		return nil, authorityUnavailable(ctx, err)
	}
	if string(priorJob.UID) != status.Writer.JobUID || priorJob.Labels[AdoptionOperationLabel] != operationID {
		return nil, ErrConflict
	}
	writer, execution, err := s.claimWriterForPod(ctx, MaintenanceAdoption, podName, podUID, operationID)
	if err != nil {
		return nil, err
	}
	if writer.JobUID == status.Writer.JobUID {
		return nil, ErrConflict
	}
	current := &corev1.Secret{}
	if err := s.direct.reader.Get(ctx, s.key, current); err != nil {
		return nil, authorityUnavailable(ctx, err)
	}
	if s.validate(current) != nil || string(current.UID) != uid || current.ResourceVersion != version {
		return nil, ErrConflict
	}
	original, err := decodeMaintenanceWriter(current.Data["writer"])
	if err != nil {
		return nil, err
	}
	// The execution UUID is also the one-use Job claim. A lost acknowledgement
	// may reconcile only this exact receipt; another concurrent winner is not ours.
	record := adoptionWriterRecord{AdmissionVersion: status.AdoptionAdmissionVersion, OperationID: operationID, ExecutionID: execution, Writer: *writer}
	raw, _ := json.Marshal(record)
	if current.Annotations == nil {
		current.Annotations = map[string]string{}
	}
	current.Annotations[adoptionWriterAnnotation] = string(raw)
	writeErr := s.direct.writer.Update(ctx, current)
	confirmed := &corev1.Secret{}
	if err := s.direct.reader.Get(ctx, s.key, confirmed); err != nil {
		return nil, authorityUnavailable(ctx, err)
	}
	if s.validate(confirmed) != nil || string(confirmed.UID) != uid || confirmed.ResourceVersion == "" || confirmed.ResourceVersion == version || confirmed.Annotations[adoptionWriterAnnotation] != string(raw) || string(confirmed.Data["owner"]) != string(current.Data["owner"]) {
		if writeErr != nil {
			return nil, authorityError(ctx, writeErr)
		}
		return nil, ErrConflict
	}
	return &MaintenanceReservation{store: s, kind: MaintenanceAdoption, uid: confirmed.UID, version: confirmed.ResourceVersion, owner: string(confirmed.Data["owner"]), writer: writer, cleanupOwner: execution, originalWriter: &original}, nil
}

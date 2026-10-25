package operatorcredentials

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const maintenanceRecoveryAnnotation = "kyber.io/operator-credential-maintenance-recovery"

type maintenanceRecoveryRecord struct {
	OperationID string `json:"operationId"`
	Nonce       string `json:"nonce"`
}

func decodeMaintenanceRecovery(raw string) (maintenanceRecoveryRecord, error) {
	var record maintenanceRecoveryRecord
	if len(raw) == 0 || len(raw) > 256 || json.Unmarshal([]byte(raw), &record) != nil || !canonicalWriterUID(record.OperationID) || !canonicalWriterUID(record.Nonce) {
		return maintenanceRecoveryRecord{}, ErrUnsupported
	}
	encoded, _ := json.Marshal(record)
	if !bytes.Equal(encoded, []byte(raw)) {
		return maintenanceRecoveryRecord{}, ErrUnsupported
	}
	return record, nil
}

// MaintenanceReleaseProof is metadata for the durable delete intent. It cannot
// construct an ownership capability or authorize a public recovery operation.
type MaintenanceReleaseProof struct {
	ResourceUID     string
	ResourceVersion string
	ProofID         string
}

// MaintenanceRecoveryHandle is opaque to API callers. Implementations must not
// serialize private ownership; Proof is only the metadata to journal first.
type MaintenanceRecoveryHandle interface {
	Proof() MaintenanceReleaseProof
	Release(context.Context) error
}

type MaintenanceRecoveryReservation struct {
	reservation *MaintenanceReservation
	operationID string
	nonce       string
}

func (MaintenanceRecoveryReservation) String() string     { return "maintenance recovery [redacted]" }
func (r MaintenanceRecoveryReservation) GoString() string { return r.String() }
func (r MaintenanceRecoveryReservation) Format(s fmt.State, _ rune) {
	_, _ = io.WriteString(s, r.String())
}
func (MaintenanceRecoveryReservation) MarshalJSON() ([]byte, error) { return nil, ErrSensitive }
func (*MaintenanceRecoveryReservation) UnmarshalJSON([]byte) error  { return ErrSensitive }
func (r *MaintenanceRecoveryReservation) Proof() MaintenanceReleaseProof {
	if r == nil || r.reservation == nil {
		return MaintenanceReleaseProof{}
	}
	return MaintenanceReleaseProof{ResourceUID: string(r.reservation.uid), ResourceVersion: r.reservation.version, ProofID: r.nonce}
}

// PrepareRecovery only fences metadata and verifies the stopped writer. The
// caller must require current admin access, durable admission and release/
// authority reconciliation, then durably record Proof BEFORE Release.
// A different active operation is never displaced. Replacing a failed marker
// requires the caller's durable terminal-failure proof and a fenced old effect;
// replaceFailedOperation must identify exactly that inspected prior operation.
func (s *MaintenanceStore) PrepareRecovery(ctx context.Context, uid, version, operationID, replaceFailedOperation string) (MaintenanceRecoveryHandle, error) {
	if s == nil || s.direct == nil || !canonicalWriterUID(uid) || version == "" || !canonicalWriterUID(operationID) || (replaceFailedOperation != "" && !canonicalWriterUID(replaceFailedOperation)) {
		return nil, ErrInvalid
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	status, err := s.Inspect(ctx)
	if err != nil {
		return nil, err
	}
	if !status.Present || status.UID != uid || status.Version != version {
		return nil, ErrConflict
	}
	if status.Kind != MaintenanceUpgrade || status.Writer == nil {
		return nil, ErrUnsupported
	}
	if state, err := s.WriterState(ctx, uid, version); err != nil {
		return nil, err
	} else if state != MaintenanceWriterStopped {
		return nil, ErrUnavailable
	}
	current := &corev1.Secret{}
	if err := s.direct.reader.Get(ctx, s.key, current); err != nil {
		return nil, authorityUnavailable(ctx, err)
	}
	if err := s.validate(current); err != nil {
		return nil, err
	}
	if string(current.UID) != uid || current.ResourceVersion != version {
		return nil, ErrConflict
	}
	if raw, found := current.Annotations[maintenanceRecoveryAnnotation]; found {
		previous, err := decodeMaintenanceRecovery(raw)
		if err != nil {
			return nil, err
		}
		if previous.OperationID != operationID && (replaceFailedOperation == "" || previous.OperationID != replaceFailedOperation) {
			return nil, ErrConflict
		}
	}
	record := maintenanceRecoveryRecord{OperationID: operationID, Nonce: uuid.NewString()}
	encoded, _ := json.Marshal(record)
	if current.Annotations == nil {
		current.Annotations = map[string]string{}
	}
	current.Annotations[maintenanceRecoveryAnnotation] = string(encoded)
	writeErr := s.direct.writer.Update(ctx, current)
	confirmed := &corev1.Secret{}
	if err := s.direct.reader.Get(ctx, s.key, confirmed); err != nil {
		return nil, authorityUnavailable(ctx, err)
	}
	if err := s.validate(confirmed); err != nil {
		return nil, err
	}
	if string(confirmed.UID) != uid || confirmed.ResourceVersion == version || confirmed.Annotations[maintenanceRecoveryAnnotation] != string(encoded) {
		if writeErr != nil {
			return nil, authorityError(ctx, writeErr)
		}
		return nil, ErrConflict
	}
	capability := &MaintenanceReservation{store: s, owner: string(confirmed.Data["owner"]), kind: MaintenanceUpgrade, uid: confirmed.UID, version: confirmed.ResourceVersion}
	return &MaintenanceRecoveryReservation{reservation: capability, operationID: operationID, nonce: record.Nonce}, nil
}

// ResumeRecovery accepts only the journal's exact immutable delete proof. It
// reads and verifies evidence but never refreshes/fences its admitted version.
func (s *MaintenanceStore) ResumeRecovery(ctx context.Context, operationID string, proof MaintenanceReleaseProof) (MaintenanceRecoveryHandle, error) {
	if s == nil || !canonicalWriterUID(operationID) || !canonicalWriterUID(proof.ResourceUID) || proof.ResourceVersion == "" || !canonicalWriterUID(proof.ProofID) {
		return nil, ErrInvalid
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	status, err := s.Inspect(ctx)
	if err != nil {
		return nil, err
	}
	if !status.Present || status.UID != proof.ResourceUID || status.Version != proof.ResourceVersion {
		return nil, ErrConflict
	}
	if status.Kind != MaintenanceUpgrade || status.Writer == nil {
		return nil, ErrUnsupported
	}
	if state, err := s.WriterState(ctx, status.UID, status.Version); err != nil {
		return nil, err
	} else if state != MaintenanceWriterStopped {
		return nil, ErrUnavailable
	}
	current := &corev1.Secret{}
	if err := s.direct.reader.Get(ctx, s.key, current); err != nil {
		return nil, authorityUnavailable(ctx, err)
	}
	if err := s.validate(current); err != nil {
		return nil, err
	}
	record, err := decodeMaintenanceRecovery(current.Annotations[maintenanceRecoveryAnnotation])
	if err != nil {
		return nil, err
	}
	if string(current.UID) != proof.ResourceUID || current.ResourceVersion != proof.ResourceVersion || record.OperationID != operationID || record.Nonce != proof.ProofID {
		return nil, ErrConflict
	}
	capability := &MaintenanceReservation{store: s, owner: string(current.Data["owner"]), kind: MaintenanceUpgrade, uid: current.UID, version: current.ResourceVersion}
	return &MaintenanceRecoveryReservation{reservation: capability, operationID: operationID, nonce: proof.ProofID}, nil
}

// Release deletes exactly the fenced incarnation/version after the caller has
// durably confirmed its delete intent. An uncertain response stays pending for
// explicit resolution; never substitute another UID or refreshed version.
func (r *MaintenanceRecoveryReservation) Release(ctx context.Context) error {
	if r == nil || r.reservation == nil {
		return ErrInvalid
	}
	cap := r.reservation
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	if err := cap.Check(ctx, MaintenanceUpgrade); err != nil {
		return err
	}
	current := &corev1.Secret{}
	if err := cap.store.direct.reader.Get(ctx, cap.store.key, current); err != nil {
		return authorityUnavailable(ctx, err)
	}
	record, err := decodeMaintenanceRecovery(current.Annotations[maintenanceRecoveryAnnotation])
	if err != nil {
		return err
	}
	if current.UID != cap.uid || current.ResourceVersion != cap.version || record.OperationID != r.operationID || record.Nonce != r.nonce {
		return ErrConflict
	}
	err = cap.store.direct.writer.Delete(ctx, current, &client.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &cap.uid, ResourceVersion: &cap.version}})
	if apierrors.IsNotFound(err) && ctx.Err() == nil {
		return nil
	}
	return authorityError(ctx, err)
}

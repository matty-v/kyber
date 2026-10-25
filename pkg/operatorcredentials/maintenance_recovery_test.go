package operatorcredentials

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func stoppedRecoveryFixture(t *testing.T) (*MaintenanceStore, client.Client, *MaintenanceReservation, MaintenanceStatus) {
	t.Helper()
	s, c, pod, job := writerFixture(t)
	ctx := context.Background()
	original, err := s.AcquireForPod(ctx, MaintenanceUpgrade, pod.Name, string(pod.UID))
	if err != nil {
		t.Fatal(err)
	}
	stopWriterFixture(t, c, pod, job)
	status, err := s.Inspect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return s, c, original, status
}
func TestMaintenanceRecoveryFencesOldOwnerAndResumesOnlyExactProof(t *testing.T) {
	s, _, original, status := stoppedRecoveryFixture(t)
	ctx := context.Background()
	operationID := uuid.NewString()
	recovery, err := s.PrepareRecovery(ctx, status.UID, status.Version, operationID, "")
	if err != nil {
		t.Fatal(err)
	}
	proof := recovery.Proof()
	if proof.ResourceUID != status.UID || proof.ResourceVersion == status.Version || !canonicalWriterUID(proof.ProofID) {
		t.Fatal("recovery proof not fenced")
	}
	if err := original.Release(ctx); !errors.Is(err, ErrConflict) {
		t.Fatal("original owner could release fenced reservation", err)
	}
	after, err := s.Inspect(ctx)
	if err != nil || after.RecoveryOperationID != operationID {
		t.Fatal("pending recovery not observable", err)
	}
	wrong := proof
	wrong.ProofID = uuid.NewString()
	if _, err := s.ResumeRecovery(ctx, operationID, wrong); !errors.Is(err, ErrConflict) {
		t.Fatal("different proof resumed release", err)
	}
	resumed, err := s.ResumeRecovery(ctx, operationID, proof)
	if err != nil {
		t.Fatal(err)
	}
	if err := resumed.Release(ctx); err != nil {
		t.Fatal(err)
	}
	if after, err := s.Inspect(ctx); err != nil || after.Present {
		t.Fatal("exact recovered object remained", err)
	}
}
func TestMaintenanceRecoveryCannotDisplaceAnotherActiveOperation(t *testing.T) {
	s, _, _, status := stoppedRecoveryFixture(t)
	ctx := context.Background()
	operationID := uuid.NewString()
	first, err := s.PrepareRecovery(ctx, status.UID, status.Version, operationID, "")
	if err != nil {
		t.Fatal(err)
	}
	current, err := s.Inspect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PrepareRecovery(ctx, current.UID, current.Version, uuid.NewString(), ""); !errors.Is(err, ErrConflict) {
		t.Fatal("active logical recovery displaced", err)
	}
	// Only the trusted caller can supply the separately verified terminal-failure
	// assertion. Public endpoints must prove the old journal outcome first.
	second, err := s.PrepareRecovery(ctx, current.UID, current.Version, uuid.NewString(), operationID)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Release(ctx); !errors.Is(err, ErrConflict) {
		t.Fatal("stale recovery deleted replacement", err)
	}
	if _, err := s.ResumeRecovery(ctx, operationID, first.Proof()); !errors.Is(err, ErrConflict) {
		t.Fatal("old effect resumed with refreshed version", err)
	}
	if err := second.Release(ctx); err != nil {
		t.Fatal(err)
	}
}
func TestMaintenanceRecoveryReconcilesLostMetadataAcknowledgement(t *testing.T) {
	s, c, _, status := stoppedRecoveryFixture(t)
	ctx := context.Background()
	writer := &checkpointUpdateClient{Client: c, lostAck: true}
	s.direct.writer = writer
	recovery, err := s.PrepareRecovery(ctx, status.UID, status.Version, uuid.NewString(), "")
	if err != nil || recovery == nil || writer.writes != 1 {
		t.Fatal("lost fence acknowledgement replayed or unowned", err)
	}
	if err := recovery.Release(ctx); err != nil {
		t.Fatal(err)
	}
}
func TestMaintenanceRecoveryRejectsLiveUnidentifiedAndStaleWriters(t *testing.T) {
	s, _, pod, _ := writerFixture(t)
	ctx := context.Background()
	if _, err := s.AcquireForPod(ctx, MaintenanceUpgrade, pod.Name, string(pod.UID)); err != nil {
		t.Fatal(err)
	}
	status, err := s.Inspect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PrepareRecovery(ctx, status.UID, status.Version, uuid.NewString(), ""); !errors.Is(err, ErrUnavailable) {
		t.Fatal("live writer recovered", err)
	}
	if _, err := s.PrepareRecovery(ctx, status.UID, "stale-version", uuid.NewString(), ""); !errors.Is(err, ErrConflict) {
		t.Fatal("stale observation recovered", err)
	}
	old, _ := fixtureMaintenance(t, false)
	if _, err := old.Acquire(ctx, MaintenanceUpgrade); err != nil {
		t.Fatal(err)
	}
	oldStatus, _ := old.Inspect(ctx)
	if _, err := old.PrepareRecovery(ctx, oldStatus.UID, oldStatus.Version, uuid.NewString(), ""); !errors.Is(err, ErrUnsupported) {
		t.Fatal("unidentified writer recovered", err)
	}
}
func TestMaintenanceRecoveryCapabilityNeverSerializes(t *testing.T) {
	s, _, _, status := stoppedRecoveryFixture(t)
	ctx := context.Background()
	cap, err := s.PrepareRecovery(ctx, status.UID, status.Version, uuid.NewString(), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := json.Marshal(cap); !errors.Is(err, ErrSensitive) {
		t.Fatal("capability serialized", err)
	}
	for _, format := range []string{"%v", "%+v", "%#v", "%q", "%x"} {
		text := fmt.Sprintf(format, cap)
		if !strings.Contains(text, "[redacted]") || strings.Contains(text, cap.(*MaintenanceRecoveryReservation).nonce) || strings.Contains(text, cap.(*MaintenanceRecoveryReservation).operationID) {
			t.Fatal("capability diagnostics exposed ownership")
		}
	}
	// An unrelated annotation change must not silently refresh an admitted proof.
	current := &corev1.Secret{}
	if err := s.direct.reader.Get(ctx, s.key, current); err != nil {
		t.Fatal(err)
	}
	current.Annotations["kyber.io/fixture-change"] = "changed"
	if err := s.direct.writer.Update(ctx, current); err != nil {
		t.Fatal(err)
	}
	if err := cap.Release(ctx); !errors.Is(err, ErrConflict) {
		t.Fatal("admitted version silently refreshed", err)
	}
}

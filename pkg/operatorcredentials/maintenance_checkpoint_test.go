package operatorcredentials

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type checkpointUpdateClient struct {
	client.Client
	lostAck    bool
	afterWrite func()
	writes     int
}

func (c *checkpointUpdateClient) Update(ctx context.Context, object client.Object, options ...client.UpdateOption) error {
	c.writes++
	if err := c.Client.Update(ctx, object, options...); err != nil {
		return err
	}
	if c.afterWrite != nil {
		c.afterWrite()
	}
	if c.lostAck {
		return errors.New("private checkpoint transport diagnostic")
	}
	return nil
}
func checkpointFixture() UpgradeEffectsCheckpoint {
	return UpgradeEffectsCheckpoint{Release: "kyber-fixture", InitialRevision: 7, TargetVersion: "1.9.0", ControlPlaneDeployment: "kyber-fixture-control-plane", Authority: "registry", RegistryUID: uuid.NewString()}
}
func TestMaintenanceCheckpointReconcilesAcknowledgementAndInvalidatesOldHandle(t *testing.T) {
	s, c, pod, _ := writerFixture(t)
	ctx := context.Background()
	original, err := s.AcquireForPod(ctx, MaintenanceUpgrade, pod.Name, string(pod.UID))
	if err != nil {
		t.Fatal(err)
	}
	before := &corev1.Secret{}
	if err := c.Get(ctx, s.key, before); err != nil {
		t.Fatal(err)
	}
	writer := &checkpointUpdateClient{Client: c, lostAck: true}
	s.direct.writer = writer
	checkpoint := checkpointFixture()
	next, err := original.RecordUpgradeEffects(ctx, checkpoint)
	if err != nil {
		t.Fatal("lost acknowledgement not reconciled", err)
	}
	if writer.writes != 1 {
		t.Fatal("checkpoint write replayed")
	}
	if err := original.Check(ctx, MaintenanceUpgrade); !errors.Is(err, ErrConflict) {
		t.Fatal("old capability remained valid", err)
	}
	if err := next.Check(ctx, MaintenanceUpgrade); err != nil {
		t.Fatal(err)
	}
	status, err := s.Inspect(ctx)
	if err != nil || status.Upgrade == nil || !reflect.DeepEqual(*status.Upgrade, checkpoint) {
		t.Fatal("durable effect admission missing", err)
	}
	after := &corev1.Secret{}
	if err := c.Get(ctx, s.key, after); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before.Data, after.Data) {
		t.Fatal("effect admission changed immutable execution metadata")
	}
	if _, err := next.RecordUpgradeEffects(ctx, checkpoint); !errors.Is(err, ErrConflict) {
		t.Fatal("checkpoint could be rewritten", err)
	}
	if writer.writes != 1 {
		t.Fatal("repeated admission attempted a write")
	}
	if err := next.Release(ctx); err != nil {
		t.Fatal(err)
	}
}
func TestMaintenanceCheckpointUnknownReadCannotReleaseWithOldVersion(t *testing.T) {
	s, c, pod, _ := writerFixture(t)
	ctx := context.Background()
	original, err := s.AcquireForPod(ctx, MaintenanceUpgrade, pod.Name, string(pod.UID))
	if err != nil {
		t.Fatal(err)
	}
	s.direct.writer = &checkpointUpdateClient{Client: c, afterWrite: func() { s.direct.reader = &failingReader{Reader: c, failure: errors.New("private read diagnostic")} }}
	if next, err := original.RecordUpgradeEffects(ctx, checkpointFixture()); next != nil || !errors.Is(err, ErrUnavailable) {
		t.Fatal("uncertain admission returned ownership", err)
	}
	s.direct.reader = c
	if err := original.Release(ctx); !errors.Is(err, ErrConflict) {
		t.Fatal("old resource version released uncertain checkpoint", err)
	}
	if status, err := s.Inspect(ctx); err != nil || !status.Present || status.Upgrade == nil {
		t.Fatal("uncertain state disappeared", err)
	}
}
func TestMaintenanceCheckpointRejectsInvalidOrUnboundAdmission(t *testing.T) {
	s, _ := fixtureMaintenance(t, false)
	ctx := context.Background()
	unbound, err := s.Acquire(ctx, MaintenanceUpgrade)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := unbound.RecordUpgradeEffects(ctx, checkpointFixture()); !errors.Is(err, ErrUnsupported) {
		t.Fatal("unidentified writer admitted effects", err)
	}
	for _, bad := range []UpgradeEffectsCheckpoint{{}, {Release: "kyber-fixture", InitialRevision: 1, TargetVersion: "private/raw/value", ControlPlaneDeployment: "cp", Authority: "unselected"}, {Release: "kyber-fixture", InitialRevision: 1, TargetVersion: "1.9.0", ControlPlaneDeployment: "cp", Authority: "registry"}} {
		if _, err := unbound.RecordUpgradeEffects(ctx, bad); !errors.Is(err, ErrInvalid) {
			t.Fatal("invalid checkpoint accepted", err)
		}
	}
	checkpoint := checkpointFixture()
	checkpoint.Authority = "unselected"
	checkpoint.RegistryUID = ""
	if !checkpoint.valid() {
		t.Fatal("known pre-adoption authority mode rejected")
	}
}

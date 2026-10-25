package selfupgrade

import (
	"context"
	"errors"
	"fmt"

	"github.com/matty-v/kyber/pkg/operatorcredentials"
	"k8s.io/client-go/rest"
)

// UpgradeCredentialReservation is held through all supervised upgrade effects,
// verification and rollback. An uncertain operation must retain it for recovery.
type UpgradeCredentialReservation interface {
	Check(context.Context) error
	Release(context.Context) error
}

// UpgradeCredentialCoordinator uses the same installation boundary as adoption.
// Implementations must use direct reads and never substitute bootstrap state
// for an unavailable authority. The live constructor fixes both Secret names.
type UpgradeCredentialCoordinator interface {
	MaintenanceRequired() bool
	Reserve(context.Context) (UpgradeCredentialReservation, error)
	Selected(context.Context) (bool, error)
}

// UpgradeCredentialExecution is supplied by the supervised Job downward API.
// It contains no operator key and is verified against live Pod/Job ownership.
type UpgradeCredentialExecution struct {
	PodName string
	PodUID  string
}

type credentialUpgradeCoordinator struct {
	execution   UpgradeCredentialExecution
	authority   *operatorcredentials.AuthorityStore
	maintenance *operatorcredentials.MaintenanceStore
	protocol    int
}

type UpgradeCredentialEffects struct {
	Release                string
	InitialRevision        int
	TargetVersion          string
	ControlPlaneDeployment string
	Selected               bool
}

// UpgradeCredentialEffectsRecorder must acknowledge the durable checkpoint
// before the supervisor is permitted to apply any CRD or Helm effect.
type UpgradeCredentialEffectsRecorder interface {
	RecordEffects(context.Context, UpgradeCredentialEffects) error
}

type credentialUpgradeReservation struct {
	authority   *operatorcredentials.AuthorityStore
	reservation *operatorcredentials.MaintenanceReservation
}

func NewUpgradeCredentialCoordinator(config *rest.Config, namespace string, execution ...UpgradeCredentialExecution) (UpgradeCredentialCoordinator, error) {
	return newUpgradeCredentialCoordinator(config, namespace, operatorcredentials.RuntimeAuthorityProtocolVersion, execution...)
}

func newUpgradeCredentialCoordinator(config *rest.Config, namespace string, protocol int, executions ...UpgradeCredentialExecution) (UpgradeCredentialCoordinator, error) {
	if len(executions) > 1 {
		return nil, operatorcredentials.ErrInvalid
	}
	var execution UpgradeCredentialExecution
	if len(executions) == 1 {
		execution = executions[0]
	}
	authority, err := operatorcredentials.NewAuthorityStore(config, namespace, operatorcredentials.DefaultSecretName, operatorcredentials.DefaultAuthoritySecretName)
	if err != nil {
		return nil, err
	}
	maintenance, err := operatorcredentials.NewMaintenanceStore(config, namespace)
	if err != nil {
		return nil, err
	}
	return &credentialUpgradeCoordinator{authority: authority, maintenance: maintenance, protocol: protocol, execution: execution}, nil
}

func (c *credentialUpgradeCoordinator) MaintenanceRequired() bool { return c.protocol >= 1 }

func (c *credentialUpgradeCoordinator) Reserve(ctx context.Context) (UpgradeCredentialReservation, error) {
	if !c.MaintenanceRequired() {
		return nil, operatorcredentials.ErrUnsupported
	}
	r, err := c.maintenance.AcquireForPod(ctx, operatorcredentials.MaintenanceUpgrade, c.execution.PodName, c.execution.PodUID)
	if err != nil {
		return nil, fmt.Errorf("reserve operator credential maintenance: %w", err)
	}
	return &credentialUpgradeReservation{reservation: r, authority: c.authority}, nil
}

func (c *credentialUpgradeCoordinator) Selected(ctx context.Context) (bool, error) {
	_, err := c.authority.Load(ctx)
	if err == operatorcredentials.ErrUninitialized {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read operator credential authority: %w", err)
	}
	if !c.MaintenanceRequired() {
		return false, operatorcredentials.ErrUnsupported
	}
	return true, nil
}

func (r *credentialUpgradeReservation) Check(ctx context.Context) error {
	return r.reservation.Check(ctx, operatorcredentials.MaintenanceUpgrade)
}

func (r *credentialUpgradeReservation) Release(ctx context.Context) error {
	return r.reservation.Release(ctx)
}

func (r *credentialUpgradeReservation) RecordEffects(ctx context.Context, effects UpgradeCredentialEffects) error {
	if r == nil || r.authority == nil || r.reservation == nil {
		return operatorcredentials.ErrInvalid
	}
	snapshot, err := r.authority.Load(ctx)
	checkpoint := operatorcredentials.UpgradeEffectsCheckpoint{Release: effects.Release, InitialRevision: effects.InitialRevision, TargetVersion: effects.TargetVersion, ControlPlaneDeployment: effects.ControlPlaneDeployment}
	switch {
	case errors.Is(err, operatorcredentials.ErrUninitialized):
		if effects.Selected {
			return operatorcredentials.ErrUnavailable
		}
		checkpoint.Authority = "unselected"
	case err != nil:
		return err
	default:
		if !effects.Selected {
			return operatorcredentials.ErrConflict
		}
		checkpoint.Authority = "registry"
		checkpoint.RegistryUID = snapshot.UID
	}
	next, err := r.reservation.RecordUpgradeEffects(ctx, checkpoint)
	if err != nil {
		return err
	}
	r.reservation = next
	return nil
}

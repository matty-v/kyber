package selfupgrade

import (
	"context"
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

type credentialUpgradeCoordinator struct {
	authority   *operatorcredentials.AuthorityStore
	maintenance *operatorcredentials.MaintenanceStore
	protocol    int
}

type credentialUpgradeReservation struct {
	reservation *operatorcredentials.MaintenanceReservation
}

func NewUpgradeCredentialCoordinator(config *rest.Config, namespace string) (UpgradeCredentialCoordinator, error) {
	return newUpgradeCredentialCoordinator(config, namespace, operatorcredentials.RuntimeAuthorityProtocolVersion)
}

func newUpgradeCredentialCoordinator(config *rest.Config, namespace string, protocol int) (UpgradeCredentialCoordinator, error) {
	authority, err := operatorcredentials.NewAuthorityStore(config, namespace, operatorcredentials.DefaultSecretName, operatorcredentials.DefaultAuthoritySecretName)
	if err != nil {
		return nil, err
	}
	maintenance, err := operatorcredentials.NewMaintenanceStore(config, namespace)
	if err != nil {
		return nil, err
	}
	return &credentialUpgradeCoordinator{authority: authority, maintenance: maintenance, protocol: protocol}, nil
}

func (c *credentialUpgradeCoordinator) MaintenanceRequired() bool { return c.protocol >= 1 }

func (c *credentialUpgradeCoordinator) Reserve(ctx context.Context) (UpgradeCredentialReservation, error) {
	if !c.MaintenanceRequired() {
		return nil, operatorcredentials.ErrUnsupported
	}
	r, err := c.maintenance.Acquire(ctx, operatorcredentials.MaintenanceUpgrade)
	if err != nil {
		return nil, fmt.Errorf("reserve operator credential maintenance: %w", err)
	}
	return &credentialUpgradeReservation{reservation: r}, nil
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

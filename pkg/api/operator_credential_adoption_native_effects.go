package api

import (
	"context"
	"fmt"
	"io"

	"github.com/matty-v/kyber/pkg/operatorcredentials"
	"github.com/matty-v/kyber/pkg/securityaudit"
	"k8s.io/client-go/rest"
)

// The only production effects adapter. Its private reservation originates from
// the native Job's one-use claim or an exact stopped-writer replacement CAS.
type nativeAdoptionEffects struct {
	registry    *operatorcredentials.SecretStore
	authority   *operatorcredentials.AuthorityStore
	maintenance *operatorcredentials.MaintenanceStore
	reservation *operatorcredentials.MaintenanceReservation
}

func (nativeAdoptionEffects) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, "native credential handoff [redacted]")
}
func (nativeAdoptionEffects) MarshalJSON() ([]byte, error) {
	return nil, operatorcredentials.ErrSensitive
}
func newNativeAdoptionEffects(config *rest.Config, namespace string, maintenance *operatorcredentials.MaintenanceStore, reservation *operatorcredentials.MaintenanceReservation) (*nativeAdoptionEffects, error) {
	if maintenance == nil || reservation == nil || maintenance.Namespace() != namespace {
		return nil, operatorcredentials.ErrInvalid
	}
	registry, err := operatorcredentials.NewSecretStore(config, namespace, operatorcredentials.DefaultSecretName)
	if err != nil {
		return nil, operatorcredentials.ErrUnavailable
	}
	authority, err := operatorcredentials.NewAuthorityStore(config, namespace, operatorcredentials.DefaultSecretName, operatorcredentials.DefaultAuthoritySecretName)
	if err != nil {
		return nil, operatorcredentials.ErrUnavailable
	}
	return &nativeAdoptionEffects{registry: registry, authority: authority, maintenance: maintenance, reservation: reservation}, nil
}
func (e *nativeAdoptionEffects) Check(ctx context.Context) error {
	return e.reservation.Check(ctx, operatorcredentials.MaintenanceAdoption)
}
func (e *nativeAdoptionEffects) ReservationMetadata(ctx context.Context) (securityaudit.CredentialAdoptionEffect, error) {
	if e.Check(ctx) != nil {
		return securityaudit.CredentialAdoptionEffect{}, operatorcredentials.ErrUnavailable
	}
	status, err := e.maintenance.Inspect(ctx)
	if err != nil || !status.Present || status.Kind != operatorcredentials.MaintenanceAdoption || status.Writer == nil || e.Check(ctx) != nil {
		return securityaudit.CredentialAdoptionEffect{}, operatorcredentials.ErrUnavailable
	}
	result := securityaudit.CredentialAdoptionEffect{Stage: "reserved", MaintenanceUID: status.UID, AdmissionVersion: status.AdoptionAdmissionVersion}
	if result.Validate() != nil {
		return securityaudit.CredentialAdoptionEffect{}, operatorcredentials.ErrUnavailable
	}
	return result, nil
}
func (e *nativeAdoptionEffects) Candidate(ctx context.Context) (operatorcredentials.Snapshot, error) {
	return e.registry.Load(ctx)
}
func (e *nativeAdoptionEffects) CreateCandidate(ctx context.Context, document operatorcredentials.Document) (operatorcredentials.Snapshot, error) {
	if err := e.Check(ctx); err != nil {
		return operatorcredentials.Snapshot{}, err
	}
	return e.registry.Create(ctx, document)
}
func (e *nativeAdoptionEffects) Selection(ctx context.Context) (operatorcredentials.AuthoritySelection, error) {
	return e.authority.ReadSelection(ctx)
}
func (e *nativeAdoptionEffects) BindCandidate(ctx context.Context, version string) error {
	return e.authority.Bind(ctx, version, e.reservation)
}
func (e *nativeAdoptionEffects) SealSelection(ctx context.Context, selected operatorcredentials.AuthoritySelection) (operatorcredentials.Snapshot, error) {
	return e.authority.SealSelection(ctx, selected.UID, selected.RegistryUID, e.reservation)
}
func (e *nativeAdoptionEffects) ReplaceSelected(ctx context.Context, snapshot operatorcredentials.Snapshot, document operatorcredentials.Document) (operatorcredentials.Snapshot, error) {
	if err := e.Check(ctx); err != nil {
		return operatorcredentials.Snapshot{}, err
	}
	return e.authority.Replace(ctx, snapshot.UID, snapshot.Version, document)
}
func (e *nativeAdoptionEffects) Release(ctx context.Context) error { return e.reservation.Release(ctx) }

var _ credentialAdoptionEffects = (*nativeAdoptionEffects)(nil)

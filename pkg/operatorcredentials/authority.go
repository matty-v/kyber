package operatorcredentials

import (
	"context"
	"sync/atomic"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	validation "k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/rest"
)

const DefaultAuthoritySecretName = "kyber-operator-credential-authority"
const authorityLabel = "kyber.io/operator-credential-authority"

// AuthorityStore binds adoption to a particular registry Secret incarnation.
// This is an integration primitive, not a runtime activation endpoint. Callers
// must finish compatibility, ownership and audit gates before calling Bind.
// Neither Secret is Helm-managed or garbage-collected with a workload.
type AuthorityStore struct {
	registry *SecretStore
	key      types.NamespacedName
	observed atomic.Bool
}

func NewAuthorityStore(config *rest.Config, namespace, registryName, authorityName string) (*AuthorityStore, error) {
	registry, err := NewSecretStore(config, namespace, registryName)
	if err != nil {
		return nil, err
	}
	return newAuthorityStore(registry, authorityName)
}

func newAuthorityStore(registry *SecretStore, authorityName string) (*AuthorityStore, error) {
	if registry == nil || len(validation.IsDNS1123Subdomain(authorityName)) != 0 || authorityName == registry.key.Name {
		return nil, ErrInvalid
	}
	return &AuthorityStore{registry: registry, key: types.NamespacedName{Namespace: registry.key.Namespace, Name: authorityName}}, nil
}

// Load returns ErrUninitialized ONLY when no adoption marker has been observed
// by this resolver and its authoritative GET says NotFound. All errors after
// adoption are unavailable/unsupported, never a signal to use configured keys.
// No registry document or selection is cached between requests.
func (s *AuthorityStore) Load(ctx context.Context) (Snapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	marker := &corev1.Secret{}
	if err := s.registry.reader.Get(ctx, s.key, marker); err != nil {
		if apierrors.IsNotFound(err) && !s.observed.Load() && ctx.Err() == nil {
			return Snapshot{}, ErrUninitialized
		}
		return Snapshot{}, authorityUnavailable(ctx, err)
	}
	s.observed.Store(true)
	if err := s.validateMarker(marker); err != nil {
		return Snapshot{}, err
	}
	secret, snapshot, err := s.registry.read(ctx)
	if err != nil {
		return Snapshot{}, authorityUnavailable(ctx, err)
	}
	if secret.UID == "" || string(secret.UID) != string(marker.Data["registryUID"]) {
		return Snapshot{}, ErrUnavailable
	}
	return snapshot, nil
}

func (s *AuthorityStore) validateMarker(marker *corev1.Secret) error {
	if marker.Namespace != s.key.Namespace || marker.Name != s.key.Name || marker.UID == "" || marker.ResourceVersion == "" || marker.Type != corev1.SecretTypeOpaque || marker.Immutable == nil || !*marker.Immutable || len(marker.OwnerReferences) != 0 || marker.Labels[authorityLabel] != "v1" || marker.Labels["app.kubernetes.io/managed-by"] != "kyber" {
		return ErrUnsupported
	}
	if len(marker.Data) != 3 || string(marker.Data["protocol"]) != "1" || string(marker.Data["registryName"]) != s.registry.key.Name || !opaque(string(marker.Data["registryUID"])) {
		return ErrUnsupported
	}
	return nil
}

// ReserveAdoption reserves the registry's maintenance boundary. The live
// registry uses DefaultMaintenanceSecretName, also used by upgrade supervision.
// Probe registries have separate names and never reserve live maintenance.
func (s *AuthorityStore) ReserveAdoption(ctx context.Context) (*MaintenanceReservation, error) {
	maintenance, err := newMaintenanceStore(s.registry, s.registry.key.Name+"-maintenance")
	if err != nil {
		return nil, err
	}
	return maintenance.Acquire(ctx, MaintenanceAdoption)
}

// Bind creates an immutable adoption marker, never updates an existing one.
// The expected version comes from the candidate reviewed by the adoption flow.
// A failed/lost response is NOT retried: Load reconciles whether binding won.
// A registry change after the final read is safe: Load always uses its CURRENT
// contents, with the same UID, rather than freezing grants at adoption time.
// The caller must reserve BEFORE adoption preflight and retain the reservation
// until marker creation and every uncertain write are reconciled. Bind never
// releases it on the caller's behalf.
func (s *AuthorityStore) Bind(ctx context.Context, expectedVersion string, reservation *MaintenanceReservation) error {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	if reservation == nil || reservation.store == nil || reservation.store.key.Namespace != s.registry.key.Namespace || reservation.store.key.Name != s.registry.key.Name+"-maintenance" {
		return ErrInvalid
	}
	if err := reservation.Check(ctx, MaintenanceAdoption); err != nil {
		return err
	}
	if expectedVersion == "" || s.observed.Load() {
		return ErrConflict
	}
	secret, snapshot, err := s.registry.read(ctx)
	if err != nil {
		return err
	}
	if snapshot.Version != expectedVersion {
		return ErrConflict
	}
	if secret.UID == "" {
		return ErrInvalid
	}
	if err := reservation.Check(ctx, MaintenanceAdoption); err != nil {
		return err
	}
	immutable := true
	marker := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: s.key.Namespace, Name: s.key.Name, Labels: map[string]string{authorityLabel: "v1", "app.kubernetes.io/managed-by": "kyber"}}, Type: corev1.SecretTypeOpaque, Immutable: &immutable, Data: map[string][]byte{"protocol": []byte("1"), "registryName": []byte(s.registry.key.Name), "registryUID": []byte(secret.UID)}}
	if err := s.registry.writer.Create(ctx, marker); err != nil {
		// An uncertain create may already have committed. Suppress configuration
		// fallback in this process even before a successful reconciliation read.
		s.observed.Store(true)
		return authorityError(ctx, err)
	}
	s.observed.Store(true)
	return nil
}

func authorityUnavailable(ctx context.Context, err error) error {
	safe := authorityError(ctx, err)
	if safe == ErrUninitialized || safe == ErrConflict {
		return ErrUnavailable
	}
	return safe
}

// Replace changes only the currently selected registry incarnation and original
// opaque resource version. It does not authorize the actor or admit audit; those
// gates belong to the management executor. Unknown writes are never retried.
func (s *AuthorityStore) Replace(ctx context.Context, expectedUID, expectedVersion string, next Document) (Snapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	current, err := s.Load(ctx)
	if err != nil {
		return Snapshot{}, authorityUnavailable(ctx, err)
	}
	if expectedUID == "" || expectedVersion == "" || current.UID != expectedUID || current.Version != expectedVersion {
		return Snapshot{}, ErrConflict
	}
	return s.registry.Replace(ctx, expectedVersion, next)
}

// Fence prevents an old CAS from committing only in the selected incarnation.
// A missing/replaced authority cannot be interpreted as a failed key change.
func (s *AuthorityStore) Fence(ctx context.Context, expectedUID, expectedVersion string) (Snapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	current, err := s.Load(ctx)
	if err != nil {
		return Snapshot{}, authorityUnavailable(ctx, err)
	}
	if expectedUID == "" || expectedVersion == "" || current.UID != expectedUID || current.Version != expectedVersion {
		return Snapshot{}, ErrConflict
	}
	return s.registry.Fence(ctx, expectedVersion)
}

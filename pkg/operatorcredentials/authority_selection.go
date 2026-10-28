package operatorcredentials

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

const selectedAuthorityAnnotation = "kyber.io/operator-credential-selected-authority"

// AuthoritySelection is object metadata, never accepted key material.
type AuthoritySelection struct {
	UID         string
	RegistryUID string
}

// bootstrapState distinguishes an unselected candidate from durable completion.
// A sealed registry prevents a fresh process from returning to configuration if
// its completed authority marker is missing. An unsealed candidate does not
// strand the administrator before selection or prevent explicit recovery.
func (s *AuthorityStore) bootstrapState(ctx context.Context) error {
	if s.observed.Load() {
		return ErrUnavailable
	}
	registry := &corev1.Secret{}
	if err := s.registry.reader.Get(ctx, s.registry.key, registry); err != nil {
		if apierrors.IsNotFound(err) && ctx.Err() == nil {
			return ErrUninitialized
		}
		return authorityUnavailable(ctx, err)
	}
	if ctx.Err() != nil {
		return ErrUnavailable
	}
	if _, sealed := registry.Annotations[selectedAuthorityAnnotation]; sealed {
		return ErrUnavailable
	}
	return ErrUninitialized
}

// ReadSelection does not prove current registry contents or authorize a write.
func (s *AuthorityStore) ReadSelection(ctx context.Context) (AuthoritySelection, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	marker := &corev1.Secret{}
	if err := s.registry.reader.Get(ctx, s.key, marker); err != nil {
		if apierrors.IsNotFound(err) && ctx.Err() == nil {
			return AuthoritySelection{}, s.bootstrapState(ctx)
		}
		return AuthoritySelection{}, authorityUnavailable(ctx, err)
	}
	s.observed.Store(true)
	if err := s.validateMarker(marker); err != nil {
		return AuthoritySelection{}, err
	}
	return AuthoritySelection{UID: string(marker.UID), RegistryUID: string(marker.Data["registryUID"])}, nil
}

// SealSelection durably records the exact selected marker before adoption may
// be declared complete. It changes metadata only at the read registry version,
// preserving current contents and all operation references. Unknown responses
// are reconciled by observing this same UID pair; never replay an issuance.
func (s *AuthorityStore) SealSelection(ctx context.Context, markerUID, registryUID string, reservation *MaintenanceReservation) (Snapshot, error) {
	if s == nil || !opaque(markerUID) || !opaque(registryUID) || reservation == nil || reservation.store == nil || reservation.store.key.Namespace != s.registry.key.Namespace || reservation.store.key.Name != s.registry.key.Name+"-maintenance" {
		return Snapshot{}, ErrInvalid
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	if err := reservation.Check(ctx, MaintenanceAdoption); err != nil {
		return Snapshot{}, err
	}
	selected, err := s.ReadSelection(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	if selected.UID != markerUID || selected.RegistryUID != registryUID {
		return Snapshot{}, ErrConflict
	}
	secret, snapshot, err := s.registry.read(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	if snapshot.UID != registryUID {
		return Snapshot{}, ErrConflict
	}
	if previous, found := secret.Annotations[selectedAuthorityAnnotation]; found {
		if previous != markerUID {
			return Snapshot{}, ErrConflict
		}
		return s.Load(ctx)
	}
	if err := reservation.Check(ctx, MaintenanceAdoption); err != nil {
		return Snapshot{}, err
	}
	if secret.Annotations == nil {
		secret.Annotations = map[string]string{}
	}
	secret.Annotations[selectedAuthorityAnnotation] = markerUID
	writeErr := s.registry.writer.Update(ctx, secret)
	confirmed := &corev1.Secret{}
	if err := s.registry.reader.Get(ctx, s.registry.key, confirmed); err != nil {
		return Snapshot{}, authorityUnavailable(ctx, err)
	}
	if string(confirmed.UID) != registryUID || confirmed.Annotations[selectedAuthorityAnnotation] != markerUID {
		if writeErr != nil {
			return Snapshot{}, authorityError(ctx, writeErr)
		}
		return Snapshot{}, ErrConflict
	}
	return s.Load(ctx)
}

// VerifyCompletedSelection verifies durable completion without reconstructing a
// private reservation or allowing a new write. Missing receipts stay unavailable.
func (s *AuthorityStore) VerifyCompletedSelection(ctx context.Context, expected AuthoritySelection) (Snapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	selected, err := s.ReadSelection(ctx)
	if err != nil || selected != expected {
		return Snapshot{}, ErrUnavailable
	}
	secret, snapshot, err := s.registry.read(ctx)
	if err != nil || snapshot.UID != expected.RegistryUID || secret.Annotations[selectedAuthorityAnnotation] != expected.UID {
		return Snapshot{}, ErrUnavailable
	}
	return snapshot, nil
}

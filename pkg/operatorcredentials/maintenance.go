package operatorcredentials

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	validation "k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	DefaultMaintenanceSecretName = DefaultSecretName + "-maintenance"
	MaintenanceAdoption          = "adoption"
	MaintenanceUpgrade           = "upgrade"
	maintenanceLabel             = "kyber.io/operator-credential-maintenance"
)

// MaintenanceStore coordinates adoption with upgrades across processes. The
// reservation deliberately has no expiry: an expired lease cannot stop a Helm
// process or an API write already in flight. A crash therefore blocks the next
// operation until recovery proves the original writer has stopped.
//
// This primitive does not activate adoption or replace compatibility checks.
// Every cooperating entrypoint must reserve before its first authoritative
// read and retain the reservation through rollback and uncertain-write recovery.
type MaintenanceStore struct {
	direct *SecretStore
	key    types.NamespacedName
}

func NewMaintenanceStore(config *rest.Config, namespace string) (*MaintenanceStore, error) {
	direct, err := NewSecretStore(config, namespace, DefaultSecretName)
	if err != nil {
		return nil, err
	}
	return newMaintenanceStore(direct, DefaultMaintenanceSecretName)
}

func newMaintenanceStore(direct *SecretStore, name string) (*MaintenanceStore, error) {
	if direct == nil || len(validation.IsDNS1123Subdomain(name)) != 0 || name == direct.key.Name {
		return nil, ErrInvalid
	}
	return &MaintenanceStore{direct: direct, key: types.NamespacedName{Namespace: direct.key.Namespace, Name: name}}, nil
}

// MaintenanceReservation is a private ownership capability, not an operator
// credential. Do not serialize it into API responses or diagnostics.
type MaintenanceReservation struct {
	store   *MaintenanceStore
	owner   string
	kind    string
	uid     types.UID
	version string
}

func (MaintenanceReservation) String() string               { return "maintenance reservation [redacted]" }
func (r MaintenanceReservation) GoString() string           { return r.String() }
func (r MaintenanceReservation) Format(s fmt.State, _ rune) { _, _ = io.WriteString(s, r.String()) }
func (MaintenanceReservation) MarshalJSON() ([]byte, error) { return nil, ErrSensitive }
func (*MaintenanceReservation) UnmarshalJSON([]byte) error  { return ErrSensitive }

// Acquire generates a fresh execution UUID and reconciles a lost create
// acknowledgement within this call. An existing owner is never displaced,
// regardless of how old its reservation is. Only this exact immutable object's
// UID and resource version may subsequently be released.
func (s *MaintenanceStore) Acquire(ctx context.Context, kind string) (*MaintenanceReservation, error) {
	if s == nil || s.direct == nil || (kind != MaintenanceAdoption && kind != MaintenanceUpgrade) {
		return nil, ErrInvalid
	}
	owner := uuid.NewString()
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	immutable := true
	reservation := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Namespace: s.key.Namespace, Name: s.key.Name,
		Labels: map[string]string{maintenanceLabel: "v1", "app.kubernetes.io/managed-by": "kyber"},
	}, Type: corev1.SecretTypeOpaque, Immutable: &immutable, Data: map[string][]byte{
		"protocol": []byte("1"), "owner": []byte(owner), "kind": []byte(kind),
		"startedAt": []byte(time.Now().UTC().Format(time.RFC3339Nano)),
	}}
	createErr := s.direct.writer.Create(ctx, reservation)
	// Read even after success: reconcile a lost acknowledgement, and use actual
	// server-assigned identity rather than assuming a successful response body.
	current := &corev1.Secret{}
	if err := s.direct.reader.Get(ctx, s.key, current); err != nil {
		return nil, errors.Join(authorityUnavailable(ctx, createErr), authorityUnavailable(ctx, err))
	}
	if err := s.validate(current); err != nil {
		return nil, err
	}
	if string(current.Data["owner"]) != owner || string(current.Data["kind"]) != kind {
		return nil, ErrConflict
	}
	// Matching ownership reconciles both AlreadyExists and uncertain creates.
	// A non-conflict API error cannot confer ownership without this fresh read.
	return &MaintenanceReservation{store: s, owner: owner, kind: kind, uid: current.UID, version: current.ResourceVersion}, nil
}

func (s *MaintenanceStore) validate(secret *corev1.Secret) error {
	if secret.DeletionTimestamp != nil {
		return ErrUnavailable
	}
	if secret.Namespace != s.key.Namespace || secret.Name != s.key.Name || secret.UID == "" || secret.ResourceVersion == "" || secret.Type != corev1.SecretTypeOpaque || secret.Immutable == nil || !*secret.Immutable || len(secret.OwnerReferences) != 0 || len(secret.Finalizers) != 0 || secret.Labels[maintenanceLabel] != "v1" || secret.Labels["app.kubernetes.io/managed-by"] != "kyber" || len(secret.Data) != 4 || string(secret.Data["protocol"]) != "1" {
		return ErrUnsupported
	}
	owner, err := uuid.Parse(string(secret.Data["owner"]))
	if err != nil || owner == uuid.Nil || owner.String() != string(secret.Data["owner"]) {
		return ErrUnsupported
	}
	kind := string(secret.Data["kind"])
	if kind != MaintenanceAdoption && kind != MaintenanceUpgrade {
		return ErrUnsupported
	}
	if _, err := time.Parse(time.RFC3339Nano, string(secret.Data["startedAt"])); err != nil {
		return ErrUnsupported
	}
	return nil
}

// Check verifies the same reservation still exists. It never renews, replaces,
// or takes over an object. Its caller must not continue after a failed check.
func (r *MaintenanceReservation) Check(ctx context.Context, kind string) error {
	if r == nil || r.store == nil || r.kind != kind || r.uid == "" || r.version == "" {
		return ErrInvalid
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	current := &corev1.Secret{}
	if err := r.store.direct.reader.Get(ctx, r.store.key, current); err != nil {
		return authorityUnavailable(ctx, err)
	}
	if err := r.store.validate(current); err != nil {
		return err
	}
	if current.UID != r.uid || current.ResourceVersion != r.version || string(current.Data["owner"]) != r.owner || string(current.Data["kind"]) != r.kind {
		return ErrConflict
	}
	return nil
}

// Release is explicit: a caller with an uncertain external write must retain
// the reservation until that write is reconciled. Never defer Release around
// an operation that can return while Helm or an API write may still commit.
func (r *MaintenanceReservation) Release(ctx context.Context) error {
	if r == nil || r.store == nil || r.uid == "" || r.version == "" {
		return ErrInvalid
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	if err := r.Check(ctx, r.kind); err != nil {
		return err
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: r.store.key.Namespace, Name: r.store.key.Name}}
	err := r.store.direct.writer.Delete(ctx, secret, &client.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &r.uid, ResourceVersion: &r.version}})
	if apierrors.IsNotFound(err) && ctx.Err() == nil {
		return nil
	}
	return authorityError(ctx, err)
}

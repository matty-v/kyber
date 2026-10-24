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
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	validation "k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	DefaultSecretName    = "kyber-operator-credentials"
	dataKey              = "registry.json"
	ownerLabel           = "kyber.io/operator-credential-registry"
	requestTimeout       = 2 * time.Second
	writeFenceAnnotation = "kyber.io/operator-credential-write-fence"
)

type Snapshot struct {
	UID      string
	Version  string
	Document Document
}

// Fence advances only metadata at an expected resource version. Recovery uses
// this after an uncertain credential update: either that update already won,
// or this fence prevents its older compare-and-swap from committing later.
// Never replay key issuance to resolve an unknown response. Load the current
// authority and operation outbox after conflicts or lost fence acknowledgements.
// This is not operator authorization; the admitted mutation owns that check.
func (s *SecretStore) Fence(ctx context.Context, version string) (Snapshot, error) {
	if version == "" {
		return Snapshot{}, ErrConflict
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	secret, previous, err := s.read(ctx)
	if err != nil {
		if errors.Is(err, ErrUninitialized) {
			return Snapshot{}, ErrUnavailable
		}
		return Snapshot{}, err
	}
	if previous.Version != version {
		return Snapshot{}, ErrConflict
	}
	if secret.UID == "" {
		return Snapshot{}, ErrUnavailable
	}
	if secret.Annotations == nil {
		secret.Annotations = map[string]string{}
	}
	secret.Annotations[writeFenceAnnotation] = uuid.NewString()
	if err := s.writer.Update(ctx, secret); err != nil {
		return Snapshot{}, authorityError(ctx, err)
	}
	if secret.ResourceVersion == "" || secret.ResourceVersion == version {
		return Snapshot{}, ErrUnavailable
	}
	return Snapshot{UID: string(secret.UID), Version: secret.ResourceVersion, Document: previous.Document.Clone()}, nil
}

func (Snapshot) String() string                   { return "operator credential snapshot [redacted]" }
func (s Snapshot) GoString() string               { return s.String() }
func (s Snapshot) Format(state fmt.State, _ rune) { _, _ = io.WriteString(state, s.String()) }
func (Snapshot) MarshalJSON() ([]byte, error)     { return nil, ErrSensitive }

// SecretStore uses a direct client for one installation, without an informer or
// last-successful-document cache. Its public factory fixes Secret mapping so
// discovery cannot extend an otherwise bounded authority read.
type SecretStore struct {
	reader client.Reader
	writer client.Client
	key    types.NamespacedName
}

func NewSecretStore(config *rest.Config, namespace, name string) (*SecretStore, error) {
	if config == nil {
		return nil, ErrUnavailable
	}
	scheme := runtime.NewScheme()
	if corev1.AddToScheme(scheme) != nil {
		return nil, ErrUnavailable
	}
	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{corev1.SchemeGroupVersion})
	mapper.Add(corev1.SchemeGroupVersion.WithKind("Secret"), meta.RESTScopeNamespace)
	boundedConfig := rest.CopyConfig(config)
	if boundedConfig.Timeout == 0 || boundedConfig.Timeout > requestTimeout {
		boundedConfig.Timeout = requestTimeout
	}
	direct, err := client.New(boundedConfig, client.Options{Scheme: scheme, Mapper: mapper})
	if err != nil {
		return nil, ErrUnavailable
	}
	return newSecretStore(direct, direct, namespace, name)
}

// Injection is private to package tests, rather than an alternate runtime
// constructor that could accidentally receive a cached manager client.
func newSecretStore(reader client.Reader, writer client.Client, namespace, name string) (*SecretStore, error) {
	if reader == nil || writer == nil {
		return nil, ErrUnavailable
	}
	if len(validation.IsDNS1123Label(namespace)) != 0 || len(validation.IsDNS1123Subdomain(name)) != 0 {
		return nil, ErrInvalid
	}
	return &SecretStore{reader: reader, writer: writer, key: types.NamespacedName{Namespace: namespace, Name: name}}, nil
}

func (s *SecretStore) read(ctx context.Context) (*corev1.Secret, Snapshot, error) {
	secret := &corev1.Secret{}
	if err := s.reader.Get(ctx, s.key, secret); err != nil {
		return nil, Snapshot{}, authorityError(ctx, err)
	}
	if secret.Namespace != s.key.Namespace || secret.Name != s.key.Name {
		return nil, Snapshot{}, ErrInvalid
	}
	if secret.Type != corev1.SecretTypeOpaque || secret.Labels[ownerLabel] != "v1" || secret.Labels["app.kubernetes.io/managed-by"] != "kyber" || len(secret.OwnerReferences) != 0 || secret.ResourceVersion == "" {
		return nil, Snapshot{}, ErrUnsupported
	}
	d, err := decode(secret.Data[dataKey])
	if err != nil {
		return nil, Snapshot{}, err
	}
	if d.Namespace != s.key.Namespace {
		return nil, Snapshot{}, ErrInvalid
	}
	return secret, Snapshot{UID: string(secret.UID), Version: secret.ResourceVersion, Document: d}, nil
}

func (s *SecretStore) Load(ctx context.Context) (Snapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	_, snapshot, err := s.read(ctx)
	return snapshot, err
}

func (s *SecretStore) Create(ctx context.Context, d Document) (Snapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	if d.Namespace != s.key.Namespace {
		return Snapshot{}, ErrInvalid
	}
	raw, d, err := encode(d)
	if err != nil {
		return Snapshot{}, err
	}
	if err = transition(nil, d); err != nil {
		return Snapshot{}, err
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: s.key.Name, Namespace: s.key.Namespace, Labels: map[string]string{ownerLabel: "v1", "app.kubernetes.io/managed-by": "kyber"}}, Type: corev1.SecretTypeOpaque, Data: map[string][]byte{dataKey: raw}}
	if err = s.writer.Create(ctx, secret); err != nil {
		return Snapshot{}, authorityError(ctx, err)
	}
	return Snapshot{UID: string(secret.UID), Version: secret.ResourceVersion, Document: d}, nil
}

func (s *SecretStore) Replace(ctx context.Context, version string, next Document) (Snapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	if version == "" {
		return Snapshot{}, ErrConflict
	}
	secret, previous, err := s.read(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	if version != previous.Version {
		return Snapshot{}, ErrConflict
	}
	if secret.Immutable != nil && *secret.Immutable {
		return Snapshot{}, ErrUnsupported
	}
	if next.Namespace != s.key.Namespace {
		return Snapshot{}, ErrInvalid
	}
	raw, next, err := encode(next)
	if err != nil {
		return Snapshot{}, err
	}
	if err = transition(&previous.Document, next); err != nil {
		return Snapshot{}, err
	}
	// Retain UID, resourceVersion, annotations and unrelated metadata. A writer
	// racing the authoritative read loses at the API server, without a retry.
	secret.Data[dataKey] = raw
	if err = s.writer.Update(ctx, secret); err != nil {
		return Snapshot{}, authorityError(ctx, err)
	}
	return Snapshot{UID: string(secret.UID), Version: secret.ResourceVersion, Document: next}, nil
}

func authorityError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return errors.Join(ErrUnavailable, ctx.Err())
	}
	if errors.Is(err, context.Canceled) {
		return errors.Join(ErrUnavailable, context.Canceled)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return errors.Join(ErrUnavailable, context.DeadlineExceeded)
	}
	switch {
	case apierrors.IsNotFound(err):
		return ErrUninitialized
	case apierrors.IsConflict(err), apierrors.IsAlreadyExists(err):
		return ErrConflict
	default:
		return ErrUnavailable // API status details may contain request data.
	}
}

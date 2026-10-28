package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/matty-v/kyber/pkg/operatorcredentials"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	validation "k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var errCredentialAdoptionSources = errors.New("configured credential sources could not be verified for adoption")

const credentialSourceReadTimeout = 2 * time.Second
const credentialSourceSnapshotTimeout = 30 * time.Second
const maxCredentialSourceDefinitions = 1024

type credentialSourceVersion struct {
	uid     types.UID
	version string
}
type credentialAdoptionSources struct {
	rootName  string
	root      string
	callers   []ScopedCaller
	namespace string
	versions  map[types.NamespacedName]credentialSourceVersion
	reader    client.Client
}

func (credentialAdoptionSources) String() string     { return "credential adoption sources [redacted]" }
func (s credentialAdoptionSources) GoString() string { return s.String() }
func (s credentialAdoptionSources) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, s.String())
}
func (credentialAdoptionSources) MarshalJSON() ([]byte, error) {
	return nil, operatorcredentials.ErrSensitive
}
func (*credentialAdoptionSources) UnmarshalJSON([]byte) error {
	return operatorcredentials.ErrSensitive
}

type credentialAdoptionSourceReader struct {
	reader          client.Client
	namespace, name string
}

func newCredentialAdoptionSourceReader(config *rest.Config, namespace, name string) (*credentialAdoptionSourceReader, error) {
	if config == nil || !validCredentialSourceLocation(namespace, name) {
		return nil, operatorcredentials.ErrInvalid
	}
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{corev1.SchemeGroupVersion})
	mapper.Add(corev1.SchemeGroupVersion.WithKind("Secret"), meta.RESTScopeNamespace)
	bounded := rest.CopyConfig(config)
	if bounded.Timeout == 0 || bounded.Timeout > credentialSourceReadTimeout {
		bounded.Timeout = credentialSourceReadTimeout
	}
	reader, err := client.New(bounded, client.Options{Scheme: scheme, Mapper: mapper})
	if err != nil {
		return nil, errCredentialAdoptionSources
	}
	return &credentialAdoptionSourceReader{reader: reader, namespace: namespace, name: name}, nil
}
func validCredentialSourceLocation(namespace, name string) bool {
	return len(validation.IsDNS1123Label(namespace)) == 0 && len(validation.IsDNS1123Subdomain(name)) == 0 && name != operatorcredentials.DefaultSecretName && name != operatorcredentials.DefaultAuthoritySecretName && name != operatorcredentials.DefaultMaintenanceSecretName
}

type trackedCredentialSourceClient struct {
	client.Client
	namespace string
	versions  map[types.NamespacedName]credentialSourceVersion
}

func (c *trackedCredentialSourceClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	secret, ok := obj.(*corev1.Secret)
	if !ok || key.Namespace != c.namespace || len(validation.IsDNS1123Subdomain(key.Name)) != 0 {
		return errCredentialAdoptionSources
	}
	bounded, cancel := context.WithTimeout(ctx, credentialSourceReadTimeout)
	defer cancel()
	if err := c.Client.Get(bounded, key, obj, opts...); err != nil || bounded.Err() != nil {
		return errCredentialAdoptionSources
	}
	if secret.Namespace != key.Namespace || secret.Name != key.Name || secret.UID == "" || secret.ResourceVersion == "" || secret.DeletionTimestamp != nil {
		return errCredentialAdoptionSources
	}
	version := credentialSourceVersion{uid: secret.UID, version: secret.ResourceVersion}
	if previous, exists := c.versions[key]; exists && previous != version {
		return errCredentialAdoptionSources
	}
	c.versions[key] = version
	return nil
}
func (r *credentialAdoptionSourceReader) Read(ctx context.Context) (*credentialAdoptionSources, error) {
	if r == nil || r.reader == nil || !validCredentialSourceLocation(r.namespace, r.name) {
		return nil, operatorcredentials.ErrInvalid
	}
	ctx, cancel := context.WithTimeout(ctx, credentialSourceSnapshotTimeout)
	defer cancel()
	tracked := &trackedCredentialSourceClient{Client: r.reader, namespace: r.namespace, versions: map[types.NamespacedName]credentialSourceVersion{}}
	root := &corev1.Secret{}
	if err := tracked.Get(ctx, types.NamespacedName{Namespace: r.namespace, Name: r.name}, root); err != nil {
		return nil, errCredentialAdoptionSources
	}
	material := string(root.Data[apiKeySecretField])
	if len(material) > 4096 || !validOperatorHTTPKey(material) || len(root.Data["callers"]) > 1<<20 {
		return nil, errCredentialAdoptionSources
	}
	declared, err := ParseScopedCallers(string(root.Data["callers"]))
	if err != nil || len(declared) > maxCredentialSourceDefinitions {
		return nil, errCredentialAdoptionSources
	}
	resolved, rejected := ResolveScopedCallers(ctx, tracked, r.namespace, declared)
	if len(rejected) != 0 || len(resolved) != len(declared) || ctx.Err() != nil {
		return nil, errCredentialAdoptionSources
	}
	for _, caller := range resolved {
		if len(caller.Key) > 4096 || !validOperatorHTTPKey(caller.Key) {
			return nil, errCredentialAdoptionSources
		}
	}
	snapshot := &credentialAdoptionSources{rootName: r.name, root: material, callers: resolved, namespace: r.namespace, versions: tracked.versions, reader: r.reader}
	if err := snapshot.Verify(ctx); err != nil {
		return nil, errCredentialAdoptionSources
	}
	return snapshot, nil
}
func (s *credentialAdoptionSources) Verify(ctx context.Context) error {
	if s == nil || s.reader == nil || s.namespace == "" || len(s.versions) == 0 {
		return operatorcredentials.ErrInvalid
	}
	ctx, cancel := context.WithTimeout(ctx, credentialSourceSnapshotTimeout)
	defer cancel()
	for key, expected := range s.versions {
		if key.Namespace != s.namespace || len(validation.IsDNS1123Subdomain(key.Name)) != 0 {
			return errCredentialAdoptionSources
		}
		current := &corev1.Secret{}
		bounded, done := context.WithTimeout(ctx, credentialSourceReadTimeout)
		err := s.reader.Get(bounded, key, current)
		expired := bounded.Err() != nil
		done()
		if err != nil || expired || current.Namespace != key.Namespace || current.Name != key.Name || current.UID != expected.uid || current.ResourceVersion != expected.version || current.DeletionTimestamp != nil {
			return errCredentialAdoptionSources
		}
	}
	return nil
}

var _ json.Marshaler = credentialAdoptionSources{}

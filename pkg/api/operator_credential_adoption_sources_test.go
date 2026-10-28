package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"time"

	"k8s.io/client-go/rest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/matty-v/kyber/pkg/operatorcredentials"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func credentialSourcesFixture(t *testing.T) (*credentialAdoptionSourceReader, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	callers, err := json.Marshal([]ScopedCaller{
		{Name: "inline", Key: "key-test", PrincipalID: "p-test", TenantID: "t-test", CredentialID: "c-test", CredentialGeneration: 1, Scopes: []string{"metrics:read"}},
		{Name: "reference", KeyFrom: &SecretKeyRef{Secret: "reference-source", Key: "value"}, PrincipalID: "p-ref", TenantID: "t-ref", CredentialID: "c-ref", CredentialGeneration: 1, AgentResources: []string{"kyber-system/alpha"}, Scopes: []string{"tasks:read"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	root := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "kyber-system", Name: "root-source", UID: "11111111-1111-4111-8111-111111111111"}, Data: map[string][]byte{"api-key": []byte("root-test"), "callers": callers}}
	reference := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "kyber-system", Name: "reference-source", UID: "22222222-2222-4222-8222-222222222222"}, Data: map[string][]byte{"value": []byte("  ref-test\n")}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(root, reference).Build()
	return &credentialAdoptionSourceReader{reader: c, namespace: "kyber-system", name: "root-source"}, c
}
func TestCredentialAdoptionSourcesMatchStartupAndRemainPrivate(t *testing.T) {
	r, _ := credentialSourcesFixture(t)
	s, err := r.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if s.root != "root-test" || len(s.callers) != 2 || s.callers[0].Key != "key-test" || s.callers[1].Key != "ref-test" || len(s.versions) != 2 {
		t.Fatal("source snapshot changed startup semantics")
	}
	for _, format := range []string{"%s", "%v", "%+v", "%#v", "%q"} {
		value := fmt.Sprintf(format, s)
		for _, material := range []string{"root-test", "key-test", "ref-test"} {
			if strings.Contains(value, material) {
				t.Fatal("private source formatting leaked material")
			}
		}
	}
	if _, err := json.Marshal(s); !errors.Is(err, operatorcredentials.ErrSensitive) {
		t.Fatal("private source serialized", err)
	}
	if err := json.Unmarshal([]byte(`{}`), s); !errors.Is(err, operatorcredentials.ErrSensitive) {
		t.Fatal("private source accepted JSON", err)
	}
}
func TestCredentialAdoptionSourcesRejectChangedReplacedMissingAndUnavailable(t *testing.T) {
	for _, scenario := range []string{"root-data", "reference-data", "same-data-metadata-change", "same-version-new-uid", "missing", "unavailable", "cancelled"} {
		t.Run(scenario, func(t *testing.T) {
			r, c := credentialSourcesFixture(t)
			ctx := context.Background()
			s, err := r.Read(ctx)
			if err != nil {
				t.Fatal(err)
			}
			key := client.ObjectKey{Namespace: r.namespace, Name: "reference-source"}
			secret := &corev1.Secret{}
			if scenario == "root-data" {
				key.Name = r.name
			}
			if err := c.Get(ctx, key, secret); err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "root-data", "reference-data":
				secret.Data["value"] = []byte("next-test")
				if err := c.Update(ctx, secret); err != nil {
					t.Fatal(err)
				}
			case "same-data-metadata-change":
				secret.Annotations = map[string]string{"fixture": "changed"}
				if err := c.Update(ctx, secret); err != nil {
					t.Fatal(err)
				}
			case "same-version-new-uid":
				s.versions[key] = credentialSourceVersion{uid: types.UID("33333333-3333-4333-8333-333333333333"), version: secret.ResourceVersion}
			case "missing":
				if err := c.Delete(ctx, secret); err != nil {
					t.Fatal(err)
				}
			case "unavailable":
				s.reader = &unavailableCredentialSourceClient{Client: c}
			case "cancelled":
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cancelled
			}
			if err := s.Verify(ctx); !errors.Is(err, errCredentialAdoptionSources) {
				t.Fatal("changed source accepted", err)
			}
		})
	}
}

type unavailableCredentialSourceClient struct{ client.Client }

func (*unavailableCredentialSourceClient) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	return errors.New("private transport diagnostic root-test ref-test")
}
func TestCredentialAdoptionSourcesRefusePartialOrMalformedInputsWithoutDiagnostics(t *testing.T) {
	for _, scenario := range []string{"malformed", "missing-reference", "missing-field", "empty-root", "invalid-root", "cross-namespace-reference", "unavailable"} {
		t.Run(scenario, func(t *testing.T) {
			r, c := credentialSourcesFixture(t)
			ctx := context.Background()
			root := &corev1.Secret{}
			if err := c.Get(ctx, client.ObjectKey{Namespace: r.namespace, Name: r.name}, root); err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "malformed":
				root.Data["callers"] = []byte(`[{"key":"bad-test"`)
			case "empty-root":
				root.Data["api-key"] = nil
			case "invalid-root":
				root.Data["api-key"] = []byte("root-test\n")
			case "cross-namespace-reference":
				root.Data["callers"] = []byte(`[{"name":"reference","keyFrom":{"secret":"other/reference-source","key":"value"},"scopes":[]}]`)
			case "missing-field":
				root.Data["callers"] = []byte(`[{"name":"reference","keyFrom":{"secret":"reference-source","key":"missing"},"scopes":[]}]`)
			case "missing-reference":
				if err := c.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: r.namespace, Name: "reference-source"}}); err != nil {
					t.Fatal(err)
				}
			case "unavailable":
				r.reader = &unavailableCredentialSourceClient{Client: c}
			}
			if err := c.Update(ctx, root); err != nil {
				t.Fatal(err)
			}
			snapshot, err := r.Read(ctx)
			if snapshot != nil || !errors.Is(err, errCredentialAdoptionSources) {
				t.Fatal("partial or malformed source accepted", err)
			}
			for _, value := range []string{"bad-test", "root-test", "ref-test", "private transport"} {
				if strings.Contains(err.Error(), value) {
					t.Fatal("source diagnostic leaked private content")
				}
			}
		})
	}
}

type changingCredentialSourceClient struct {
	client.Client
	reads int
}

func (c *changingCredentialSourceClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	c.reads++
	if c.reads == 2 {
		root := &corev1.Secret{}
		rootKey := client.ObjectKey{Namespace: key.Namespace, Name: "root-source"}
		if err := c.Client.Get(ctx, rootKey, root); err != nil {
			return err
		}
		root.Annotations = map[string]string{"fixture": "changed-during-read"}
		if err := c.Client.Update(ctx, root); err != nil {
			return err
		}
	}
	return c.Client.Get(ctx, key, obj, opts...)
}
func TestCredentialAdoptionSourcesDetectChangeDuringCollection(t *testing.T) {
	r, c := credentialSourcesFixture(t)
	r.reader = &changingCredentialSourceClient{Client: c}
	if snapshot, err := r.Read(context.Background()); snapshot != nil || !errors.Is(err, errCredentialAdoptionSources) {
		t.Fatal("mixed source snapshot returned", err)
	}
}

func TestCredentialAdoptionSourcesDoNotReusePreviousSuccess(t *testing.T) {
	r, c := credentialSourcesFixture(t)
	if _, err := r.Read(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.reader = &unavailableCredentialSourceClient{Client: c}
	if snapshot, err := r.Read(context.Background()); snapshot != nil || !errors.Is(err, errCredentialAdoptionSources) {
		t.Fatal("last-good input reused", err)
	}
}
func TestCredentialAdoptionSourceConstructorUsesBoundedDirectSecretReads(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != "GET" || r.URL.Path != "/api/v1/namespaces/kyber-system/secrets/root-source" {
			t.Error("unexpected discovery, namespace or mutation request")
			w.WriteHeader(500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(&corev1.Secret{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"}, ObjectMeta: metav1.ObjectMeta{Namespace: "kyber-system", Name: "root-source", UID: "11111111-1111-4111-8111-111111111111", ResourceVersion: "opaque-version"}, Data: map[string][]byte{"api-key": []byte("root-test")}})
	}))
	defer server.Close()
	reader, err := newCredentialAdoptionSourceReader(&rest.Config{Host: server.URL, Timeout: time.Hour}, "kyber-system", "root-source")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reader.Read(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatal("expected collection and recheck without discovery", calls.Load())
	}
}
func TestCredentialAdoptionSourceLocationsAndCancelledReadFailClosed(t *testing.T) {
	for _, location := range []struct{ namespace, name string }{{"other/namespace", "root-source"}, {"kyber-system", "other/source"}, {"kyber-system", operatorcredentials.DefaultSecretName}, {"kyber-system", operatorcredentials.DefaultAuthoritySecretName}, {"kyber-system", operatorcredentials.DefaultMaintenanceSecretName}} {
		if _, err := newCredentialAdoptionSourceReader(&rest.Config{Host: "http://127.0.0.1"}, location.namespace, location.name); !errors.Is(err, operatorcredentials.ErrInvalid) {
			t.Fatal("unsafe source location accepted", err)
		}
	}
	if _, err := newCredentialAdoptionSourceReader(nil, "kyber-system", "root-source"); !errors.Is(err, operatorcredentials.ErrInvalid) {
		t.Fatal("nil configuration accepted")
	}
	reader, _ := credentialSourcesFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if snapshot, err := reader.Read(ctx); snapshot != nil || !errors.Is(err, errCredentialAdoptionSources) {
		t.Fatal("cancelled collection returned input", err)
	}
}

func TestCredentialAdoptionSourcesRejectChangedRepeatedReference(t *testing.T) {
	r, c := credentialSourcesFixture(t)
	ctx := context.Background()
	root := &corev1.Secret{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: r.namespace, Name: r.name}, root); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal([]ScopedCaller{{Name: "alias", KeyFrom: &SecretKeyRef{Secret: r.name, Key: apiKeySecretField}, Scopes: []string{"metrics:read"}}})
	if err != nil {
		t.Fatal(err)
	}
	root.Data["callers"] = raw
	if err := c.Update(ctx, root); err != nil {
		t.Fatal(err)
	}
	r.reader = &changingCredentialSourceClient{Client: c}
	if snapshot, err := r.Read(ctx); snapshot != nil || !errors.Is(err, errCredentialAdoptionSources) {
		t.Fatal("inconsistent repeated reference accepted", err)
	}
}
func TestCredentialAdoptionSourceHTTPStallIsBounded(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer server.Close()
	reader, err := newCredentialAdoptionSourceReader(&rest.Config{Host: server.URL, Timeout: time.Hour}, "kyber-system", "root-source")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	started := time.Now()
	snapshot, err := reader.Read(ctx)
	if snapshot != nil || !errors.Is(err, errCredentialAdoptionSources) {
		t.Fatal("stalled source yielded input", err)
	}
	if elapsed := time.Since(started); elapsed > 4*time.Second {
		t.Fatal("Secret read exceeded its bounded deadline", elapsed)
	}
}

//go:build integration

package operatorcredentials

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// This opt-in test writes only its own inert Secret. The registry is not wired
// to authentication, and no accepted credentials or Agent objects are changed.
func TestGCPDevSecretStoreResourceVersions(t *testing.T) {
	if os.Getenv("KYBER_OPERATOR_CREDENTIALS_DEV_VERIFY") != "1" {
		t.Skip("explicit GCP dev verification required")
	}
	const namespace = "kyber-system"
	const contextName = "gke_datawire-dev_us-central1-a_kyber-dev"
	expectedHost := os.Getenv("KYBER_OPERATOR_CREDENTIALS_DEV_HOST")
	if expectedHost == "" || os.Getenv("KUBECONFIG") == "" {
		t.Fatal("verified dev endpoint/configuration required")
	}
	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(&clientcmd.ClientConfigLoadingRules{ExplicitPath: os.Getenv("KUBECONFIG")}, &clientcmd.ConfigOverrides{CurrentContext: contextName}).ClientConfig()
	if err != nil || config.Host != expectedHost {
		t.Fatal("dev context/endpoint mismatch")
	}
	config.Timeout = 5 * time.Second
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, appsv1.AddToScheme, kyberv1.AddToScheme} {
		if add(scheme) != nil {
			t.Fatal("fixture scheme failed")
		}
	}
	direct, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal("dev client unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var deployment appsv1.Deployment
	if err = direct.Get(ctx, client.ObjectKey{Namespace: namespace, Name: "kyber-dev-control-plane"}, &deployment); err != nil || deployment.Labels["app.kubernetes.io/instance"] != "kyber-dev" {
		t.Fatal("dev installation mismatch")
	}
	var agents kyberv1.AgentList
	if err = direct.List(ctx, &agents, client.InNamespace(namespace)); err != nil || len(agents.Items) != 0 {
		t.Fatal("dev Agent preflight failed")
	}
	rootBefore := &metav1.PartialObjectMetadata{}
	rootBefore.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "Secret"})
	rootKey := client.ObjectKey{Namespace: namespace, Name: "kyber-api-credentials"}
	if err = direct.Get(ctx, rootKey, rootBefore); err != nil {
		t.Fatal("credential metadata preflight failed")
	}
	name := "operator-registry-probe-" + uuid.NewString()
	a, err := NewSecretStore(config, namespace, name)
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewSecretStore(config, namespace, name)
	if err != nil {
		t.Fatal(err)
	}
	doc := fixtureDocument()
	operationID := doc.Operations[0].ID
	var createdUID types.UID
	t.Cleanup(func() {
		cleanupCtx, done := context.WithTimeout(context.Background(), 15*time.Second)
		defer done()
		secret := &corev1.Secret{}
		if err := direct.Get(cleanupCtx, client.ObjectKey{Namespace: namespace, Name: name}, secret); apierrors.IsNotFound(err) {
			return
		} else if err != nil {
			t.Error("probe cleanup lookup failed")
			return
		}
		parsed, err := decode(secret.Data[dataKey])
		owned := false
		if err == nil {
			for _, op := range parsed.Operations {
				if op.ID == operationID {
					owned = true
				}
			}
		}
		if !owned || secret.UID == "" || secret.Labels[ownerLabel] != "v1" || createdUID != "" && secret.UID != createdUID {
			t.Error("probe cleanup ownership mismatch")
			return
		}
		uid := secret.UID
		if err = direct.Delete(cleanupCtx, secret, &client.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}}); err != nil && !apierrors.IsNotFound(err) {
			t.Error("probe deletion failed")
			return
		}
		if err = direct.Get(cleanupCtx, client.ObjectKey{Namespace: namespace, Name: name}, &corev1.Secret{}); !apierrors.IsNotFound(err) {
			t.Error("probe remains after deletion")
		}
	})
	created, err := a.Create(ctx, doc)
	if err != nil {
		t.Fatal("probe creation failed without changing live credentials")
	}
	secret := &corev1.Secret{}
	if err = direct.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, secret); err != nil {
		t.Fatal("probe identity lookup failed")
	}
	createdUID = secret.UID
	stale, err := b.Load(ctx)
	if err != nil {
		t.Fatal("independent reader failed")
	}
	next := created.Document.Clone()
	next.Credentials[1].Name = "Verified integration"
	next.Operations = append(next.Operations, fixtureOperation("update", fixtureServiceID))
	updated, err := a.Replace(ctx, created.Version, next)
	if err != nil || updated.Version == created.Version {
		t.Fatal("real resource-version update failed")
	}
	stale.Document.Credentials[1].Name = "Stale integration"
	stale.Document.Operations = append(stale.Document.Operations, fixtureOperation("update", fixtureServiceID))
	if _, err = b.Replace(ctx, stale.Version, stale.Document); !errors.Is(err, ErrConflict) {
		t.Fatal("stale writer was accepted")
	}
	fresh, err := b.Load(ctx)
	if err != nil || fresh.Document.Credentials[1].Name != "Verified integration" {
		t.Fatal("second authority reader was stale")
	}
	race := &racingWriter{Client: a.writer}
	contended, err := newSecretStore(a.reader, race, namespace, name)
	if err != nil {
		t.Fatal(err)
	}
	next = fresh.Document.Clone()
	next.Credentials[1].Name = "Conflicting integration"
	next.Operations = append(next.Operations, fixtureOperation("update", fixtureServiceID))
	if _, err = contended.Replace(ctx, fresh.Version, next); !errors.Is(err, ErrConflict) || race.updates != 1 {
		t.Fatal("server-side conflict was overwritten or retried")
	}
	confirmed, err := b.Load(ctx)
	if err != nil || confirmed.Document.Credentials[1].Name != "Verified integration" {
		t.Fatal("conflicting data persisted")
	}
	// Verify adoption selection using a second inert Secret, never the live
	// authentication registry or installation credentials.
	authorityName := name + "-authority"
	authority, err := NewAuthorityStore(config, namespace, name, authorityName)
	if err != nil {
		t.Fatal(err)
	}
	authorityKey := client.ObjectKey{Namespace: namespace, Name: authorityName}
	var markerUID types.UID
	originalRegistryUID := createdUID
	t.Cleanup(func() {
		cleanupCtx, done := context.WithTimeout(context.Background(), 15*time.Second)
		defer done()
		marker := &corev1.Secret{}
		if err := direct.Get(cleanupCtx, authorityKey, marker); apierrors.IsNotFound(err) {
			return
		} else if err != nil {
			t.Error("marker cleanup lookup failed")
			return
		}
		if marker.Labels[authorityLabel] != "v1" || string(marker.Data["registryUID"]) != string(originalRegistryUID) || marker.UID == "" || markerUID != "" && marker.UID != markerUID {
			t.Error("marker cleanup ownership mismatch")
			return
		}
		uid := marker.UID
		if err := direct.Delete(cleanupCtx, marker, &client.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}}); err != nil {
			t.Error("marker cleanup deletion failed")
			return
		}
		if err := direct.Get(cleanupCtx, authorityKey, &corev1.Secret{}); !apierrors.IsNotFound(err) {
			t.Error("marker remains after cleanup")
		}
	})
	if _, err := authority.Load(ctx); !errors.Is(err, ErrUninitialized) {
		t.Fatal("inert marker unexpectedly exists")
	}
	reservation, err := authority.ReserveAdoption(ctx)
	if err != nil {
		t.Fatal("probe maintenance reservation failed")
	}
	t.Cleanup(func() {
		if err := reservation.Release(context.Background()); err != nil {
			t.Error("probe reservation cleanup failed")
		}
	})
	if err := authority.Bind(ctx, confirmed.Version, reservation); err != nil {
		t.Fatal("inert authority binding failed")
	}
	marker := &corev1.Secret{}
	if err := direct.Get(ctx, authorityKey, marker); err != nil {
		t.Fatal("marker identity lookup failed")
	}
	markerUID = marker.UID
	if _, err := authority.Load(ctx); err != nil {
		t.Fatal("bound authority could not load")
	}
	replica, err := NewAuthorityStore(config, namespace, name, authorityName)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := replica.Load(ctx); err != nil {
		t.Fatal("independent replica lost adoption selection")
	}
	marker.Data["registryUID"] = []byte("replacement")
	if err := direct.Update(ctx, marker); !apierrors.IsInvalid(err) {
		t.Fatal("real API did not enforce immutable marker data")
	}
	// Delete and recreate only our disposable candidate. The bound UID must
	// reject a replacement even when its name and valid document are identical.
	if err := direct.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name}}, &client.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &createdUID}}); err != nil {
		t.Fatal("inert candidate deletion failed")
	}
	if _, err := replica.Load(ctx); !errors.Is(err, ErrUnavailable) {
		t.Fatal("missing adopted candidate allowed fallback")
	}
	if _, err := a.Create(ctx, doc); err != nil {
		t.Fatal("inert candidate recreation failed")
	}
	if err := direct.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, secret); err != nil {
		t.Fatal("replacement identity lookup failed")
	}
	createdUID = secret.UID
	if createdUID == originalRegistryUID {
		t.Fatal("API reused registry UID")
	}
	freshReplica, err := NewAuthorityStore(config, namespace, name, authorityName)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := freshReplica.Load(ctx); !errors.Is(err, ErrUnavailable) {
		t.Fatal("new replica accepted replacement registry")
	}
	rootAfter := &metav1.PartialObjectMetadata{}
	rootAfter.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "Secret"})
	if err = direct.Get(ctx, rootKey, rootAfter); err != nil || rootBefore.UID != rootAfter.UID || rootBefore.ResourceVersion != rootAfter.ResourceVersion {
		t.Fatal("live credential metadata changed during probe")
	}
	if err = direct.List(ctx, &agents, client.InNamespace(namespace)); err != nil || len(agents.Items) != 0 {
		t.Fatal("dev Agent postflight failed")
	}
	t.Log("Independent readers, stale-version and raced-write rejection passed; live credential metadata unchanged; zero Agents. Immutable adoption marker, replica selection and missing/replaced registry rejection passed. Both probe deletions are verified by cleanup.")
}

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
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type maintenanceDeleteRaceClient struct {
	client.Client
	beforeDelete func() error
}

func (c maintenanceDeleteRaceClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	if err := c.beforeDelete(); err != nil {
		return err
	}
	return c.Client.Delete(ctx, obj, opts...)
}

// Secret metadata uses the real API server; writer lifecycle objects are explicit
// fixtures. This proves CAS/delete transport, not real Pod shutdown or a complete
// runtime recovery, which remains an activation acceptance requirement.
type maintenanceRecoveryTransportReader struct {
	real      client.Reader
	lifecycle client.Reader
}

func (r maintenanceRecoveryTransportReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, ok := obj.(*corev1.Secret); ok {
		return r.real.Get(ctx, key, obj, opts...)
	}
	return r.lifecycle.Get(ctx, key, obj, opts...)
}
func (r maintenanceRecoveryTransportReader) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	return r.lifecycle.List(ctx, list, opts...)
}

type maintenanceRecoveryTransportWriter struct {
	client.Client
	lifecycle client.Client
}

func (w maintenanceRecoveryTransportWriter) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	if _, ok := obj.(*batchv1.Job); ok {
		return w.lifecycle.Update(ctx, obj, opts...)
	}
	return w.Client.Update(ctx, obj, opts...)
}

// This probe never uses the installation's real reservation name. It exercises
// server-side immutable Secrets and UID/RV delete preconditions, which the fake
// client does not implement. No accepted credential or Agent is modified.
func TestGCPDevMaintenanceReservation(t *testing.T) {
	if os.Getenv("KYBER_OPERATOR_CREDENTIALS_DEV_VERIFY") != "1" {
		t.Skip("explicit GCP dev verification required")
	}
	const ns = "kyber-system"
	expected := os.Getenv("KYBER_OPERATOR_CREDENTIALS_DEV_HOST")
	if expected == "" || os.Getenv("KUBECONFIG") == "" {
		t.Fatal("verified dev endpoint/configuration required")
	}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(&clientcmd.ClientConfigLoadingRules{ExplicitPath: os.Getenv("KUBECONFIG")}, &clientcmd.ConfigOverrides{CurrentContext: "gke_datawire-dev_us-central1-a_kyber-dev"}).ClientConfig()
	if err != nil || cfg.Host != expected {
		t.Fatal("dev context/endpoint mismatch")
	}
	cfg.Timeout = 5 * time.Second
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, appsv1.AddToScheme, kyberv1.AddToScheme} {
		if add(scheme) != nil {
			t.Fatal("fixture scheme failed")
		}
	}
	direct, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal("dev client unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	var deployment appsv1.Deployment
	if err := direct.Get(ctx, client.ObjectKey{Namespace: ns, Name: "kyber-dev-control-plane"}, &deployment); err != nil || deployment.Labels["app.kubernetes.io/instance"] != "kyber-dev" {
		t.Fatal("dev installation mismatch")
	}
	checkAgents := func() {
		t.Helper()
		var agents kyberv1.AgentList
		if err := direct.List(ctx, &agents); err != nil || len(agents.Items) != 0 {
			t.Fatal("zero-Agent verification failed")
		}
	}
	checkAgents()
	root := &metav1.PartialObjectMetadata{}
	root.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "Secret"})
	rootKey := client.ObjectKey{Namespace: ns, Name: "kyber-api-credentials"}
	if err := direct.Get(ctx, rootKey, root); err != nil {
		t.Fatal("credential metadata preflight failed")
	}
	name := "operator-maintenance-probe-" + uuid.NewString()
	transport, err := NewSecretStore(cfg, ns, DefaultSecretName)
	if err != nil {
		t.Fatal(err)
	}
	a, err := newMaintenanceStore(transport, name)
	if err != nil {
		t.Fatal(err)
	}
	bTransport, err := NewSecretStore(cfg, ns, DefaultSecretName)
	if err != nil {
		t.Fatal(err)
	}
	b, err := newMaintenanceStore(bTransport, name)
	if err != nil {
		t.Fatal(err)
	}
	ownedUIDs := map[string]bool{}
	t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.Background(), 15*time.Second)
		defer done()
		current := &corev1.Secret{}
		if err := direct.Get(cleanup, a.key, current); apierrors.IsNotFound(err) {
			return
		} else if err != nil {
			t.Error("probe cleanup read failed")
			return
		}
		if !ownedUIDs[string(current.UID)] || a.validate(current) != nil {
			t.Error("probe cleanup ownership mismatch")
			return
		}
		uid, rv := current.UID, current.ResourceVersion
		if err := direct.Delete(cleanup, current, &client.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &rv}}); err != nil {
			t.Error("probe cleanup delete failed")
			return
		}
		if err := direct.Get(cleanup, a.key, &corev1.Secret{}); !apierrors.IsNotFound(err) {
			t.Error("probe cleanup not confirmed")
		}
	})
	if status, err := a.Inspect(ctx); err != nil || status.Present {
		t.Fatal("new probe reservation was not idle")
	}
	first, err := a.Acquire(ctx, MaintenanceAdoption)
	if err != nil {
		t.Fatal(err)
	}
	ownedUIDs[string(first.uid)] = true
	if status, err := b.Inspect(ctx); err != nil || !status.Present || status.ExecutionID != first.owner || status.UID != string(first.uid) || status.Version != first.version || status.Kind != MaintenanceAdoption {
		t.Fatal("independent inspection lost exact reservation metadata")
	}
	if _, err := b.Acquire(ctx, MaintenanceUpgrade); !errors.Is(err, ErrConflict) {
		t.Fatal("independent executor stole reservation", err)
	}
	secret := &corev1.Secret{}
	if err := direct.Get(ctx, a.key, secret); err != nil {
		t.Fatal("probe read failed")
	}
	secret.Data["kind"] = []byte(MaintenanceUpgrade)
	if err := direct.Update(ctx, secret); !apierrors.IsInvalid(err) {
		t.Fatal("immutable reservation update accepted")
	}
	var replacement *MaintenanceReservation
	// Replace the object AFTER Release's ownership read but BEFORE its DELETE.
	// Only API-server preconditions can prevent the stale deletion here.
	transport.writer = maintenanceDeleteRaceClient{Client: direct, beforeDelete: func() error {
		if err := direct.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}, &client.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &first.uid, ResourceVersion: &first.version}}); err != nil {
			return err
		}
		var err error
		replacement, err = b.Acquire(ctx, MaintenanceUpgrade)
		if err == nil {
			ownedUIDs[string(replacement.uid)] = true
		}
		return err
	}}
	if err := first.Release(ctx); !errors.Is(err, ErrConflict) {
		t.Fatal("stale delete did not conflict", err)
	}
	if err := replacement.Check(ctx, MaintenanceUpgrade); err != nil {
		t.Fatal("replacement was deleted", err)
	}
	if err := replacement.Release(ctx); err != nil {
		t.Fatal("replacement release failed", err)
	}
	if status, err := a.Inspect(ctx); err != nil || status.Present {
		t.Fatal("released probe reservation remained blocked")
	}
	// Separately prove the resource-version precondition with an unchanged UID.
	third, err := b.Acquire(ctx, MaintenanceAdoption)
	if err != nil {
		t.Fatal(err)
	}
	ownedUIDs[string(third.uid)] = true
	bTransport.writer = maintenanceDeleteRaceClient{Client: direct, beforeDelete: func() error {
		current := &corev1.Secret{}
		if err := direct.Get(ctx, b.key, current); err != nil {
			return err
		}
		current.Annotations = map[string]string{"kyber.io/probe-race": "changed"}
		return direct.Update(ctx, current)
	}}
	if err := third.Release(ctx); !errors.Is(err, ErrConflict) {
		t.Fatal("resource-version delete precondition not enforced", err)
	}
	retained := &corev1.Secret{}
	if err := direct.Get(ctx, b.key, retained); err != nil || retained.UID != third.uid {
		t.Fatal("metadata race deleted reservation")
	}

	// Clear the retained, owned third probe before the recovery transport probe.
	retainedUID, retainedRV := retained.UID, retained.ResourceVersion
	if err := direct.Delete(ctx, retained, &client.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &retainedUID, ResourceVersion: &retainedRV}}); err != nil {
		t.Fatal("retained probe cleanup failed")
	}
	_, lifecycle, writerPod, writerJob := writerFixture(t)
	recoveryTransport, err := NewSecretStore(cfg, ns, DefaultSecretName)
	if err != nil {
		t.Fatal("recovery transport unavailable")
	}
	recoveryTransport.reader = maintenanceRecoveryTransportReader{real: direct, lifecycle: lifecycle}
	recoveryTransport.writer = maintenanceRecoveryTransportWriter{Client: direct, lifecycle: lifecycle}
	recoveryStore, err := newMaintenanceStore(recoveryTransport, name)
	if err != nil {
		t.Fatal("recovery store unavailable")
	}
	original, err := recoveryStore.AcquireForPod(ctx, MaintenanceUpgrade, writerPod.Name, string(writerPod.UID))
	if err != nil {
		t.Fatal("bound transport probe acquisition failed", err)
	}
	ownedUIDs[string(original.uid)] = true
	stopWriterFixture(t, lifecycle, writerPod, writerJob)
	status, err := recoveryStore.Inspect(ctx)
	if err != nil {
		t.Fatal("recovery transport inspection failed")
	}
	operationID := uuid.NewString()
	capability, err := recoveryStore.PrepareRecovery(ctx, status.UID, status.Version, operationID, "")
	if err != nil {
		t.Fatal("recovery transport fence failed", err)
	}
	if err := original.Release(ctx); !errors.Is(err, ErrConflict) {
		t.Fatal("old owner survived recovery CAS", err)
	}
	proof := capability.Proof()
	resumed, err := recoveryStore.ResumeRecovery(ctx, operationID, proof)
	if err != nil {
		t.Fatal("exact recovery proof could not resume", err)
	}
	recoveryTransport.writer = maintenanceDeleteRaceClient{Client: direct, beforeDelete: func() error {
		current := &corev1.Secret{}
		if err := direct.Get(ctx, recoveryStore.key, current); err != nil {
			return err
		}
		current.Annotations["kyber.io/probe-race"] = "recovery-changed"
		return direct.Update(ctx, current)
	}}
	if err := resumed.Release(ctx); !errors.Is(err, ErrConflict) {
		t.Fatal("recovery delete ignored resource-version precondition", err)
	}
	if _, err := recoveryStore.ResumeRecovery(ctx, operationID, proof); !errors.Is(err, ErrConflict) {
		t.Fatal("stale journal proof refreshed itself", err)
	}
	recoveryTransport.writer = maintenanceRecoveryTransportWriter{Client: direct, lifecycle: lifecycle}
	status, err = recoveryStore.Inspect(ctx)
	if err != nil || !status.Present {
		t.Fatal("recovery race deleted reservation")
	}
	capability, err = recoveryStore.PrepareRecovery(ctx, status.UID, status.Version, operationID, "")
	if err != nil {
		t.Fatal("transport probe refence failed", err)
	}
	if err := capability.Release(ctx); err != nil {
		t.Fatal("exact recovery transport release failed", err)
	}
	if status, err := recoveryStore.Inspect(ctx); err != nil || status.Present {
		t.Fatal("recovery transport cleanup not confirmed")
	}

	after := &metav1.PartialObjectMetadata{}
	after.SetGroupVersionKind(root.GroupVersionKind())
	if err := direct.Get(ctx, rootKey, after); err != nil || after.UID != root.UID || after.ResourceVersion != root.ResourceVersion {
		t.Fatal("installation credential metadata changed")
	}
	checkAgents()
}

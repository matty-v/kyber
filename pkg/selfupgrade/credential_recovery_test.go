package selfupgrade

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/matty-v/kyber/pkg/operatorcredentials"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type maintenanceAuthorityFixture struct {
	uid    string
	err    error
	calls  int
	onLoad func(int) (operatorcredentials.Snapshot, error)
}

func (f *maintenanceAuthorityFixture) Load(context.Context) (operatorcredentials.Snapshot, error) {
	f.calls++
	if f.onLoad != nil {
		return f.onLoad(f.calls)
	}
	return operatorcredentials.Snapshot{UID: f.uid}, f.err
}

type maintenanceHelmFixture struct {
	manifest, status string
	err              error
	calls            int
}

func (f *maintenanceHelmFixture) Output(_ context.Context, _ string, args ...string) (string, error) {
	f.calls++
	if f.err != nil {
		return "", f.err
	}
	if len(args) > 0 && args[0] == "status" {
		return f.status, nil
	}
	return f.manifest, nil
}
func maintenanceVerifierFixture(t *testing.T) (*MaintenanceReleaseVerifier, operatorcredentials.MaintenanceStatus, client.Client, *appsv1.Deployment, *corev1.Pod, *appsv1.ReplicaSet) {
	t.Helper()
	deployment, err := credentialCompatibleDeployment(credentialCompatibleManifest, "kyber-system", "kyber-control-plane")
	if err != nil {
		t.Fatal(err)
	}
	replicas := int32(1)
	controller := true
	deployment.UID = types.UID(uuid.NewString())
	deployment.ResourceVersion = "1"
	deployment.Generation = 3
	deployment.Spec.Replicas = &replicas
	deployment.Spec.Selector = &metav1.LabelSelector{MatchLabels: map[string]string{"app": "control-plane"}}
	deployment.Status = appsv1.DeploymentStatus{ObservedGeneration: 3, Replicas: 1, UpdatedReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 1}
	rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Namespace: "kyber-system", Name: "control-plane-rs", UID: types.UID(uuid.NewString()), OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "Deployment", Name: deployment.Name, UID: deployment.UID, Controller: &controller}}}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "kyber-system", Name: "control-plane-pod", UID: types.UID(uuid.NewString()), Labels: map[string]string{"app": "control-plane"}, OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: rs.Name, UID: rs.UID, Controller: &controller}}}, Spec: deployment.Spec.Template.Spec, Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}}
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = appsv1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deployment, rs, pod).WithStatusSubresource(&appsv1.Deployment{}, &corev1.Pod{}).Build()
	uid := uuid.NewString()
	authority := &maintenanceAuthorityFixture{uid: uid}
	helm := &maintenanceHelmFixture{manifest: credentialCompatibleManifest, status: `{"name":"kyber","version":8,"info":{"status":"deployed"}}`}
	verifier := &MaintenanceReleaseVerifier{namespace: "kyber-system", authority: authority, reader: c, helm: helm}
	status := operatorcredentials.MaintenanceStatus{Present: true, Kind: operatorcredentials.MaintenanceUpgrade, Writer: &operatorcredentials.MaintenanceWriter{PodName: "original-pod", PodUID: uuid.NewString(), JobName: "original-job", JobUID: uuid.NewString(), Container: "upgrade"}, Upgrade: &operatorcredentials.UpgradeEffectsCheckpoint{Release: "kyber", InitialRevision: 7, TargetVersion: "1.9.0", ControlPlaneDeployment: deployment.Name, Authority: "registry", RegistryUID: uid}}
	return verifier, status, c, deployment, pod, rs
}
func TestMaintenanceReleaseVerifierRequiresHealthyConsistentState(t *testing.T) {
	for _, scenario := range []string{"healthy", "failed-release", "wrong-image", "unready-pod", "unready-rollout", "wrong-owner", "changed-authority", "unsupported-artifact", "private-command-error"} {
		t.Run(scenario, func(t *testing.T) {
			v, status, c, deployment, pod, rs := maintenanceVerifierFixture(t)
			ctx := context.Background()
			switch scenario {
			case "failed-release":
				v.helm.(*maintenanceHelmFixture).status = `{"name":"kyber","version":8,"info":{"status":"failed"}}`
			case "wrong-image":
				deployment.Spec.Template.Spec.Containers[0].Image = "unverified.invalid/other:1.9.0"
				if err := c.Update(ctx, deployment); err != nil {
					t.Fatal(err)
				}
			case "unready-pod":
				pod.Status.Conditions = nil
				if err := c.Status().Update(ctx, pod); err != nil {
					t.Fatal(err)
				}
			case "unready-rollout":
				deployment.Status.UpdatedReplicas = 0
				if err := c.Status().Update(ctx, deployment); err != nil {
					t.Fatal(err)
				}
			case "wrong-owner":
				rs.OwnerReferences[0].UID = types.UID(uuid.NewString())
				if err := c.Update(ctx, rs); err != nil {
					t.Fatal(err)
				}
			case "changed-authority":
				v.authority.(*maintenanceAuthorityFixture).onLoad = func(call int) (operatorcredentials.Snapshot, error) {
					if call > 1 {
						return operatorcredentials.Snapshot{UID: uuid.NewString()}, nil
					}
					return operatorcredentials.Snapshot{UID: status.Upgrade.RegistryUID}, nil
				}
			case "unsupported-artifact":
				v.helm.(*maintenanceHelmFixture).manifest = strings.Replace(credentialCompatibleManifest, `protocol: "1"`, `protocol: "0"`, 1)
			case "private-command-error":
				v.helm.(*maintenanceHelmFixture).err = errors.New("private-manifest-canary")
			}
			err := v.Verify(ctx, status)
			if scenario == "healthy" {
				if err != nil {
					t.Fatal("healthy selected authority rejected", err)
				}
			} else if !errors.Is(err, ErrMaintenanceReleaseUnverified) || strings.Contains(err.Error(), "private-manifest-canary") {
				t.Fatal("unsafe or private reconciliation result", err)
			}
		})
	}
}
func TestMaintenanceReleaseVerifierPreservesUnselectedAndPreflightCases(t *testing.T) {
	v, status, _, _, _, _ := maintenanceVerifierFixture(t)
	v.authority = &maintenanceAuthorityFixture{err: operatorcredentials.ErrUninitialized}
	status.Upgrade.Authority = "unselected"
	status.Upgrade.RegistryUID = ""
	v.helm.(*maintenanceHelmFixture).manifest = strings.Replace(credentialCompatibleManifest, `protocol: "1"`, `protocol: "0"`, 1)
	if err := v.Verify(context.Background(), status); err != nil {
		t.Fatal("pre-adoption healthy release rejected", err)
	}
	v, status, _, _, _, _ = maintenanceVerifierFixture(t)
	status.Upgrade = nil
	if err := v.Verify(context.Background(), status); err != nil {
		t.Fatal(err)
	}
	if v.helm.(*maintenanceHelmFixture).calls != 0 {
		t.Fatal("preflight-only case inferred Helm effects")
	}
	v.authority = &maintenanceAuthorityFixture{err: errors.New("private authority diagnostic")}
	if err := v.Verify(context.Background(), status); !errors.Is(err, ErrMaintenanceReleaseUnverified) {
		t.Fatal("unknown authority treated as safe", err)
	}
}
func TestMaintenanceReleaseOutputIsBounded(t *testing.T) {
	var output maintenanceOutput
	if _, err := output.Write(make([]byte, maxCompatibilityManifestBytes+1)); !errors.Is(err, ErrMaintenanceReleaseUnverified) || output.Len() != 0 {
		t.Fatal("unbounded or partial private output retained", err)
	}
}

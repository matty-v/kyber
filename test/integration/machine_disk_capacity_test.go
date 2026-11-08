package integration

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/matty-v/kyber/pkg/adapters"
	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	machinecontroller "github.com/matty-v/kyber/pkg/controllers/machine"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// Exercise status writes and Agent deletion against a real API server.
// Agent PVC reservations must affect local storage only.
func TestMachineDiskCapacityStorageBackend(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := kyberv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	env := &envtest.Environment{Scheme: scheme, BinaryAssetsDirectory: os.Getenv("KUBEBUILDER_ASSETS"),
		CRDDirectoryPaths: []string{filepath.Join("..", "..", "deploy", "helm", "kyber", "crds")}, ErrorIfCRDPathMissing: true}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("starting envtest: %v", err)
	}
	t.Cleanup(func() {
		if err := env.Stop(); err != nil {
			t.Errorf("stopping envtest: %v", err)
		}
	})
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, backend := range []struct{ name, driver, before, after string }{
		{"pd", "pd.csi.storage.gke.io", "0", "0"},
		{"ebs", "ebs.csi.aws.com", "0", "0"},
		{"local", "rancher.io/local-path", "90Gi", "190Gi"},
	} {
		t.Run(backend.name, func(t *testing.T) {
			ns := "capacity-" + backend.name
			if err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
				t.Fatal(err)
			}
			class := &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: backend.name}, Provisioner: backend.driver}
			if err := c.Create(ctx, class); err != nil {
				t.Fatal(err)
			}
			machine := &kyberv1.Machine{ObjectMeta: metav1.ObjectMeta{Name: "worker-" + backend.name, Namespace: ns},
				Spec: kyberv1.MachineSpec{Provider: kyberv1.MachineProviderMock, Capacity: kyberv1.MachineCapacity{
					CPU: resource.MustParse("8"), Memory: resource.MustParse("32Gi")}}}
			if err := c.Create(ctx, machine); err != nil {
				t.Fatal(err)
			}
			// Static/mock machine reconciliation reads the explicitly named Node.
			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-" + backend.name, Labels: map[string]string{machinecontroller.MachineLabelKey: "worker-" + backend.name, "kubernetes.io/hostname": "node-" + backend.name}}}
			if err := c.Create(ctx, node); err != nil {
				t.Fatal(err)
			}
			node.Status.Allocatable = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("8"), corev1.ResourceMemory: resource.MustParse("32Gi"), corev1.ResourceEphemeralStorage: resource.MustParse("200Gi")}
			node.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue, LastHeartbeatTime: metav1.Now()}}
			if err := c.Status().Update(ctx, node); err != nil {
				t.Fatal(err)
			}
			machine.Status.NodeName = node.Name
			if err := c.Status().Update(ctx, machine); err != nil {
				t.Fatal(err)
			}
			agent := &kyberv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "han", Namespace: ns}, Spec: kyberv1.AgentSpec{Machine: "worker-" + backend.name, Runtime: "codex", Secrets: kyberv1.AgentSecrets{AuthType: kyberv1.AgentAuthTypeAPIKey}, Resources: kyberv1.AgentResources{
				CPU: resource.MustParse("1"), Memory: resource.MustParse("4Gi"), Disk: resource.MustParse("100Gi")}}}
			if err := c.Create(ctx, agent); err != nil {
				t.Fatal(err)
			}
			r := &machinecontroller.MachineReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(100), ComputeAdapter: adapters.NewMockComputeAdapter(),
				AgentStorageClass: class.Name, PlatformReservation: kyberv1.MachineCapacity{CPU: resource.MustParse("1"), Memory: resource.MustParse("1Gi"), EphemeralStorage: resource.MustParse("10Gi")}}
			key := types.NamespacedName{Namespace: ns, Name: machine.Name}
			check := func(wantDisk, wantCPU string) {
				t.Helper()
				if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
					t.Fatal(err)
				}
				var got kyberv1.Machine
				if err := c.Get(ctx, key, &got); err != nil {
					t.Fatal(err)
				}
				if got.Status.AvailableCapacity == nil || got.Status.AvailableCapacity.EphemeralStorage.Cmp(resource.MustParse(wantDisk)) != 0 || got.Status.AvailableCapacity.CPU.Cmp(resource.MustParse(wantCPU)) != 0 {
					t.Fatalf("available = %+v; want disk %s, CPU %s", got.Status.AvailableCapacity, wantDisk, wantCPU)
				}
				if got.Status.ObservedCapacity.EphemeralStorage.Cmp(resource.MustParse("200Gi")) != 0 {
					t.Fatal("physical node disk observation changed")
				}
			}
			check(backend.before, "6")
			if err := c.Delete(ctx, agent); err != nil {
				t.Fatal(err)
			}
			check(backend.after, "7")
		})
	}
}

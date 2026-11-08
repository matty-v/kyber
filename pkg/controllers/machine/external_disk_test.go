package machine

import (
	"context"
	"testing"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestUpdateNodeInfoExternalDisks(t *testing.T) {
	ctx := context.Background()
	for _, driver := range []string{"pd.csi.storage.gke.io", "ebs.csi.aws.com", "rancher.io/local-path"} {
		t.Run(driver, func(t *testing.T) {
			machine := &kyberv1.Machine{ObjectMeta: metav1.ObjectMeta{Name: "coders", Namespace: "kyber-system"}, Spec: kyberv1.MachineSpec{Provider: kyberv1.MachineProviderGKE}}
			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node", Labels: map[string]string{MachineLabelKey: machine.Name}}, Status: corev1.NodeStatus{Allocatable: corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse("8"), corev1.ResourceMemory: resource.MustParse("32Gi"), corev1.ResourceEphemeralStorage: resource.MustParse("150Gi"),
			}}}
			class := &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "agents"}, Provisioner: driver}
			agent := &kyberv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "han", Namespace: machine.Namespace}, Spec: kyberv1.AgentSpec{Machine: machine.Name, Resources: kyberv1.AgentResources{
				CPU: resource.MustParse("1"), Memory: resource.MustParse("4Gi"), Disk: resource.MustParse("50Gi"),
			}}}
			c := fake.NewClientBuilder().WithScheme(buildTestScheme()).WithObjects(machine, node, class, agent).WithStatusSubresource(machine).Build()
			r := &MachineReconciler{Client: c, AgentStorageClass: class.Name}
			for _, deleted := range []bool{false, true} {
				if deleted {
					if err := c.Delete(ctx, agent); err != nil {
						t.Fatal(err)
					}
				}
				if err := r.updateNodeInfo(ctx, machine); err != nil {
					t.Fatal(err)
				}
				var got kyberv1.Machine
				if err := c.Get(ctx, client.ObjectKeyFromObject(machine), &got); err != nil {
					t.Fatal(err)
				}
				wantDisk, wantCPU := "0", "7"
				if deleted {
					wantCPU = "8"
				}
				if driver == "rancher.io/local-path" {
					wantDisk = "100Gi"
					if deleted {
						wantDisk = "150Gi"
					}
				}
				if got.Status.AvailableCapacity == nil || got.Status.AvailableCapacity.EphemeralStorage.Cmp(resource.MustParse(wantDisk)) != 0 || got.Status.AvailableCapacity.CPU.Cmp(resource.MustParse(wantCPU)) != 0 {
					t.Fatalf("deleted=%v: available=%+v, want disk %s CPU %s", deleted, got.Status.AvailableCapacity, wantDisk, wantCPU)
				}
			}
		})
	}
}

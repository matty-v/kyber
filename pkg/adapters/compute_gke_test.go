package adapters

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"google.golang.org/api/container/v1"
	"google.golang.org/api/googleapi"
)

type fakeGKENodePoolsClient struct {
	pool        *container.NodePool
	err         error
	mutationErr error
	names       []string
	creates     []*container.NodePool
	sizes       []int64
	deletes     int
}

func (f *fakeGKENodePoolsClient) Create(_ context.Context, _ string, pool *container.NodePool) (*container.Operation, error) {
	f.creates = append(f.creates, pool)
	return &container.Operation{}, f.mutationErr
}
func (f *fakeGKENodePoolsClient) SetSize(_ context.Context, _ string, size int64) (*container.Operation, error) {
	f.sizes = append(f.sizes, size)
	return &container.Operation{}, f.mutationErr
}
func (f *fakeGKENodePoolsClient) Delete(_ context.Context, _ string) (*container.Operation, error) {
	f.deletes++
	return &container.Operation{}, f.mutationErr
}

func (f *fakeGKENodePoolsClient) Get(_ context.Context, name string) (*container.NodePool, error) {
	f.names = append(f.names, name)
	return f.pool, f.err
}

func TestGKEObservationOnlyProvider(t *testing.T) {
	tests := []struct {
		name   string
		status string
		state  AvailabilityState
		reason AvailabilityReason
	}{
		{name: "running", status: "RUNNING", state: CapacityAvailable, reason: ReasonReady},
		{name: "provisioning", status: "PROVISIONING", state: CapacityPending, reason: ReasonProvisioning},
		{name: "reconciling", status: "RECONCILING", state: CapacityRecovering, reason: ReasonRepairing},
		{name: "degraded", status: "RUNNING_WITH_ERROR", state: CapacityRecovering, reason: ReasonProviderError},
		{name: "failed", status: "ERROR", state: CapacityFailed, reason: ReasonProviderError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &fakeGKENodePoolsClient{pool: &container.NodePool{Status: tt.status}}
			provider := &GKEAdapter{ProjectID: "project", Location: "us-central1-a", Cluster: "cluster", client: client}
			got, err := provider.Reconcile(context.Background(), MachineIdentity{Name: "agents"}, DesiredMachine{Availability: DesiredOnline}, "")
			if err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			if got.State != tt.state || got.Reason != tt.reason {
				t.Fatalf("observation = %+v", got)
			}
			if got.ProviderRef != "gke://project/us-central1-a/cluster/nodePools/agents" {
				t.Errorf("ProviderRef = %q", got.ProviderRef)
			}
			if got.NodeSelector[gkeNodePoolLabel] != "agents" {
				t.Errorf("NodeSelector = %#v", got.NodeSelector)
			}
			wantName := "projects/project/locations/us-central1-a/clusters/cluster/nodePools/agents"
			if len(client.names) != 1 || client.names[0] != wantName {
				t.Errorf("Get names = %#v, want %q", client.names, wantName)
			}
		})
	}
}

func TestGKEObservationOnlyWaitsForExternalPool(t *testing.T) {
	provider := &GKEAdapter{
		ProjectID: "project", Location: "us-central1-a", Cluster: "cluster",
		client: &fakeGKENodePoolsClient{err: &googleapi.Error{Code: 404, Message: "not found"}},
	}
	got, err := provider.Reconcile(context.Background(), MachineIdentity{Name: "agents"}, DesiredMachine{Availability: DesiredOnline}, "")
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got.State != CapacityPending || got.Reason != ReasonExternalWait {
		t.Fatalf("observation = %+v", got)
	}
}

func TestGKEObservationOnlyDeletionUnregistersWithoutCloudCall(t *testing.T) {
	client := &fakeGKENodePoolsClient{err: errors.New("must not be called")}
	provider := &GKEAdapter{ProjectID: "project", Location: "us-central1-a", Cluster: "cluster", client: client}
	got, err := provider.Reconcile(context.Background(), MachineIdentity{Name: "agents"}, DesiredMachine{Availability: DesiredDeleted}, "")
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got.State != CapacityAbsent || len(client.names) != 0 {
		t.Fatalf("observation = %+v, calls = %#v", got, client.names)
	}
}

func TestGKEProviderReferenceIsClusterScoped(t *testing.T) {
	provider := &GKEAdapter{ProjectID: "project", Location: "us-central1-a", Cluster: "cluster"}
	if _, err := provider.resolvePool(MachineIdentity{}, "gke://other/us-central1-a/cluster/nodePools/agents"); err == nil {
		t.Fatal("cross-project reference accepted")
	}
	if _, err := provider.resolvePool(MachineIdentity{}, "gke://project/us-central1-a/other/nodePools/agents"); err == nil {
		t.Fatal("cross-cluster reference accepted")
	}
}

func TestGKEManagedLifecycleRequiresOwnership(t *testing.T) {
	profile := GKEProfile{ID: "standard", MachineType: "e2-standard-4", DiskSizeGB: 200, DiskType: "pd-balanced", ImageType: "UBUNTU_CONTAINERD", AvailabilityClasses: []string{"reliable", "costOptimized"}}
	provider := func(client *fakeGKENodePoolsClient) *GKEAdapter {
		return &GKEAdapter{ProjectID: "project", Location: "us-central1-a", Cluster: "cluster", client: client, profiles: map[string]GKEProfile{"standard": profile}}
	}
	desired := DesiredMachine{Availability: DesiredOnline, Profile: "standard", Managed: true, AttachmentObserved: true}

	createClient := &fakeGKENodePoolsClient{err: &googleapi.Error{Code: 404}}
	created, err := provider(createClient).Reconcile(context.Background(), MachineIdentity{Name: "test-pool"}, desired, "")
	if err != nil || created.State != CapacityPending || len(createClient.creates) != 1 {
		t.Fatalf("create = %+v, err=%v, calls=%d", created, err, len(createClient.creates))
	}
	createdPool := createClient.creates[0]
	if createdPool.Config.Labels["kyber.io/managed-by"] != "kyber" || createdPool.Config.Labels[MachineLabelKey] != "test-pool" {
		t.Fatalf("created pool lacks ownership: %+v", createdPool)
	}

	unownedClient := &fakeGKENodePoolsClient{pool: &container.NodePool{Status: "RUNNING", Config: &container.NodeConfig{}}}
	unowned, err := provider(unownedClient).Reconcile(context.Background(), MachineIdentity{Name: "test-pool"}, desired, "")
	if err != nil || unowned.State != CapacityFailed || len(unownedClient.sizes) != 0 {
		t.Fatalf("unowned = %+v, err=%v, sizes=%v", unowned, err, unownedClient.sizes)
	}

	ownedPool := &container.NodePool{Status: "RUNNING", Config: &container.NodeConfig{Labels: map[string]string{"kyber.io/managed-by": "kyber", MachineLabelKey: "test-pool"}}}
	resizeClient := &fakeGKENodePoolsClient{pool: ownedPool}
	resizing, err := provider(resizeClient).Reconcile(context.Background(), MachineIdentity{Name: "test-pool"}, desired, "")
	if err != nil || resizing.State != CapacityRecovering || len(resizeClient.sizes) != 1 || resizeClient.sizes[0] != 1 {
		t.Fatalf("resize = %+v, err=%v, sizes=%v", resizing, err, resizeClient.sizes)
	}

	repairClient := &fakeGKENodePoolsClient{pool: &container.NodePool{
		Status: "RUNNING", InitialNodeCount: 1,
		Config: &container.NodeConfig{Labels: map[string]string{"kyber.io/managed-by": "kyber", MachineLabelKey: "test-pool"}},
	}}
	repairing, err := provider(repairClient).Reconcile(context.Background(), MachineIdentity{Name: "test-pool"}, desired, "")
	if err != nil || repairing.State != CapacityRecovering || len(repairClient.sizes) != 0 {
		t.Fatalf("repair = %+v, err=%v, sizes=%v; an already-size-one pool must repair without another resize", repairing, err, repairClient.sizes)
	}
	if repairing.Message == "" {
		t.Error("repair observation must explain that provider capacity is pending")
	}

	deleteClient := &fakeGKENodePoolsClient{pool: ownedPool}
	desired.Availability = DesiredDeleted
	deleting, err := provider(deleteClient).Reconcile(context.Background(), MachineIdentity{Name: "test-pool"}, desired, "")
	if err != nil || deleting.State != CapacityRecovering || deleteClient.deletes != 1 {
		t.Fatalf("delete = %+v, err=%v, calls=%d", deleting, err, deleteClient.deletes)
	}
}

func TestGKEObservationOnlyCapabilities(t *testing.T) {
	provider := &GKEAdapter{}
	got, err := provider.Capabilities(context.Background())
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	if got.CanProvision || !got.CanDiscoverExisting || got.SuspendMode != SuspendUnsupported || got.DeletionMode != UnregisterOnly {
		t.Fatalf("Capabilities = %+v", got)
	}
}

func TestGKEValidateRejectsUnsupportedAvailabilityClass(t *testing.T) {
	provider := &GKEAdapter{profiles: map[string]GKEProfile{"reliable-only": {
		ID: "reliable-only", CPU: "2", Memory: "8Gi", MachineType: "e2-standard-2",
		DiskSizeGB: 20, DiskType: "pd-balanced", ImageType: "UBUNTU_CONTAINERD",
		AvailabilityClasses: []string{"reliable"},
	}}}
	err := provider.Validate(context.Background(), DesiredMachine{
		Availability: DesiredOnline, Profile: "reliable-only", Managed: true, Interruptible: true,
	})
	if err == nil || !strings.Contains(err.Error(), "costOptimized") {
		t.Fatalf("Validate error = %v, want unsupported costOptimized class", err)
	}
}

func TestParseGKEProfiles(t *testing.T) {
	profiles, err := ParseGKEProfiles(`[{"id":"standard","displayName":"Standard","cpu":"4","memory":"16Gi","availabilityClasses":["reliable","costOptimized"],"machineType":"e2-standard-4","diskSizeGb":200,"diskType":"pd-balanced","imageType":"UBUNTU_CONTAINERD"}]`)
	if err != nil {
		t.Fatalf("ParseGKEProfiles: %v", err)
	}
	if profiles["standard"].MachineType != "e2-standard-4" {
		t.Fatalf("profiles = %+v", profiles)
	}
	if _, err := ParseGKEProfiles(`[{"id":"broken"}]`); err == nil {
		t.Fatal("incomplete profile accepted")
	}
}

func TestGKELiveObservation(t *testing.T) {
	project := os.Getenv("KYBER_TEST_GKE_PROJECT")
	location := os.Getenv("KYBER_TEST_GKE_LOCATION")
	cluster := os.Getenv("KYBER_TEST_GKE_CLUSTER")
	pool := os.Getenv("KYBER_TEST_GKE_NODE_POOL")
	if project == "" || location == "" || cluster == "" || pool == "" {
		t.Skip("set KYBER_TEST_GKE_PROJECT, KYBER_TEST_GKE_LOCATION, KYBER_TEST_GKE_CLUSTER, and KYBER_TEST_GKE_NODE_POOL")
	}
	provider, err := NewGKEAdapter(context.Background(), project, location, cluster)
	if err != nil {
		t.Fatalf("NewGKEAdapter: %v", err)
	}
	got, err := provider.Reconcile(
		context.Background(), MachineIdentity{Name: pool},
		DesiredMachine{Availability: DesiredOnline}, "",
	)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got.State != CapacityAvailable {
		t.Fatalf("live observation = %+v, want Available", got)
	}
	if got.NodeSelector[gkeNodePoolLabel] != pool {
		t.Fatalf("live selector = %#v", got.NodeSelector)
	}
}

func TestGKELiveManagedLifecycle(t *testing.T) {
	if os.Getenv("KYBER_TEST_GKE_MANAGED") != "true" {
		t.Skip("set KYBER_TEST_GKE_MANAGED=true to permit disposable node-pool mutation")
	}
	project := os.Getenv("KYBER_TEST_GKE_PROJECT")
	location := os.Getenv("KYBER_TEST_GKE_LOCATION")
	cluster := os.Getenv("KYBER_TEST_GKE_CLUSTER")
	poolName := os.Getenv("KYBER_TEST_GKE_MANAGED_POOL")
	if project == "" || location == "" || cluster == "" || poolName == "" {
		t.Fatal("managed live test requires project, location, cluster, and managed pool variables")
	}
	if !strings.HasPrefix(poolName, "kyber-test-") || poolName == "platform" || poolName == "agents" {
		t.Fatalf("refusing unsafe managed test pool name %q", poolName)
	}
	provider, err := NewGKEAdapter(context.Background(), project, location, cluster)
	if err != nil {
		t.Fatalf("NewGKEAdapter: %v", err)
	}
	provider.profiles = map[string]GKEProfile{"live-small": {
		ID: "live-small", CPU: "2", Memory: "2Gi", MachineType: "e2-small",
		DiskSizeGB: 20, DiskType: "pd-balanced", ImageType: "UBUNTU_CONTAINERD",
		AvailabilityClasses: []string{"reliable"},
	}}
	resourceName := provider.resourceName(poolName)
	if _, err := provider.client.Get(context.Background(), resourceName); err == nil || !isGKENotFound(err) {
		t.Fatalf("refusing to reuse existing pool %q: %v", poolName, err)
	}
	cleanup := func() {
		pool, getErr := provider.client.Get(context.Background(), resourceName)
		if isGKENotFound(getErr) {
			return
		}
		if getErr != nil {
			t.Errorf("cleanup Get: %v", getErr)
			return
		}
		if !provider.owned(pool, poolName) {
			t.Errorf("cleanup refused unowned pool %q", poolName)
			return
		}
		if _, deleteErr := provider.client.Delete(context.Background(), resourceName); deleteErr != nil && !isGKENotFound(deleteErr) && !isGKEConflict(deleteErr) {
			t.Errorf("cleanup Delete: %v", deleteErr)
		}
		if waitErr := waitForGKEPool(provider, poolName, func(_ *container.NodePool, err error) bool { return isGKENotFound(err) }); waitErr != nil {
			t.Errorf("cleanup wait: %v", waitErr)
		}
	}
	t.Cleanup(cleanup)

	desired := DesiredMachine{Availability: DesiredOnline, Profile: "live-small", Managed: true, AttachmentObserved: true, AttachedNodes: 0}
	created, err := provider.Reconcile(context.Background(), MachineIdentity{Name: poolName}, desired, "")
	if err != nil || created.State != CapacityPending {
		t.Fatalf("create reconcile = %+v, err=%v", created, err)
	}
	if err := waitForGKEPool(provider, poolName, func(pool *container.NodePool, err error) bool {
		return err == nil && pool.Status == "RUNNING" && pool.InitialNodeCount == 1
	}); err != nil {
		t.Fatalf("waiting for created pool: %v", err)
	}

	desired.Availability, desired.AttachedNodes = DesiredOffline, 1
	if got, err := provider.Reconcile(context.Background(), MachineIdentity{Name: poolName}, desired, created.ProviderRef); err != nil || got.State != CapacityRecovering {
		t.Fatalf("offline reconcile = %+v, err=%v", got, err)
	}
	if err := waitForGKEPool(provider, poolName, func(pool *container.NodePool, err error) bool {
		return err == nil && pool.Status == "RUNNING" && pool.InitialNodeCount == 0
	}); err != nil {
		t.Fatalf("waiting for zero nodes: %v", err)
	}

	desired.Availability, desired.AttachedNodes = DesiredOnline, 0
	if got, err := provider.Reconcile(context.Background(), MachineIdentity{Name: poolName}, desired, created.ProviderRef); err != nil || got.State != CapacityRecovering {
		t.Fatalf("online reconcile = %+v, err=%v", got, err)
	}
	if err := waitForGKEPool(provider, poolName, func(pool *container.NodePool, err error) bool {
		return err == nil && pool.Status == "RUNNING" && pool.InitialNodeCount == 1
	}); err != nil {
		t.Fatalf("waiting for one node: %v", err)
	}

	desired.Availability = DesiredDeleted
	if got, err := provider.Reconcile(context.Background(), MachineIdentity{Name: poolName}, desired, created.ProviderRef); err != nil || got.State != CapacityRecovering {
		t.Fatalf("delete reconcile = %+v, err=%v", got, err)
	}
	if err := waitForGKEPool(provider, poolName, func(_ *container.NodePool, err error) bool { return isGKENotFound(err) }); err != nil {
		t.Fatalf("waiting for deletion: %v", err)
	}
}

func waitForGKEPool(provider *GKEAdapter, poolName string, done func(*container.NodePool, error) bool) error {
	deadline := time.Now().Add(12 * time.Minute)
	for time.Now().Before(deadline) {
		pool, err := provider.client.Get(context.Background(), provider.resourceName(poolName))
		if done(pool, err) {
			return nil
		}
		if err != nil && !isGKENotFound(err) {
			return err
		}
		time.Sleep(10 * time.Second)
	}
	return fmt.Errorf("timed out waiting for GKE node pool %s", poolName)
}

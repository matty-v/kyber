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
	pools       map[string]*container.NodePool
	err         error
	mutationErr error
	names       []string
	creates     []*container.NodePool
	sizes       []int64
	sizeNames   []string
	autoscaling []*container.NodePoolAutoscaling
	deletes     int
}

func (f *fakeGKENodePoolsClient) Create(_ context.Context, _ string, pool *container.NodePool) (*container.Operation, error) {
	f.creates = append(f.creates, pool)
	if f.pools != nil && f.mutationErr == nil {
		pool.Status = "RUNNING"
		f.pools[pool.Name] = pool
	}
	return &container.Operation{}, f.mutationErr
}
func (f *fakeGKENodePoolsClient) SetSize(_ context.Context, name string, size int64) (*container.Operation, error) {
	f.sizes = append(f.sizes, size)
	f.sizeNames = append(f.sizeNames, name)
	if f.pools != nil && f.mutationErr == nil {
		if pool := f.pools[gkeResourceLeaf(name)]; pool != nil {
			pool.InitialNodeCount = size
		}
	}
	return &container.Operation{}, f.mutationErr
}
func (f *fakeGKENodePoolsClient) SetAutoscaling(_ context.Context, name string, autoscaling *container.NodePoolAutoscaling) (*container.Operation, error) {
	f.autoscaling = append(f.autoscaling, autoscaling)
	if f.pools != nil && f.mutationErr == nil {
		if pool := f.pools[gkeResourceLeaf(name)]; pool != nil {
			pool.Autoscaling = autoscaling
		}
	}
	return &container.Operation{}, f.mutationErr
}
func (f *fakeGKENodePoolsClient) Delete(_ context.Context, name string) (*container.Operation, error) {
	f.deletes++
	if f.pools != nil && f.mutationErr == nil {
		delete(f.pools, gkeResourceLeaf(name))
	}
	return &container.Operation{}, f.mutationErr
}

func (f *fakeGKENodePoolsClient) Get(_ context.Context, name string) (*container.NodePool, error) {
	f.names = append(f.names, name)
	if f.pools != nil {
		if pool := f.pools[gkeResourceLeaf(name)]; pool != nil {
			return pool, nil
		}
		return nil, &googleapi.Error{Code: 404}
	}
	return f.pool, f.err
}

func gkeResourceLeaf(name string) string {
	parts := strings.Split(name, "/")
	return parts[len(parts)-1]
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

	if resizing.Message == "" {
		t.Error("resize observation must explain that provider capacity is pending")
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
	if got.RequiresSchedulerDemand {
		t.Fatal("zonal GKE provider must not advertise scheduler demand")
	}

	provider.nodeLocations = []string{"us-central1-a", "us-central1-b", "us-central1-c"}
	got, err = provider.Capabilities(context.Background())
	if err != nil {
		t.Fatalf("regional Capabilities: %v", err)
	}
	if !got.RequiresSchedulerDemand {
		t.Fatal("regional GKE provider must advertise scheduler demand")
	}
}

func TestGKEReliableFallbackCapabilityRequiresExplicitOptIn(t *testing.T) {
	provider := &GKEAdapter{profiles: map[string]GKEProfile{"standard": {ID: "standard"}}}
	got, _ := provider.Capabilities(context.Background())
	if got.ReliableFallbackMode != ReliableFallbackUnsupported {
		t.Fatalf("default fallback mode = %q", got.ReliableFallbackMode)
	}
	provider.reliableFallback = true
	got, _ = provider.Capabilities(context.Background())
	if got.ReliableFallbackMode != ReliableFallbackAutomatic {
		t.Fatalf("enabled fallback mode = %q", got.ReliableFallbackMode)
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

func TestGKEZonalCostOptimizedFallbackAndManualRetry(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	client := &fakeGKENodePoolsClient{pools: map[string]*container.NodePool{}}
	provider := &GKEAdapter{
		ProjectID: "project", Location: "us-central1-a", Cluster: "cluster", client: client,
		reliableFallback: true, now: func() time.Time { return now }, fallbackThreshold: 5 * time.Minute,
		profiles: map[string]GKEProfile{"standard": {
			ID: "standard", CPU: "2", Memory: "8Gi", MachineType: "e2-standard-2",
			DiskSizeGB: 20, DiskType: "pd-balanced", ImageType: "UBUNTU_CONTAINERD",
			AvailabilityClasses: []string{"reliable", "costOptimized"},
		}},
	}
	id := MachineIdentity{Name: "agents"}
	d := DesiredMachine{Availability: DesiredOnline, Profile: "standard", Managed: true, Interruptible: true, AttachmentObserved: true, AttachedNodes: 1}
	created, err := provider.Reconcile(ctx, id, d, "")
	if err != nil || len(client.creates) != 1 || !client.creates[0].Config.Spot {
		t.Fatalf("Spot create = %+v, err=%v, creates=%d", created, err, len(client.creates))
	}
	if _, err := provider.Reconcile(ctx, id, d, created.ProviderRef); err != nil || len(client.creates) != 2 || client.creates[1].Config.Spot || client.creates[1].InitialNodeCount != 0 {
		t.Fatalf("reliable precreate: err=%v creates=%+v", err, client.creates)
	}
	ready, _ := provider.Reconcile(ctx, id, d, created.ProviderRef)
	if ready.State != CapacityAvailable || ready.EffectiveAvailabilityClass != "costOptimized" {
		t.Fatalf("initial ready = %+v", ready)
	}

	// After five minutes without a node, Spot is removed first. Only after the
	// attachment count reaches zero is standard capacity requested.
	d.AttachedNodes = 0
	d.CostOptimizedUnavailableSince = now.Add(-5 * time.Minute)
	fallbackStarted, _ := provider.Reconcile(ctx, id, d, created.ProviderRef)
	if client.sizes[len(client.sizes)-1] != 0 || !strings.HasSuffix(client.sizeNames[len(client.sizeNames)-1], "agents-spot") || fallbackStarted.FallbackSince.IsZero() {
		t.Fatalf("fallback start = %+v sizes=%v names=%v", fallbackStarted, client.sizes, client.sizeNames)
	}
	d.FallbackSince = fallbackStarted.FallbackSince
	d.EffectiveAvailabilityClass = fallbackStarted.EffectiveAvailabilityClass
	fallbackScaling, _ := provider.Reconcile(ctx, id, d, created.ProviderRef)
	if client.sizes[len(client.sizes)-1] != 1 || !strings.HasSuffix(client.sizeNames[len(client.sizeNames)-1], "agents-reliable") || fallbackScaling.EffectiveAvailabilityClass != "reliable" {
		t.Fatalf("fallback scale = %+v sizes=%v names=%v", fallbackScaling, client.sizes, client.sizeNames)
	}
	d.EffectiveAvailabilityClass = "reliable"
	d.AttachedNodes = 1
	fallbackReady, _ := provider.Reconcile(ctx, id, d, created.ProviderRef)
	if fallbackReady.State != CapacityAvailable || fallbackReady.ProviderRef != created.ProviderRef {
		t.Fatalf("fallback ready = %+v", fallbackReady)
	}

	// The provider-neutral retry uses the same stable reference and reverses
	// ordering: standard zero, attachment zero, then Spot one.
	d.CostOptimizedRetryRequest = "retry-1"
	retryStarted, _ := provider.Reconcile(ctx, id, d, created.ProviderRef)
	d.CostOptimizedRetrySince = retryStarted.CostOptimizedRetrySince
	provider.Reconcile(ctx, id, d, created.ProviderRef)
	if client.sizes[len(client.sizes)-1] != 0 || !strings.HasSuffix(client.sizeNames[len(client.sizeNames)-1], "agents-reliable") {
		t.Fatal("manual retry did not remove reliable first")
	}
	d.AttachedNodes = 0
	toSpot, _ := provider.Reconcile(ctx, id, d, created.ProviderRef)
	if client.sizes[len(client.sizes)-1] != 1 || !strings.HasSuffix(client.sizeNames[len(client.sizeNames)-1], "agents-spot") || toSpot.EffectiveAvailabilityClass != "costOptimized" {
		t.Fatalf("manual retry Spot scale = %+v", toSpot)
	}
	d.EffectiveAvailabilityClass = "costOptimized"
	d.AttachedNodes = 1
	retried, _ := provider.Reconcile(ctx, id, d, created.ProviderRef)
	if retried.State != CapacityAvailable || retried.CostOptimizedRetryObserved != "retry-1" || retried.ProviderRef != created.ProviderRef || !retried.FallbackSince.IsZero() {
		t.Fatalf("manual retry ready = %+v", retried)
	}
}

func TestGKECostOptimizedRetryTimeoutRestoresReliable(t *testing.T) {
	now := time.Date(2026, 8, 26, 12, 10, 0, 0, time.UTC)
	profile := GKEProfile{ID: "standard", CPU: "2", Memory: "8Gi", MachineType: "e2-standard-2", DiskSizeGB: 20, DiskType: "pd-balanced", ImageType: "UBUNTU_CONTAINERD", AvailabilityClasses: []string{"reliable", "costOptimized"}}
	client := &fakeGKENodePoolsClient{pools: map[string]*container.NodePool{}}
	provider := &GKEAdapter{ProjectID: "project", Location: "us-central1-a", Cluster: "cluster", client: client, reliableFallback: true, now: func() time.Time { return now }, fallbackThreshold: 5 * time.Minute, profiles: map[string]GKEProfile{"standard": profile}}
	client.pools["agents-spot"] = provider.managedPairPool("agents-spot", "agents", profile, true, true)
	client.pools["agents-reliable"] = provider.managedPairPool("agents-reliable", "agents", profile, false, false)
	d := DesiredMachine{Availability: DesiredOnline, Profile: "standard", Managed: true, Interruptible: true, AttachmentObserved: true, AttachedNodes: 1, EffectiveAvailabilityClass: "costOptimized", FallbackSince: now.Add(-time.Hour), CostOptimizedUnavailableSince: now.Add(-time.Hour), CostOptimizedRetryRequest: "retry-1", CostOptimizedRetrySince: now.Add(-5 * time.Minute)}
	ref := provider.providerRef("agents")
	removed, _ := provider.Reconcile(context.Background(), MachineIdentity{Name: "agents"}, d, ref)
	if client.sizes[len(client.sizes)-1] != 0 || removed.EffectiveAvailabilityClass != "costOptimized" || removed.CostOptimizedRetryObserved != "" {
		t.Fatalf("rollback remove Spot = %+v sizes=%v", removed, client.sizes)
	}
	// The shared Machine selector still observes the draining Spot Node. It
	// must not be interpreted as a Ready reliable Node or acknowledge rollback.
	d.EffectiveAvailabilityClass = removed.EffectiveAvailabilityClass
	stillDraining, _ := provider.Reconcile(context.Background(), MachineIdentity{Name: "agents"}, d, ref)
	if stillDraining.State == CapacityAvailable || stillDraining.CostOptimizedRetryObserved != "" || client.sizes[len(client.sizes)-1] != 0 {
		t.Fatalf("rollback draining Spot = %+v sizes=%v", stillDraining, client.sizes)
	}
	d.AttachedNodes = 0
	d.EffectiveAvailabilityClass = stillDraining.EffectiveAvailabilityClass
	restored, _ := provider.Reconcile(context.Background(), MachineIdentity{Name: "agents"}, d, ref)
	if client.sizes[len(client.sizes)-1] != 1 || restored.EffectiveAvailabilityClass != "reliable" || restored.CostOptimizedRetryObserved != "" {
		t.Fatalf("rollback restore reliable = %+v sizes=%v", restored, client.sizes)
	}
	d.EffectiveAvailabilityClass = "reliable"
	d.AttachedNodes = 1
	ready, _ := provider.Reconcile(context.Background(), MachineIdentity{Name: "agents"}, d, ref)
	if ready.State != CapacityAvailable || ready.CostOptimizedRetryObserved != "retry-1" || !ready.CostOptimizedRetrySince.IsZero() {
		t.Fatalf("rollback ready = %+v", ready)
	}
}

func TestGKELateSpotNodeCannotCompleteFallbackAsReliable(t *testing.T) {
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	profile := GKEProfile{ID: "standard", CPU: "2", Memory: "8Gi", MachineType: "e2-standard-2", DiskSizeGB: 20, DiskType: "pd-balanced", ImageType: "UBUNTU_CONTAINERD", AvailabilityClasses: []string{"reliable", "costOptimized"}}
	client := &fakeGKENodePoolsClient{pools: map[string]*container.NodePool{}}
	provider := &GKEAdapter{ProjectID: "project", Location: "us-central1-a", Cluster: "cluster", client: client, reliableFallback: true, now: func() time.Time { return now }, profiles: map[string]GKEProfile{"standard": profile}}
	client.pools["agents-spot"] = provider.managedPairPool("agents-spot", "agents", profile, true, false)
	client.pools["agents-reliable"] = provider.managedPairPool("agents-reliable", "agents", profile, false, false)
	d := DesiredMachine{Availability: DesiredOnline, Profile: "standard", Managed: true, Interruptible: true, AttachmentObserved: true, AttachedNodes: 1, EffectiveAvailabilityClass: "costOptimized", FallbackSince: now, CostOptimizedUnavailableSince: now.Add(-5 * time.Minute)}
	waiting, err := provider.Reconcile(context.Background(), MachineIdentity{Name: "agents"}, d, provider.providerRef("agents"))
	if err != nil || waiting.State != CapacityRecovering || waiting.EffectiveAvailabilityClass != "costOptimized" || waiting.FallbackSince != now {
		t.Fatalf("late-node observation = %+v err=%v", waiting, err)
	}
	if client.pools["agents-reliable"].InitialNodeCount != 0 {
		t.Fatal("reliable pool started before the late Spot Node detached")
	}
	d.AttachedNodes = 0
	started, _ := provider.Reconcile(context.Background(), MachineIdentity{Name: "agents"}, d, provider.providerRef("agents"))
	if started.EffectiveAvailabilityClass != "reliable" || client.pools["agents-reliable"].InitialNodeCount != 1 {
		t.Fatalf("reliable start = %+v", started)
	}
}

func TestGKEFallbackOptInPreservesLegacySinglePool(t *testing.T) {
	profile := GKEProfile{ID: "standard", CPU: "2", Memory: "8Gi", MachineType: "e2-standard-2", DiskSizeGB: 20, DiskType: "pd-balanced", ImageType: "UBUNTU_CONTAINERD", AvailabilityClasses: []string{"costOptimized"}}
	legacy := &container.NodePool{Status: "RUNNING", Config: &container.NodeConfig{Spot: true, Labels: map[string]string{"kyber.io/managed-by": "kyber", MachineLabelKey: "agents"}}}
	client := &fakeGKENodePoolsClient{pools: map[string]*container.NodePool{"agents": legacy}}
	provider := &GKEAdapter{ProjectID: "project", Location: "us-central1-a", Cluster: "cluster", client: client, reliableFallback: true, profiles: map[string]GKEProfile{"standard": profile}}
	got, err := provider.Reconcile(context.Background(), MachineIdentity{Name: "agents"}, DesiredMachine{Availability: DesiredOnline, Profile: "standard", Managed: true, Interruptible: true, AttachmentObserved: true, AttachedNodes: 1}, provider.providerRef("agents"))
	if err != nil || got.State != CapacityAvailable || len(client.creates) != 0 {
		t.Fatalf("legacy reconcile = %+v err=%v creates=%d", got, err, len(client.creates))
	}
}

func TestGKEManagedDeletionRequiresAuthoritativeNodeAbsence(t *testing.T) {
	profile := GKEProfile{ID: "standard", CPU: "2", Memory: "8Gi", MachineType: "e2-standard-2", DiskSizeGB: 20, DiskType: "pd-balanced", ImageType: "UBUNTU_CONTAINERD", AvailabilityClasses: []string{"reliable"}}
	pool := &container.NodePool{Status: "RUNNING", InitialNodeCount: 0, Config: &container.NodeConfig{Labels: map[string]string{"kyber.io/managed-by": "kyber", MachineLabelKey: "agents"}}}
	client := &fakeGKENodePoolsClient{pool: pool}
	provider := &GKEAdapter{ProjectID: "project", Location: "us-central1-a", Cluster: "cluster", client: client, profiles: map[string]GKEProfile{"standard": profile}}
	got, err := provider.Reconcile(context.Background(), MachineIdentity{Name: "agents"}, DesiredMachine{Availability: DesiredDeleted, Profile: "standard", Managed: true}, provider.providerRef("agents"))
	if err != nil || got.State != CapacityRecovering || client.deletes != 0 {
		t.Fatalf("unobserved deletion = %+v err=%v deletes=%d", got, err, client.deletes)
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

func TestGKERegionalManagedPoolUsesOneTotalNodeAcrossZones(t *testing.T) {
	client := &fakeGKENodePoolsClient{err: &googleapi.Error{Code: 404}}
	provider := &GKEAdapter{
		ProjectID: "project", Location: "us-central1", Cluster: "cluster", client: client,
		nodeLocations: []string{"us-central1-a", "us-central1-c"},
		profiles: map[string]GKEProfile{"standard": {
			ID: "standard", CPU: "4", Memory: "16Gi", MachineType: "e2-standard-4",
			DiskSizeGB: 200, DiskType: "pd-balanced", ImageType: "UBUNTU_CONTAINERD",
			AvailabilityClasses: []string{"reliable", "costOptimized"},
		}},
	}
	if !provider.NeedsSchedulerDemand() {
		t.Fatal("regional GKE provider must request scheduler demand")
	}
	got, err := provider.Reconcile(context.Background(), MachineIdentity{Name: "agents"}, DesiredMachine{
		Availability: DesiredOnline, Profile: "standard", Managed: true, Interruptible: true,
	}, "")
	if err != nil || got.State != CapacityPending || len(client.creates) != 1 {
		t.Fatalf("Reconcile = %+v, err=%v, creates=%d", got, err, len(client.creates))
	}
	pool := client.creates[0]
	if pool.InitialNodeCount != 0 {
		t.Errorf("InitialNodeCount = %d, want 0", pool.InitialNodeCount)
	}
	if !regionalAutoscalingOnline(pool.Autoscaling) {
		t.Errorf("Autoscaling = %+v, want total size one with ANY placement", pool.Autoscaling)
	}
	if fmt.Sprint(pool.Locations) != "[us-central1-a us-central1-c]" {
		t.Errorf("Locations = %v", pool.Locations)
	}
}

func TestGKERegionalRecoveryDoesNotRepeatSetSize(t *testing.T) {
	client := &fakeGKENodePoolsClient{pool: &container.NodePool{
		Status: "RUNNING", Autoscaling: (&GKEAdapter{}).onlineAutoscaling(),
		Config: &container.NodeConfig{Labels: map[string]string{"kyber.io/managed-by": "kyber", MachineLabelKey: "agents"}},
	}}
	provider := &GKEAdapter{
		ProjectID: "project", Location: "us-central1", Cluster: "cluster", client: client,
		nodeLocations: []string{"us-central1-a", "us-central1-c"},
		profiles: map[string]GKEProfile{"standard": {
			ID: "standard", CPU: "4", Memory: "16Gi", MachineType: "e2-standard-4", DiskSizeGB: 200,
			DiskType: "pd-balanced", ImageType: "UBUNTU_CONTAINERD", AvailabilityClasses: []string{"costOptimized"},
		}},
	}
	got, err := provider.Reconcile(context.Background(), MachineIdentity{Name: "agents"}, DesiredMachine{
		Availability: DesiredOnline, Profile: "standard", Managed: true, Interruptible: true,
		AttachmentObserved: true, AttachedNodes: 0,
	}, "")
	if err != nil || got.State != CapacityRecovering {
		t.Fatalf("Reconcile = %+v, err=%v", got, err)
	}
	if len(client.sizes) != 0 || len(client.autoscaling) != 0 {
		t.Fatalf("unexpected mutations: sizes=%v autoscaling=%v", client.sizes, client.autoscaling)
	}
}

func TestGKENarrowedPoolDisablesAutoscalingBeforeResize(t *testing.T) {
	ownedPool := &container.NodePool{
		Status: "RUNNING", Autoscaling: (&GKEAdapter{}).onlineAutoscaling(),
		Config: &container.NodeConfig{Labels: map[string]string{"kyber.io/managed-by": "kyber", MachineLabelKey: "agents"}},
	}
	for _, availability := range []DesiredAvailability{DesiredOnline, DesiredOffline} {
		t.Run(string(availability), func(t *testing.T) {
			client := &fakeGKENodePoolsClient{pool: ownedPool}
			provider := &GKEAdapter{
				ProjectID: "project", Location: "us-central1-a", Cluster: "cluster", client: client,
				profiles: map[string]GKEProfile{"standard": {ID: "standard", AvailabilityClasses: []string{"reliable"}}},
			}
			got, err := provider.Reconcile(context.Background(), MachineIdentity{Name: "agents"}, DesiredMachine{
				Availability: availability, Profile: "standard", Managed: true,
				AttachmentObserved: true, AttachedNodes: 1,
			}, "")
			if err != nil || got.State != CapacityRecovering {
				t.Fatalf("Reconcile = %+v, err=%v", got, err)
			}
			if len(client.autoscaling) != 1 || client.autoscaling[0].Enabled {
				t.Fatalf("autoscaling mutations = %+v, want one disable", client.autoscaling)
			}
			if len(client.sizes) != 0 {
				t.Fatalf("SetSize called before autoscaling disabled: %v", client.sizes)
			}
		})
	}
}

func TestValidateGKENodeLocations(t *testing.T) {
	if err := validateGKENodeLocations("us-central1", []string{"us-central1-a", "us-central1-c"}); err != nil {
		t.Fatalf("valid regional locations rejected: %v", err)
	}
	for _, locations := range [][]string{{"us-east1-b"}, {"us-central1"}, {"us-central1", "us-central1-a"}, {"us-central1-a", "us-central1-a"}, {""}} {
		if err := validateGKENodeLocations("us-central1", locations); err == nil {
			t.Errorf("invalid locations accepted: %v", locations)
		}
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

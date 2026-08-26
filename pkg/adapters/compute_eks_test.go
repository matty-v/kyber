package adapters

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	ekstypes "github.com/aws/aws-sdk-go-v2/service/eks/types"
)

type fakeEKSClient struct {
	nodegroup *ekstypes.Nodegroup
	groups    map[string]*ekstypes.Nodegroup
	create    *eks.CreateNodegroupInput
	creates   []*eks.CreateNodegroupInput
	update    *eks.UpdateNodegroupConfigInput
	deleted   bool
}

func (f *fakeEKSClient) DescribeNodegroup(_ context.Context, in *eks.DescribeNodegroupInput, _ ...func(*eks.Options)) (*eks.DescribeNodegroupOutput, error) {
	if f.groups != nil {
		if ng := f.groups[aws.ToString(in.NodegroupName)]; ng != nil {
			return &eks.DescribeNodegroupOutput{Nodegroup: ng}, nil
		}
		return nil, &ekstypes.ResourceNotFoundException{Message: aws.String("missing")}
	}
	if f.nodegroup == nil {
		return nil, &ekstypes.ResourceNotFoundException{Message: aws.String("missing")}
	}
	return &eks.DescribeNodegroupOutput{Nodegroup: f.nodegroup}, nil
}
func (f *fakeEKSClient) CreateNodegroup(_ context.Context, in *eks.CreateNodegroupInput, _ ...func(*eks.Options)) (*eks.CreateNodegroupOutput, error) {
	f.create = in
	f.creates = append(f.creates, in)
	return &eks.CreateNodegroupOutput{}, nil
}
func (f *fakeEKSClient) UpdateNodegroupConfig(_ context.Context, in *eks.UpdateNodegroupConfigInput, _ ...func(*eks.Options)) (*eks.UpdateNodegroupConfigOutput, error) {
	f.update = in
	if f.groups != nil {
		if ng := f.groups[aws.ToString(in.NodegroupName)]; ng != nil {
			ng.ScalingConfig = in.ScalingConfig
		}
	}
	return &eks.UpdateNodegroupConfigOutput{}, nil
}
func (f *fakeEKSClient) DeleteNodegroup(_ context.Context, in *eks.DeleteNodegroupInput, _ ...func(*eks.Options)) (*eks.DeleteNodegroupOutput, error) {
	f.deleted = true
	if f.groups != nil {
		delete(f.groups, aws.ToString(in.NodegroupName))
	}
	return &eks.DeleteNodegroupOutput{}, nil
}

func pairGroup(machine, subnet string, capacity ekstypes.CapacityTypes, size int32) *ekstypes.Nodegroup {
	return &ekstypes.Nodegroup{Status: ekstypes.NodegroupStatusActive, CapacityType: capacity, Subnets: []string{subnet}, ScalingConfig: &ekstypes.NodegroupScalingConfig{DesiredSize: aws.Int32(size)}, Labels: map[string]string{MachineLabelKey: machine}, Tags: map[string]string{"kyber.io/managed-by": "kyber", "kyber.io/machine": machine}}
}

func validEKSConfig() ProviderConfig {
	return ProviderConfig{
		EKSConfigRegion: "us-east-1", EKSConfigCluster: "kyber",
		EKSConfigNodeRoleARN:   "arn:aws:iam::123456789012:role/kyber-node",
		EKSConfigAllowedZones:  `["us-east-1a","us-east-1b"]`,
		EKSConfigSubnetsByZone: `{"us-east-1a":"subnet-a","us-east-1b":"subnet-b"}`,
		EKSConfigProfiles:      `[{"id":"small","cpu":"2","memory":"8Gi","instanceTypes":["m7i.large"],"diskSizeGb":100,"availabilityClasses":["reliable","costOptimized"]}]`,
	}
}

func TestParseEKSConfig(t *testing.T) {
	a, err := parseEKSConfig(validEKSConfig())
	if err != nil {
		t.Fatalf("parseEKSConfig: %v", err)
	}
	if a.region != "us-east-1" || len(a.profiles) != 1 || len(a.allowedZones) != 2 {
		t.Fatalf("adapter = %+v", a)
	}
	if err := a.Validate(context.Background(), DesiredMachine{Profile: "small", Location: "us-east-1a"}); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestParseEKSConfigFailsClosed(t *testing.T) {
	for _, mutate := range []func(ProviderConfig){
		func(c ProviderConfig) { delete(c, EKSConfigRegion) },
		func(c ProviderConfig) { c[EKSConfigProfiles] = `[]` },
		func(c ProviderConfig) { c[EKSConfigAllowedZones] = `[]` },
		func(c ProviderConfig) { c[EKSConfigProfiles] = `[{` },
	} {
		cfg := validEKSConfig()
		mutate(cfg)
		if _, err := parseEKSConfig(cfg); err == nil {
			t.Fatal("parseEKSConfig unexpectedly succeeded")
		}
	}
}

func TestParseEKSConfigRejectsIncompleteLaunchTemplateAndUnknownClass(t *testing.T) {
	for _, profiles := range []string{
		`[{"id":"small","instanceTypes":["m7i.large"],"diskSizeGb":100,"availabilityClasses":["reliable"],"launchTemplateId":"lt-123"}]`,
		`[{"id":"small","instanceTypes":["m7i.large"],"diskSizeGb":100,"availabilityClasses":["cheap"]}]`,
	} {
		cfg := validEKSConfig()
		cfg[EKSConfigProfiles] = profiles
		if _, err := parseEKSConfig(cfg); err == nil {
			t.Fatalf("invalid profile accepted: %s", profiles)
		}
	}
}

func TestEKSValidateRejectsUnknownZoneAndAcceptsCostOptimized(t *testing.T) {
	a, _ := parseEKSConfig(validEKSConfig())
	if err := a.Validate(context.Background(), DesiredMachine{Profile: "small", Location: "us-west-2a"}); err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("zone error = %v", err)
	}
	if err := a.Validate(context.Background(), DesiredMachine{Profile: "small", Location: "us-east-1a", Interruptible: true}); err != nil {
		t.Fatalf("cost optimized validation = %v", err)
	}
}

func TestEKSValidateRejectsUnsupportedAvailabilityClass(t *testing.T) {
	cfg := validEKSConfig()
	cfg[EKSConfigProfiles] = `[{"id":"small","instanceTypes":["m7i.large"],"diskSizeGb":100,"availabilityClasses":["reliable"]}]`
	a, _ := parseEKSConfig(cfg)
	err := a.Validate(context.Background(), DesiredMachine{Profile: "small", Location: "us-east-1a", Interruptible: true})
	if err == nil || !strings.Contains(err.Error(), "does not support") {
		t.Fatalf("validation error = %v", err)
	}
}

func TestEKSProviderRefRoundTripAndHostileRefs(t *testing.T) {
	a, _ := parseEKSConfig(validEKSConfig())
	ref := a.providerRef("kyber-worker-1")
	if got, err := a.parseProviderRef(ref); err != nil || got != "kyber-worker-1" {
		t.Fatalf("round trip = %q, %v", got, err)
	}
	for _, ref := range []ProviderRef{"", "gke://kyber/pool", "eks://other/pool", "eks://kyber/a/b", "eks://kyber/pool?x=1", "eks://kyber/%2F"} {
		if _, err := a.parseProviderRef(ref); err == nil {
			t.Errorf("parseProviderRef(%q) unexpectedly succeeded", ref)
		}
	}
}

func TestEKSReliableLifecycleAndOwnership(t *testing.T) {
	ctx := context.Background()
	a, _ := parseEKSConfig(validEKSConfig())
	client := &fakeEKSClient{}
	a.client = client
	desired := DesiredMachine{Availability: DesiredOnline, Profile: "small", Location: "us-east-1a"}
	id := MachineIdentity{Name: "worker"}
	created, err := a.Reconcile(ctx, id, desired, "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.State != CapacityPending || client.create == nil || client.create.CapacityType != ekstypes.CapacityTypesOnDemand || len(client.create.Subnets) != 1 || client.create.Subnets[0] != "subnet-a" {
		t.Fatalf("create observation/input = %+v / %+v", created, client.create)
	}
	client.nodegroup = &ekstypes.Nodegroup{Status: ekstypes.NodegroupStatusActive, CapacityType: ekstypes.CapacityTypesOnDemand, Subnets: []string{"subnet-a"}, ScalingConfig: &ekstypes.NodegroupScalingConfig{DesiredSize: aws.Int32(1)}, Labels: map[string]string{MachineLabelKey: "worker"}, Tags: map[string]string{"kyber.io/managed-by": "kyber", "kyber.io/machine": "worker"}}
	ready, err := a.Reconcile(ctx, id, desired, created.ProviderRef)
	if err != nil || ready.State != CapacityAvailable {
		t.Fatalf("ready = %+v, %v", ready, err)
	}
	desired.Availability = DesiredOffline
	desired.AttachmentObserved = true
	desired.AttachedNodes = 1
	stopping, err := a.Reconcile(ctx, id, desired, created.ProviderRef)
	if err != nil || stopping.State != CapacityRecovering || client.update == nil || *client.update.ScalingConfig.DesiredSize != 0 {
		t.Fatalf("stopping = %+v, %v", stopping, err)
	}
	client.nodegroup.ScalingConfig.DesiredSize = aws.Int32(0)
	desired.AttachedNodes = 0
	offline, err := a.Reconcile(ctx, id, desired, created.ProviderRef)
	if err != nil || offline.State != CapacityOffline {
		t.Fatalf("offline = %+v, %v", offline, err)
	}
	desired.Availability = DesiredDeleted
	if _, err := a.Reconcile(ctx, id, desired, created.ProviderRef); err != nil || !client.deleted {
		t.Fatalf("delete: %v deleted=%v", err, client.deleted)
	}
}

func TestEKSLaunchTemplateRetainsInstallerInstanceChoicesAndDisk(t *testing.T) {
	cfg := validEKSConfig()
	cfg[EKSConfigProfiles] = `[{"id":"small","cpu":"2","memory":"8Gi","instanceTypes":["m7i.large","m7a.large"],"diskSizeGb":100,"availabilityClasses":["reliable","costOptimized"],"launchTemplateId":"lt-123","launchTemplateVersion":"7"}]`
	a, _ := parseEKSConfig(cfg)
	client := &fakeEKSClient{}
	a.client = client
	_, err := a.Reconcile(context.Background(), MachineIdentity{Name: "worker"}, DesiredMachine{Availability: DesiredOnline, Profile: "small", Location: "us-east-1a"}, "")
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if client.create.LaunchTemplate == nil || aws.ToString(client.create.LaunchTemplate.Id) != "lt-123" || aws.ToString(client.create.LaunchTemplate.Version) != "7" || len(client.create.InstanceTypes) != 2 || client.create.DiskSize != nil {
		t.Fatalf("create input = %+v", client.create)
	}
}

func TestEKSRefusesMutationWithoutOwnership(t *testing.T) {
	a, _ := parseEKSConfig(validEKSConfig())
	a.client = &fakeEKSClient{nodegroup: &ekstypes.Nodegroup{Status: ekstypes.NodegroupStatusActive, CapacityType: ekstypes.CapacityTypesOnDemand, Subnets: []string{"subnet-a"}, ScalingConfig: &ekstypes.NodegroupScalingConfig{DesiredSize: aws.Int32(1)}}}
	got, err := a.Reconcile(context.Background(), MachineIdentity{Name: "worker"}, DesiredMachine{Availability: DesiredDeleted, Profile: "small", Location: "us-east-1a"}, a.providerRef("kyber-worker"))
	if err != nil || got.State != CapacityFailed || !strings.Contains(got.Message, "ownership") {
		t.Fatalf("observation = %+v, %v", got, err)
	}
}

func TestEKSDeletionRequiresAuthoritativeNodeAbsence(t *testing.T) {
	a, _ := parseEKSConfig(validEKSConfig())
	client := &fakeEKSClient{nodegroup: pairGroup("worker", "subnet-a", ekstypes.CapacityTypesOnDemand, 0)}
	a.client = client
	got, err := a.Reconcile(context.Background(), MachineIdentity{Name: "worker"}, DesiredMachine{Availability: DesiredDeleted, Profile: "small", Location: "us-east-1a"}, a.providerRef("kyber-worker"))
	if err != nil || got.State != CapacityRecovering || client.deleted {
		t.Fatalf("unobserved deletion = %+v err=%v deleted=%v", got, err, client.deleted)
	}
}

func TestEKSCostOptimizedFallbackAndRetry(t *testing.T) {
	ctx := context.Background()
	a, _ := parseEKSConfig(validEKSConfig())
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	a.now = func() time.Time { return now }
	c := &fakeEKSClient{groups: map[string]*ekstypes.Nodegroup{}}
	a.client = c
	id := MachineIdentity{Name: "spot-worker"}
	d := DesiredMachine{Availability: DesiredOnline, Profile: "small", Location: "us-east-1a", Interruptible: true, AttachmentObserved: true}
	first, _ := a.Reconcile(ctx, id, d, "")
	if first.State != CapacityPending || len(c.creates) != 1 || c.creates[0].CapacityType != ekstypes.CapacityTypesSpot {
		t.Fatalf("spot create = %+v", first)
	}
	base := eksNodeGroupName(id.Name)
	c.groups[base+"-spot"] = pairGroup(id.Name, "subnet-a", ekstypes.CapacityTypesSpot, 1)
	a.Reconcile(ctx, id, d, first.ProviderRef)
	if len(c.creates) != 2 || c.creates[1].CapacityType != ekstypes.CapacityTypesOnDemand || aws.ToInt32(c.creates[1].ScalingConfig.DesiredSize) != 0 {
		t.Fatal("reliable fallback was not pre-created at zero")
	}
	c.groups[base+"-reliable"] = pairGroup(id.Name, "subnet-a", ekstypes.CapacityTypesOnDemand, 0)
	d.CostOptimizedUnavailableSince = now.Add(-5 * time.Minute)
	d.AttachedNodes = 0
	fallbackStarted, _ := a.Reconcile(ctx, id, d, first.ProviderRef)
	if groupSize(c.groups[base+"-spot"]) != 0 {
		t.Fatal("spot not scaled to zero")
	}
	if fallbackStarted.FallbackSince.IsZero() {
		t.Fatal("fallback start was not persisted with Spot scale-down")
	}
	d.FallbackSince = fallbackStarted.FallbackSince
	fallback, _ := a.Reconcile(ctx, id, d, first.ProviderRef)
	if groupSize(c.groups[base+"-reliable"]) != 1 || fallback.EffectiveAvailabilityClass != "reliable" {
		t.Fatalf("fallback = %+v", fallback)
	}
	d.AttachedNodes = 1
	ready, _ := a.Reconcile(ctx, id, d, first.ProviderRef)
	if ready.State != CapacityAvailable || ready.ProviderRef != first.ProviderRef {
		t.Fatalf("ready fallback = %+v", ready)
	}
	d.CostOptimizedRetryRequest = "retry-1"
	retryStarted, _ := a.Reconcile(ctx, id, d, first.ProviderRef)
	if retryStarted.CostOptimizedRetrySince.IsZero() {
		t.Fatal("retry start was not persisted before moving capacity")
	}
	d.CostOptimizedRetrySince = retryStarted.CostOptimizedRetrySince
	a.Reconcile(ctx, id, d, first.ProviderRef)
	if groupSize(c.groups[base+"-reliable"]) != 0 {
		t.Fatal("reliable not scaled down for retry")
	}
	d.AttachedNodes = 0
	a.Reconcile(ctx, id, d, first.ProviderRef)
	if groupSize(c.groups[base+"-spot"]) != 1 {
		t.Fatal("spot not scaled up for retry")
	}
	d.AttachedNodes = 1
	retried, _ := a.Reconcile(ctx, id, d, first.ProviderRef)
	if retried.EffectiveAvailabilityClass != "costOptimized" || retried.CostOptimizedRetryObserved != "retry-1" || retried.ProviderRef != first.ProviderRef {
		t.Fatalf("retry = %+v", retried)
	}
	if !retried.CostOptimizedRetrySince.IsZero() || !retried.FallbackSince.IsZero() || !retried.CostOptimizedUnavailableSince.IsZero() {
		t.Fatalf("successful retry retained stale fallback state: %+v", retried)
	}
}

func TestEKSLateSpotNodeCannotResetFallbackOrStartDualCapacity(t *testing.T) {
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	a, _ := parseEKSConfig(validEKSConfig())
	a.now = func() time.Time { return now }
	base := eksNodeGroupName("spot-worker")
	c := &fakeEKSClient{groups: map[string]*ekstypes.Nodegroup{
		base + "-spot":     pairGroup("spot-worker", "subnet-a", ekstypes.CapacityTypesSpot, 0),
		base + "-reliable": pairGroup("spot-worker", "subnet-a", ekstypes.CapacityTypesOnDemand, 0),
	}}
	a.client = c
	d := DesiredMachine{Availability: DesiredOnline, Profile: "small", Location: "us-east-1a", Interruptible: true, AttachmentObserved: true, AttachedNodes: 1, CostOptimizedUnavailableSince: now.Add(-5 * time.Minute), FallbackSince: now}
	waiting, err := a.Reconcile(context.Background(), MachineIdentity{Name: "spot-worker"}, d, a.providerRef(base))
	if err != nil || waiting.State != CapacityRecovering || waiting.EffectiveAvailabilityClass != "costOptimized" || waiting.FallbackSince != now {
		t.Fatalf("late-node observation = %+v err=%v", waiting, err)
	}
	if groupSize(c.groups[base+"-reliable"]) != 0 {
		t.Fatal("reliable capacity started while a late Spot Node was attached")
	}
	d.AttachedNodes = 0
	_, _ = a.Reconcile(context.Background(), MachineIdentity{Name: "spot-worker"}, d, a.providerRef(base))
	if groupSize(c.groups[base+"-reliable"]) != 1 || groupSize(c.groups[base+"-spot"]) != 0 {
		t.Fatal("reliable capacity did not start in isolation after detach")
	}
}

func TestEKSCostOptimizedRetryRollsBackWithoutDualCapacity(t *testing.T) {
	ctx := context.Background()
	a, _ := parseEKSConfig(validEKSConfig())
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	a.now = func() time.Time { return now }
	base := eksNodeGroupName("spot-worker")
	c := &fakeEKSClient{groups: map[string]*ekstypes.Nodegroup{
		base + "-spot":     pairGroup("spot-worker", "subnet-a", ekstypes.CapacityTypesSpot, 0),
		base + "-reliable": pairGroup("spot-worker", "subnet-a", ekstypes.CapacityTypesOnDemand, 1),
	}}
	a.client = c
	ref := a.providerRef(base)
	d := DesiredMachine{Availability: DesiredOnline, Profile: "small", Location: "us-east-1a", Interruptible: true, AttachmentObserved: true, AttachedNodes: 1, FallbackSince: now.Add(-time.Hour), CostOptimizedUnavailableSince: now.Add(-time.Hour), CostOptimizedRetryRequest: "retry-1"}
	started, _ := a.Reconcile(ctx, MachineIdentity{Name: "spot-worker"}, d, ref)
	d.CostOptimizedRetrySince = started.CostOptimizedRetrySince
	if d.CostOptimizedRetrySince.IsZero() || groupSize(c.groups[base+"-reliable"]) != 1 {
		t.Fatalf("retry was not durably initialized before mutation: %+v", started)
	}

	// Remove reliable, wait for its Node, and request Spot. At no point may
	// both native groups have desired capacity.
	a.Reconcile(ctx, MachineIdentity{Name: "spot-worker"}, d, ref)
	if groupSize(c.groups[base+"-reliable"]) != 0 || groupSize(c.groups[base+"-spot"]) != 0 {
		t.Fatal("reliable shutdown produced dual capacity")
	}
	d.AttachedNodes = 0
	a.Reconcile(ctx, MachineIdentity{Name: "spot-worker"}, d, ref)
	if groupSize(c.groups[base+"-spot"]) != 1 || groupSize(c.groups[base+"-reliable"]) != 0 {
		t.Fatal("spot retry produced dual capacity")
	}

	// Spot never attaches. After the bounded window, remove it first, then
	// restore reliable and acknowledge the one-shot request.
	now = now.Add(5 * time.Minute)
	a.Reconcile(ctx, MachineIdentity{Name: "spot-worker"}, d, ref)
	if groupSize(c.groups[base+"-spot"]) != 0 || groupSize(c.groups[base+"-reliable"]) != 0 {
		t.Fatal("rollback did not remove Spot before restoring reliable")
	}
	a.Reconcile(ctx, MachineIdentity{Name: "spot-worker"}, d, ref)
	if groupSize(c.groups[base+"-reliable"]) != 1 || groupSize(c.groups[base+"-spot"]) != 0 {
		t.Fatal("rollback did not restore reliable in isolation")
	}
	d.AttachedNodes = 1
	rolledBack, _ := a.Reconcile(ctx, MachineIdentity{Name: "spot-worker"}, d, ref)
	if rolledBack.State != CapacityAvailable || rolledBack.EffectiveAvailabilityClass != "reliable" || rolledBack.CostOptimizedRetryObserved != "retry-1" || !rolledBack.CostOptimizedRetrySince.IsZero() || rolledBack.ProviderRef != ref {
		t.Fatalf("rollback = %+v", rolledBack)
	}
}

func TestEKSNodeGroupNameKeepsLongMachineNamesDistinct(t *testing.T) {
	left := eksNodeGroupName("machine-with-a-very-long-shared-prefix-that-only-differs-at-the-end-left")
	right := eksNodeGroupName("machine-with-a-very-long-shared-prefix-that-only-differs-at-the-end-right")
	if left == right {
		t.Fatalf("long machine names collided: %q", left)
	}
	for _, name := range []string{left, right} {
		if len(name) != 54 {
			t.Fatalf("node group name %q has length %d, want 54", name, len(name))
		}
		if !strings.HasPrefix(name, "kyber-") {
			t.Fatalf("node group name %q lost its ownership prefix", name)
		}
	}
}

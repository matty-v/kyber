package adapters

import (
	"context"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	ekstypes "github.com/aws/aws-sdk-go-v2/service/eks/types"
)

type fakeEKSClient struct {
	nodegroup *ekstypes.Nodegroup
	create    *eks.CreateNodegroupInput
	update    *eks.UpdateNodegroupConfigInput
	deleted   bool
}

func (f *fakeEKSClient) DescribeNodegroup(context.Context, *eks.DescribeNodegroupInput, ...func(*eks.Options)) (*eks.DescribeNodegroupOutput, error) {
	if f.nodegroup == nil {
		return nil, &ekstypes.ResourceNotFoundException{Message: aws.String("missing")}
	}
	return &eks.DescribeNodegroupOutput{Nodegroup: f.nodegroup}, nil
}
func (f *fakeEKSClient) CreateNodegroup(_ context.Context, in *eks.CreateNodegroupInput, _ ...func(*eks.Options)) (*eks.CreateNodegroupOutput, error) {
	f.create = in
	return &eks.CreateNodegroupOutput{}, nil
}
func (f *fakeEKSClient) UpdateNodegroupConfig(_ context.Context, in *eks.UpdateNodegroupConfigInput, _ ...func(*eks.Options)) (*eks.UpdateNodegroupConfigOutput, error) {
	f.update = in
	return &eks.UpdateNodegroupConfigOutput{}, nil
}
func (f *fakeEKSClient) DeleteNodegroup(context.Context, *eks.DeleteNodegroupInput, ...func(*eks.Options)) (*eks.DeleteNodegroupOutput, error) {
	f.deleted = true
	return &eks.DeleteNodegroupOutput{}, nil
}

func validEKSConfig() ProviderConfig {
	return ProviderConfig{
		EKSConfigRegion: "us-east-1", EKSConfigCluster: "kyber",
		EKSConfigNodeRoleARN:   "arn:aws:iam::123456789012:role/kyber-node",
		EKSConfigAllowedZones:  `["us-east-1a","us-east-1b"]`,
		EKSConfigSubnetsByZone: `{"us-east-1a":"subnet-a","us-east-1b":"subnet-b"}`,
		EKSConfigProfiles:      `[{"id":"small","cpu":"2","memory":"8Gi","instanceTypes":["m7i.large"],"diskSizeGb":100,"availabilityClasses":["reliable"]}]`,
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

func TestEKSValidateRejectsUnknownZoneAndCostOptimized(t *testing.T) {
	a, _ := parseEKSConfig(validEKSConfig())
	if err := a.Validate(context.Background(), DesiredMachine{Profile: "small", Location: "us-west-2a"}); err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("zone error = %v", err)
	}
	if err := a.Validate(context.Background(), DesiredMachine{Profile: "small", Location: "us-east-1a", Interruptible: true}); err == nil || !strings.Contains(err.Error(), "not enabled") {
		t.Fatalf("cost error = %v", err)
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

func TestEKSRefusesMutationWithoutOwnership(t *testing.T) {
	a, _ := parseEKSConfig(validEKSConfig())
	a.client = &fakeEKSClient{nodegroup: &ekstypes.Nodegroup{Status: ekstypes.NodegroupStatusActive, CapacityType: ekstypes.CapacityTypesOnDemand, Subnets: []string{"subnet-a"}, ScalingConfig: &ekstypes.NodegroupScalingConfig{DesiredSize: aws.Int32(1)}}}
	got, err := a.Reconcile(context.Background(), MachineIdentity{Name: "worker"}, DesiredMachine{Availability: DesiredDeleted, Profile: "small", Location: "us-east-1a"}, a.providerRef("kyber-worker"))
	if err != nil || got.State != CapacityFailed || !strings.Contains(got.Message, "ownership") {
		t.Fatalf("observation = %+v, %v", got, err)
	}
}

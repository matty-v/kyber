package adapters

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	ekstypes "github.com/aws/aws-sdk-go-v2/service/eks/types"
	"github.com/aws/smithy-go"
)

const (
	EKSConfigRegion        = "eks.region"
	EKSConfigCluster       = "eks.cluster"
	EKSConfigProfiles      = "eks.profiles"
	EKSConfigAllowedZones  = "eks.allowedZones"
	EKSConfigNodeRoleARN   = "eks.nodeRoleArn"
	EKSConfigSubnetsByZone = "eks.subnetsByZone"
)

type EKSProfile struct {
	ID                    string   `json:"id"`
	DisplayName           string   `json:"displayName"`
	CPU                   string   `json:"cpu"`
	Memory                string   `json:"memory"`
	InstanceTypes         []string `json:"instanceTypes"`
	DiskSizeGB            int32    `json:"diskSizeGb"`
	AvailabilityClasses   []string `json:"availabilityClasses"`
	LaunchTemplateID      string   `json:"launchTemplateId,omitempty"`
	LaunchTemplateVersion string   `json:"launchTemplateVersion,omitempty"`
}

type eksClient interface {
	DescribeNodegroup(context.Context, *eks.DescribeNodegroupInput, ...func(*eks.Options)) (*eks.DescribeNodegroupOutput, error)
	CreateNodegroup(context.Context, *eks.CreateNodegroupInput, ...func(*eks.Options)) (*eks.CreateNodegroupOutput, error)
	UpdateNodegroupConfig(context.Context, *eks.UpdateNodegroupConfigInput, ...func(*eks.Options)) (*eks.UpdateNodegroupConfigOutput, error)
	DeleteNodegroup(context.Context, *eks.DeleteNodegroupInput, ...func(*eks.Options)) (*eks.DeleteNodegroupOutput, error)
}

type EKSAdapter struct {
	region, cluster, nodeRoleARN string
	allowedZones                 map[string]struct{}
	profiles                     map[string]EKSProfile
	subnetsByZone                map[string]string
	client                       eksClient
}

func (e *EKSAdapter) providerRef(nodeGroup string) ProviderRef {
	return ProviderRef("eks://" + url.PathEscape(e.cluster) + "/" + url.PathEscape(nodeGroup))
}

func (e *EKSAdapter) parseProviderRef(ref ProviderRef) (string, error) {
	u, err := url.Parse(string(ref))
	if err != nil || u.Scheme != "eks" || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("invalid EKS provider ref")
	}
	cluster, err := url.PathUnescape(u.Host)
	if err != nil || cluster != e.cluster {
		return "", fmt.Errorf("EKS provider ref belongs to another cluster")
	}
	parts := strings.Split(strings.TrimPrefix(u.EscapedPath(), "/"), "/")
	if len(parts) != 1 || parts[0] == "" {
		return "", fmt.Errorf("invalid EKS provider ref path")
	}
	name, err := url.PathUnescape(parts[0])
	if err != nil || name == "" || strings.Contains(name, "/") {
		return "", fmt.Errorf("invalid EKS node group name")
	}
	return name, nil
}

func init() {
	RegisterComputeProvider("eks", func(ctx context.Context, cfg ProviderConfig) (ComputeAdapter, error) {
		adapter, err := parseEKSConfig(cfg)
		if err != nil {
			return nil, err
		}
		awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(adapter.region))
		if err != nil {
			return nil, fmt.Errorf("loading AWS default credential chain: %w", err)
		}
		adapter.client = eks.NewFromConfig(awsCfg)
		return adapter, nil
	})
}

func parseEKSConfig(cfg ProviderConfig) (*EKSAdapter, error) {
	required := []string{EKSConfigRegion, EKSConfigCluster, EKSConfigProfiles, EKSConfigAllowedZones, EKSConfigNodeRoleARN, EKSConfigSubnetsByZone}
	for _, key := range required {
		if strings.TrimSpace(cfg[key]) == "" {
			return nil, fmt.Errorf("%s is required", key)
		}
	}
	var profiles []EKSProfile
	if err := json.Unmarshal([]byte(cfg[EKSConfigProfiles]), &profiles); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", EKSConfigProfiles, err)
	}
	var zones []string
	if err := json.Unmarshal([]byte(cfg[EKSConfigAllowedZones]), &zones); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", EKSConfigAllowedZones, err)
	}
	if len(profiles) == 0 || len(zones) == 0 {
		return nil, fmt.Errorf("EKS profiles and allowed zones must not be empty")
	}
	var subnets map[string]string
	if err := json.Unmarshal([]byte(cfg[EKSConfigSubnetsByZone]), &subnets); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", EKSConfigSubnetsByZone, err)
	}
	a := &EKSAdapter{region: cfg[EKSConfigRegion], cluster: cfg[EKSConfigCluster], nodeRoleARN: cfg[EKSConfigNodeRoleARN], profiles: map[string]EKSProfile{}, allowedZones: map[string]struct{}{}, subnetsByZone: subnets}
	for _, zone := range zones {
		if strings.TrimSpace(zone) == "" {
			return nil, fmt.Errorf("EKS allowed zone must not be empty")
		}
		a.allowedZones[zone] = struct{}{}
		if strings.TrimSpace(subnets[zone]) == "" {
			return nil, fmt.Errorf("EKS allowed zone %q has no subnet", zone)
		}
	}
	for _, profile := range profiles {
		if profile.ID == "" || len(profile.InstanceTypes) == 0 || profile.DiskSizeGB < 1 {
			return nil, fmt.Errorf("invalid EKS profile %q", profile.ID)
		}
		if _, exists := a.profiles[profile.ID]; exists {
			return nil, fmt.Errorf("duplicate EKS profile %q", profile.ID)
		}
		a.profiles[profile.ID] = profile
	}
	return a, nil
}

func (e *EKSAdapter) Type() string                       { return "eks" }
func (e *EKSAdapter) NodeAttachment() NodeAttachmentMode { return NodeAttachmentManaged }
func (e *EKSAdapter) Capabilities(context.Context) (Capabilities, error) {
	return Capabilities{CanProvision: true, SuspendMode: SuspendCapacity, DeletionMode: DeleteCapacity, SupportsReliable: true, SupportsInterruptible: false, SupportsLocations: true, ReliableFallbackMode: ReliableFallbackUnsupported}, nil
}
func (e *EKSAdapter) Profiles(context.Context) ([]Profile, error) {
	out := make([]Profile, 0, len(e.profiles))
	for _, p := range e.profiles {
		out = append(out, Profile{ID: p.ID, DisplayName: p.DisplayName, CPU: p.CPU, Memory: p.Memory, AvailabilityClasses: append([]string(nil), p.AvailabilityClasses...)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}
func (e *EKSAdapter) Validate(_ context.Context, d DesiredMachine) error {
	if _, ok := e.profiles[d.Profile]; !ok {
		return fmt.Errorf("validating EKS capacity: unknown profile %q", d.Profile)
	}
	if _, ok := e.allowedZones[d.Location]; !ok {
		return fmt.Errorf("validating EKS capacity: location %q is not allowed", d.Location)
	}
	if d.Interruptible {
		return fmt.Errorf("validating EKS capacity: cost-optimized lifecycle is not enabled yet")
	}
	return nil
}
func (e *EKSAdapter) Reconcile(ctx context.Context, identity MachineIdentity, desired DesiredMachine, ref ProviderRef) (CapacityObservation, error) {
	if err := e.Validate(ctx, desired); err != nil {
		return CapacityObservation{}, err
	}
	group := eksNodeGroupName(identity.Name)
	if ref != "" {
		parsed, err := e.parseProviderRef(ref)
		if err != nil {
			return CapacityObservation{}, err
		}
		group = parsed
	}
	stableRef := e.providerRef(group)
	selector := map[string]string{"eks.amazonaws.com/nodegroup": group}
	out, err := e.client.DescribeNodegroup(ctx, &eks.DescribeNodegroupInput{ClusterName: aws.String(e.cluster), NodegroupName: aws.String(group)})
	if err != nil {
		if !isEKSNotFound(err) {
			return CapacityObservation{}, fmt.Errorf("describing EKS node group: %w", err)
		}
		switch desired.Availability {
		case DesiredDeleted:
			return CapacityObservation{State: CapacityAbsent, Reason: ReasonDeleted}, nil
		case DesiredOffline:
			return CapacityObservation{State: CapacityOffline, Reason: ReasonStopped, ProviderRef: stableRef, Location: desired.Location, NodeSelector: selector}, nil
		case DesiredOnline:
			profile := e.profiles[desired.Profile]
			input := &eks.CreateNodegroupInput{ClusterName: aws.String(e.cluster), NodegroupName: aws.String(group), NodeRole: aws.String(e.nodeRoleARN), Subnets: []string{e.subnetsByZone[desired.Location]}, CapacityType: ekstypes.CapacityTypesOnDemand, InstanceTypes: append([]string(nil), profile.InstanceTypes...), DiskSize: aws.Int32(profile.DiskSizeGB), ScalingConfig: &ekstypes.NodegroupScalingConfig{MinSize: aws.Int32(0), MaxSize: aws.Int32(1), DesiredSize: aws.Int32(1)}, Labels: map[string]string{"kyber.io/managed-by": "kyber", MachineLabelKey: identity.Name}, Tags: map[string]string{"kyber.io/managed-by": "kyber", "kyber.io/machine": identity.Name, "kyber.io/location": desired.Location, "kyber.io/profile": desired.Profile}, ClientRequestToken: aws.String("create-" + group)}
			if profile.LaunchTemplateID != "" {
				input.LaunchTemplate = &ekstypes.LaunchTemplateSpecification{Id: aws.String(profile.LaunchTemplateID), Version: aws.String(profile.LaunchTemplateVersion)}
				input.InstanceTypes = nil
				input.DiskSize = nil
			}
			if _, err := e.client.CreateNodegroup(ctx, input); err != nil && !isEKSConflict(err) {
				return CapacityObservation{}, fmt.Errorf("creating EKS node group: %w", err)
			}
			return CapacityObservation{State: CapacityPending, Reason: ReasonProvisioning, ProviderRef: stableRef, Location: desired.Location, NodeSelector: selector, EffectiveAvailabilityClass: "reliable"}, nil
		}
	}
	ng := out.Nodegroup
	if ng == nil {
		return CapacityObservation{}, fmt.Errorf("describing EKS node group: empty response")
	}
	if err := e.validateOwnedNodeGroup(ng, identity, desired); err != nil {
		return CapacityObservation{State: CapacityFailed, Reason: ReasonProviderError, Message: err.Error(), ProviderRef: stableRef, Location: desired.Location, NodeSelector: selector}, nil
	}
	if desired.Availability == DesiredDeleted {
		if _, err := e.client.DeleteNodegroup(ctx, &eks.DeleteNodegroupInput{ClusterName: aws.String(e.cluster), NodegroupName: aws.String(group)}); err != nil && !isEKSNotFound(err) && !isEKSConflict(err) {
			return CapacityObservation{}, fmt.Errorf("deleting EKS node group: %w", err)
		}
		return CapacityObservation{State: CapacityRecovering, Reason: ReasonStopping, ProviderRef: stableRef, Location: desired.Location, NodeSelector: selector}, nil
	}
	want := int32(1)
	if desired.Availability == DesiredOffline {
		want = 0
	}
	if ng.ScalingConfig == nil || ng.ScalingConfig.DesiredSize == nil || *ng.ScalingConfig.DesiredSize != want {
		_, err := e.client.UpdateNodegroupConfig(ctx, &eks.UpdateNodegroupConfigInput{ClusterName: aws.String(e.cluster), NodegroupName: aws.String(group), ScalingConfig: &ekstypes.NodegroupScalingConfig{MinSize: aws.Int32(0), MaxSize: aws.Int32(1), DesiredSize: aws.Int32(want)}, ClientRequestToken: aws.String(fmt.Sprintf("size-%s-%d", group, want))})
		if err != nil && !isEKSConflict(err) {
			return CapacityObservation{}, fmt.Errorf("resizing EKS node group: %w", err)
		}
		return CapacityObservation{State: CapacityRecovering, Reason: ReasonRepairing, ProviderRef: stableRef, Location: desired.Location, NodeSelector: selector, EffectiveAvailabilityClass: "reliable"}, nil
	}
	obs := CapacityObservation{ProviderRef: stableRef, Location: desired.Location, NodeSelector: selector, EffectiveAvailabilityClass: "reliable"}
	if desired.Availability == DesiredOffline && desired.AttachmentObserved && desired.AttachedNodes == 0 {
		obs.State, obs.Reason = CapacityOffline, ReasonStopped
		return obs, nil
	}
	switch ng.Status {
	case ekstypes.NodegroupStatusActive:
		obs.State, obs.Reason = CapacityAvailable, ReasonReady
	case ekstypes.NodegroupStatusCreateFailed, ekstypes.NodegroupStatusDeleteFailed, ekstypes.NodegroupStatusDegraded:
		obs.State, obs.Reason = CapacityFailed, ReasonProviderError
	default:
		obs.State, obs.Reason = CapacityRecovering, ReasonRepairing
	}
	return obs, nil
}

func eksNodeGroupName(machine string) string {
	name := "kyber-" + strings.ToLower(machine)
	if len(name) > 63 {
		name = name[:63]
	}
	return name
}
func (e *EKSAdapter) validateOwnedNodeGroup(ng *ekstypes.Nodegroup, identity MachineIdentity, desired DesiredMachine) error {
	if ng.Tags["kyber.io/managed-by"] != "kyber" || ng.Tags["kyber.io/machine"] != identity.Name || ng.Labels[MachineLabelKey] != identity.Name {
		return fmt.Errorf("refusing to mutate EKS node group without Kyber ownership")
	}
	if len(ng.Subnets) != 1 || ng.Subnets[0] != e.subnetsByZone[desired.Location] {
		return fmt.Errorf("refusing cross-zone or multi-subnet EKS node group")
	}
	if ng.CapacityType != ekstypes.CapacityTypesOnDemand {
		return fmt.Errorf("reliable EKS node group has incompatible capacity type")
	}
	return nil
}
func isEKSNotFound(err error) bool {
	var api smithy.APIError
	return errors.As(err, &api) && api.ErrorCode() == "ResourceNotFoundException"
}
func isEKSConflict(err error) bool {
	var api smithy.APIError
	return errors.As(err, &api) && (api.ErrorCode() == "ResourceInUseException" || api.ErrorCode() == "ConflictException")
}
func (e *EKSAdapter) CreateInstance(context.Context, MachineSpec) (string, error) {
	return "", fmt.Errorf("EKS uses declarative capacity reconciliation")
}
func (e *EKSAdapter) StartInstance(context.Context, string) error {
	return fmt.Errorf("EKS uses declarative capacity reconciliation")
}
func (e *EKSAdapter) StopInstance(context.Context, string) error {
	return fmt.Errorf("EKS uses declarative capacity reconciliation")
}
func (e *EKSAdapter) DeleteInstance(context.Context, string) error {
	return fmt.Errorf("EKS uses declarative capacity reconciliation")
}
func (e *EKSAdapter) Observe(context.Context, string) (InstanceObservation, error) {
	return InstanceObservation{}, fmt.Errorf("EKS uses declarative capacity reconciliation")
}

var _ ComputeAdapter = (*EKSAdapter)(nil)
var _ CapacityProvider = (*EKSAdapter)(nil)

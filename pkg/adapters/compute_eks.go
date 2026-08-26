package adapters

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

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
	now                          func() time.Time
	fallbackThreshold            time.Duration
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
	fallbackThreshold, err := parseFallbackThreshold(cfg[ComputeConfigFallbackThreshold])
	if err != nil {
		return nil, err
	}
	a := &EKSAdapter{region: cfg[EKSConfigRegion], cluster: cfg[EKSConfigCluster], nodeRoleARN: cfg[EKSConfigNodeRoleARN], profiles: map[string]EKSProfile{}, allowedZones: map[string]struct{}{}, subnetsByZone: subnets, now: func() time.Time { return time.Now().UTC() }, fallbackThreshold: fallbackThreshold}
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
		if profile.ID == "" || len(profile.InstanceTypes) == 0 || profile.DiskSizeGB < 1 || len(profile.AvailabilityClasses) == 0 {
			return nil, fmt.Errorf("invalid EKS profile %q", profile.ID)
		}
		if (profile.LaunchTemplateID == "") != (profile.LaunchTemplateVersion == "") {
			return nil, fmt.Errorf("EKS profile %q must set launch template ID and version together", profile.ID)
		}
		for _, class := range profile.AvailabilityClasses {
			if class != "reliable" && class != "costOptimized" {
				return nil, fmt.Errorf("EKS profile %q has unsupported availability class %q", profile.ID, class)
			}
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
	return Capabilities{CanProvision: true, SuspendMode: SuspendCapacity, DeletionMode: DeleteCapacity, SupportsReliable: true, SupportsInterruptible: true, SupportsLocations: true, ReliableFallbackMode: ReliableFallbackAutomatic}, nil
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
	profile, ok := e.profiles[d.Profile]
	if !ok {
		return fmt.Errorf("validating EKS capacity: unknown profile %q", d.Profile)
	}
	if _, ok := e.allowedZones[d.Location]; !ok {
		return fmt.Errorf("validating EKS capacity: location %q is not allowed", d.Location)
	}
	class := d.AvailabilityClass
	if class == "" {
		if d.Interruptible {
			class = "costOptimized"
		} else {
			class = "reliable"
		}
	}
	if !containsString(profile.AvailabilityClasses, class) {
		return fmt.Errorf("validating EKS capacity: profile %q does not support availability class %q", d.Profile, class)
	}
	return nil
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
func (e *EKSAdapter) Reconcile(ctx context.Context, identity MachineIdentity, desired DesiredMachine, ref ProviderRef) (CapacityObservation, error) {
	if err := e.Validate(ctx, desired); err != nil {
		return CapacityObservation{}, err
	}
	if desired.Interruptible {
		return e.reconcileCostOptimized(ctx, identity, desired, ref)
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
		if ng.ScalingConfig == nil || ng.ScalingConfig.DesiredSize == nil || *ng.ScalingConfig.DesiredSize != 0 {
			_, err := e.client.UpdateNodegroupConfig(ctx, &eks.UpdateNodegroupConfigInput{ClusterName: aws.String(e.cluster), NodegroupName: aws.String(group), ScalingConfig: &ekstypes.NodegroupScalingConfig{MinSize: aws.Int32(0), MaxSize: aws.Int32(1), DesiredSize: aws.Int32(0)}, ClientRequestToken: aws.String(fmt.Sprintf("size-%s-0", group))})
			if err != nil && !isEKSConflict(err) {
				return CapacityObservation{}, fmt.Errorf("scaling EKS node group down for deletion: %w", err)
			}
			return CapacityObservation{State: CapacityRecovering, Reason: ReasonStopping, ProviderRef: stableRef, Location: desired.Location, NodeSelector: selector}, nil
		}
		if !desired.AttachmentObserved || desired.AttachedNodes > 0 {
			return CapacityObservation{State: CapacityRecovering, Reason: ReasonStopping, ProviderRef: stableRef, Location: desired.Location, NodeSelector: selector}, nil
		}
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

func (e *EKSAdapter) reconcileCostOptimized(ctx context.Context, identity MachineIdentity, desired DesiredMachine, ref ProviderRef) (CapacityObservation, error) {
	base := eksNodeGroupName(identity.Name)
	if ref != "" {
		parsed, err := e.parseProviderRef(ref)
		if err != nil {
			return CapacityObservation{}, err
		}
		base = parsed
	}
	stable := e.providerRef(base)
	selector := map[string]string{MachineLabelKey: identity.Name}
	spotName, reliableName := base+"-spot", base+"-reliable"
	spot, err := e.describeNodeGroup(ctx, spotName)
	if err != nil {
		return CapacityObservation{}, err
	}
	reliable, err := e.describeNodeGroup(ctx, reliableName)
	if err != nil {
		return CapacityObservation{}, err
	}
	if desired.Availability == DesiredDeleted {
		return e.deletePair(ctx, identity, desired, stable, selector, spot, reliable)
	}
	profile := e.profiles[desired.Profile]
	if spot == nil {
		if err := e.createNodeGroup(ctx, identity, desired, profile, spotName, ekstypes.CapacityTypesSpot, 1); err != nil {
			return CapacityObservation{}, err
		}
		return pairObservation(stable, selector, desired, CapacityPending, ReasonProvisioning, "costOptimized"), nil
	}
	if reliable == nil {
		if err := e.createNodeGroup(ctx, identity, desired, profile, reliableName, ekstypes.CapacityTypesOnDemand, 0); err != nil {
			return CapacityObservation{}, err
		}
		return pairObservation(stable, selector, desired, CapacityPending, ReasonProvisioning, "costOptimized"), nil
	}
	if err := e.validateOwnedPairGroup(spot, identity, desired, ekstypes.CapacityTypesSpot); err != nil {
		return failedPair(stable, selector, desired, err), nil
	}
	if err := e.validateOwnedPairGroup(reliable, identity, desired, ekstypes.CapacityTypesOnDemand); err != nil {
		return failedPair(stable, selector, desired, err), nil
	}
	if desired.Availability == DesiredOffline {
		if groupSize(spot) != 0 {
			return e.resizePair(ctx, spotName, 0, stable, selector, desired, "costOptimized")
		}
		if groupSize(reliable) != 0 {
			return e.resizePair(ctx, reliableName, 0, stable, selector, desired, "reliable")
		}
		if desired.AttachmentObserved && desired.AttachedNodes == 0 {
			return pairObservation(stable, selector, desired, CapacityOffline, ReasonStopped, "costOptimized"), nil
		}
		return pairObservation(stable, selector, desired, CapacityRecovering, ReasonStopping, "costOptimized"), nil
	}
	// Manual retry always removes reliable capacity before asking for Spot.
	if desired.CostOptimizedRetryRequest != "" && desired.CostOptimizedRetryRequest != desired.CostOptimizedRetryObserved {
		// Persist a retry-specific clock before moving capacity. FallbackSince is
		// the age of the reliable fallback, not the age of this retry attempt.
		if desired.CostOptimizedRetrySince.IsZero() {
			o := pairObservation(stable, selector, desired, CapacityAvailable, ReasonReady, "reliable")
			o.CostOptimizedRetrySince = e.now()
			return o, nil
		}
		if e.now().Sub(desired.CostOptimizedRetrySince) >= e.fallbackThreshold {
			// A bounded retry rolls back in the same safe order: remove Spot,
			// wait for its Node to detach, then restore reliable capacity.
			if groupSize(spot) != 0 {
				return e.resizePair(ctx, spotName, 0, stable, selector, desired, "reliable")
			}
			if groupSize(reliable) == 1 {
				o := pairObservation(stable, selector, desired, CapacityRecovering, ReasonRepairing, "reliable")
				o.FallbackReason = "Cost-optimized retry unavailable; reliable fallback retained"
				o.CostOptimizedRetryObserved = desired.CostOptimizedRetryRequest
				o.CostOptimizedRetrySince = time.Time{}
				if desired.AttachmentObserved && desired.AttachedNodes > 0 && reliable.Status == ekstypes.NodegroupStatusActive {
					o.State, o.Reason = CapacityAvailable, ReasonReady
				}
				return o, nil
			}
			if desired.AttachmentObserved && desired.AttachedNodes > 0 {
				return pairObservation(stable, selector, desired, CapacityRecovering, ReasonRepairing, "reliable"), nil
			}
			return e.resizePair(ctx, reliableName, 1, stable, selector, desired, "reliable")
		}
		if groupSize(reliable) != 0 {
			return e.resizePair(ctx, reliableName, 0, stable, selector, desired, "reliable")
		}
		if groupSize(spot) == 1 && desired.AttachmentObserved && desired.AttachedNodes > 0 && spot.Status == ekstypes.NodegroupStatusActive {
			o := pairObservation(stable, selector, desired, CapacityAvailable, ReasonReady, "costOptimized")
			o.CostOptimizedRetryObserved = desired.CostOptimizedRetryRequest
			o.CostOptimizedRetrySince = time.Time{}
			o.FallbackSince = time.Time{}
			o.FallbackReason = ""
			o.CostOptimizedUnavailableSince = time.Time{}
			return o, nil
		}
		if desired.AttachmentObserved && desired.AttachedNodes > 0 {
			return pairObservation(stable, selector, desired, CapacityRecovering, ReasonRepairing, "reliable"), nil
		}
		if groupSize(spot) != 1 {
			return e.resizePair(ctx, spotName, 1, stable, selector, desired, "reliable")
		}
		return pairObservation(stable, selector, desired, CapacityRecovering, ReasonRepairing, "reliable"), nil
	}
	// A live reliable group means fallback is active.
	if groupSize(reliable) == 1 {
		o := pairObservation(stable, selector, desired, CapacityRecovering, ReasonRepairing, "reliable")
		if o.FallbackSince.IsZero() {
			o.FallbackSince = e.now()
		}
		o.CostOptimizedUnavailableSince = desired.CostOptimizedUnavailableSince
		o.FallbackReason = "Cost-optimized capacity unavailable for 5 minutes"
		if desired.AttachmentObserved && desired.AttachedNodes > 0 && reliable.Status == ekstypes.NodegroupStatusActive {
			o.State, o.Reason = CapacityAvailable, ReasonReady
		}
		return o, nil
	}
	// Once fallback drain has started, a late Spot Node must be allowed to
	// detach; it must not reset the timer or be reported Ready. Reliable is
	// requested only after authoritative attachment count reaches zero.
	if !desired.FallbackSince.IsZero() {
		if desired.AttachmentObserved && desired.AttachedNodes > 0 {
			o := pairObservation(stable, selector, desired, CapacityRecovering, ReasonRepairing, "costOptimized")
			o.FallbackReason = "Cost-optimized capacity unavailable for 5 minutes"
			return o, nil
		}
		return e.resizePair(ctx, reliableName, 1, stable, selector, desired, "reliable")
	}
	if desired.AttachmentObserved && desired.AttachedNodes > 0 && spot.Status == ekstypes.NodegroupStatusActive {
		o := pairObservation(stable, selector, desired, CapacityAvailable, ReasonReady, "costOptimized")
		o.FallbackSince = time.Time{}
		o.FallbackReason = ""
		o.CostOptimizedUnavailableSince = time.Time{}
		o.CostOptimizedRetrySince = time.Time{}
		return o, nil
	}
	unavailable := desired.CostOptimizedUnavailableSince
	if unavailable.IsZero() {
		unavailable = e.now()
	}
	if e.now().Sub(unavailable) < e.fallbackThreshold {
		o := pairObservation(stable, selector, desired, CapacityRecovering, ReasonInterrupted, "costOptimized")
		o.CostOptimizedUnavailableSince = unavailable
		return o, nil
	}
	if groupSize(spot) != 0 {
		o, err := e.resizePair(ctx, spotName, 0, stable, selector, desired, "costOptimized")
		if err == nil {
			o.CostOptimizedUnavailableSince = unavailable
			o.FallbackSince = e.now()
			o.FallbackReason = "Cost-optimized capacity unavailable for 5 minutes"
		}
		return o, err
	}
	if desired.AttachmentObserved && desired.AttachedNodes > 0 {
		o := pairObservation(stable, selector, desired, CapacityRecovering, ReasonRepairing, "costOptimized")
		o.CostOptimizedUnavailableSince = unavailable
		return o, nil
	}
	if groupSize(reliable) != 1 {
		return e.resizePair(ctx, reliableName, 1, stable, selector, desired, "reliable")
	}
	o := pairObservation(stable, selector, desired, CapacityRecovering, ReasonRepairing, "reliable")
	o.CostOptimizedUnavailableSince = unavailable
	o.FallbackSince = e.now()
	o.FallbackReason = "Cost-optimized capacity unavailable for 5 minutes"
	return o, nil
}

func (e *EKSAdapter) describeNodeGroup(ctx context.Context, name string) (*ekstypes.Nodegroup, error) {
	out, err := e.client.DescribeNodegroup(ctx, &eks.DescribeNodegroupInput{ClusterName: aws.String(e.cluster), NodegroupName: aws.String(name)})
	if isEKSNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("describing EKS node group %s: %w", name, err)
	}
	if out.Nodegroup == nil {
		return nil, fmt.Errorf("describing EKS node group %s: empty response", name)
	}
	return out.Nodegroup, nil
}
func (e *EKSAdapter) createNodeGroup(ctx context.Context, id MachineIdentity, d DesiredMachine, p EKSProfile, name string, capacity ekstypes.CapacityTypes, size int32) error {
	in := &eks.CreateNodegroupInput{ClusterName: aws.String(e.cluster), NodegroupName: aws.String(name), NodeRole: aws.String(e.nodeRoleARN), Subnets: []string{e.subnetsByZone[d.Location]}, CapacityType: capacity, InstanceTypes: append([]string(nil), p.InstanceTypes...), DiskSize: aws.Int32(p.DiskSizeGB), ScalingConfig: &ekstypes.NodegroupScalingConfig{MinSize: aws.Int32(0), MaxSize: aws.Int32(1), DesiredSize: aws.Int32(size)}, Labels: map[string]string{"kyber.io/managed-by": "kyber", MachineLabelKey: id.Name}, Tags: map[string]string{"kyber.io/managed-by": "kyber", "kyber.io/machine": id.Name, "kyber.io/location": d.Location, "kyber.io/profile": d.Profile, "kyber.io/role": strings.ToLower(string(capacity))}, ClientRequestToken: aws.String("create-" + name)}
	if p.LaunchTemplateID != "" {
		in.LaunchTemplate = &ekstypes.LaunchTemplateSpecification{Id: aws.String(p.LaunchTemplateID), Version: aws.String(p.LaunchTemplateVersion)}
		in.DiskSize = nil
	}
	_, err := e.client.CreateNodegroup(ctx, in)
	if err != nil && !isEKSConflict(err) {
		return fmt.Errorf("creating EKS node group %s: %w", name, err)
	}
	return nil
}
func (e *EKSAdapter) validateOwnedPairGroup(ng *ekstypes.Nodegroup, id MachineIdentity, d DesiredMachine, capacity ekstypes.CapacityTypes) error {
	if err := e.validateOwnedNodeGroupCommon(ng, id, d); err != nil {
		return err
	}
	if ng.CapacityType != capacity {
		return fmt.Errorf("EKS node group has incompatible capacity type")
	}
	return nil
}
func (e *EKSAdapter) validateOwnedNodeGroupCommon(ng *ekstypes.Nodegroup, id MachineIdentity, d DesiredMachine) error {
	if ng.Tags["kyber.io/managed-by"] != "kyber" || ng.Tags["kyber.io/machine"] != id.Name || ng.Labels[MachineLabelKey] != id.Name {
		return fmt.Errorf("refusing to mutate EKS node group without Kyber ownership")
	}
	if len(ng.Subnets) != 1 || ng.Subnets[0] != e.subnetsByZone[d.Location] {
		return fmt.Errorf("refusing cross-zone or multi-subnet EKS node group")
	}
	return nil
}
func groupSize(ng *ekstypes.Nodegroup) int32 {
	if ng == nil || ng.ScalingConfig == nil || ng.ScalingConfig.DesiredSize == nil {
		return -1
	}
	return *ng.ScalingConfig.DesiredSize
}
func (e *EKSAdapter) resizePair(ctx context.Context, name string, size int32, ref ProviderRef, sel map[string]string, d DesiredMachine, effective string) (CapacityObservation, error) {
	_, err := e.client.UpdateNodegroupConfig(ctx, &eks.UpdateNodegroupConfigInput{ClusterName: aws.String(e.cluster), NodegroupName: aws.String(name), ScalingConfig: &ekstypes.NodegroupScalingConfig{MinSize: aws.Int32(0), MaxSize: aws.Int32(1), DesiredSize: aws.Int32(size)}, ClientRequestToken: aws.String(fmt.Sprintf("size-%s-%d", name, size))})
	if err != nil && !isEKSConflict(err) {
		return CapacityObservation{}, err
	}
	return pairObservation(ref, sel, d, CapacityRecovering, ReasonRepairing, effective), nil
}
func pairObservation(ref ProviderRef, sel map[string]string, d DesiredMachine, state AvailabilityState, reason AvailabilityReason, effective string) CapacityObservation {
	o := CapacityObservation{
		ProviderRef: ref, NodeSelector: sel, Location: d.Location, State: state, Reason: reason,
		EffectiveAvailabilityClass:    effective,
		FallbackSince:                 d.FallbackSince,
		CostOptimizedUnavailableSince: d.CostOptimizedUnavailableSince,
		CostOptimizedRetryObserved:    d.CostOptimizedRetryObserved,
		CostOptimizedRetrySince:       d.CostOptimizedRetrySince,
	}
	if !d.FallbackSince.IsZero() {
		o.FallbackReason = "Cost-optimized capacity unavailable for 5 minutes"
	}
	return o
}
func failedPair(ref ProviderRef, sel map[string]string, d DesiredMachine, err error) CapacityObservation {
	o := pairObservation(ref, sel, d, CapacityFailed, ReasonProviderError, "")
	o.Message = err.Error()
	return o
}
func (e *EKSAdapter) deletePair(ctx context.Context, id MachineIdentity, d DesiredMachine, ref ProviderRef, sel map[string]string, groups ...*ekstypes.Nodegroup) (CapacityObservation, error) {
	base := eksNodeGroupName(id.Name)
	if ref != "" {
		parsed, err := e.parseProviderRef(ref)
		if err != nil {
			return CapacityObservation{}, err
		}
		base = parsed
	}
	names := []string{base + "-spot", base + "-reliable"}
	allAbsent := true
	for i, g := range groups {
		if g == nil {
			continue
		}
		allAbsent = false
		if err := e.validateOwnedNodeGroupCommon(g, id, d); err != nil {
			return failedPair(ref, sel, d, err), nil
		}
		if groupSize(g) != 0 {
			return e.resizePair(ctx, names[i], 0, ref, sel, d, "")
		}
		if !d.AttachmentObserved || d.AttachedNodes > 0 {
			return pairObservation(ref, sel, d, CapacityRecovering, ReasonStopping, ""), nil
		}
		if _, err := e.client.DeleteNodegroup(ctx, &eks.DeleteNodegroupInput{ClusterName: aws.String(e.cluster), NodegroupName: aws.String(names[i])}); err != nil && !isEKSNotFound(err) && !isEKSConflict(err) {
			return CapacityObservation{}, err
		}
		return pairObservation(ref, sel, d, CapacityRecovering, ReasonStopping, ""), nil
	}
	if allAbsent {
		return CapacityObservation{State: CapacityAbsent, Reason: ReasonDeleted}, nil
	}
	return pairObservation(ref, sel, d, CapacityRecovering, ReasonStopping, ""), nil
}

func eksNodeGroupName(machine string) string {
	name := "kyber-" + strings.ToLower(machine)
	if len(name) > 54 {
		digest := sha256.Sum256([]byte(name))
		name = fmt.Sprintf("%s-%x", name[:45], digest[:4])
	}
	return name
}
func (e *EKSAdapter) validateOwnedNodeGroup(ng *ekstypes.Nodegroup, identity MachineIdentity, desired DesiredMachine) error {
	if err := e.validateOwnedNodeGroupCommon(ng, identity, desired); err != nil {
		return err
	}
	if ng.CapacityType != ekstypes.CapacityTypesOnDemand {
		return fmt.Errorf("reliable EKS node group has incompatible capacity type")
	}
	return nil
}
func (e *EKSAdapter) NodeSelector(identity MachineIdentity, _ ProviderRef) map[string]string {
	return map[string]string{MachineLabelKey: identity.Name}
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
var _ CapacityNodeSelector = (*EKSAdapter)(nil)

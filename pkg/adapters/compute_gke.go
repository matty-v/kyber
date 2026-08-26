package adapters

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"google.golang.org/api/container/v1"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
)

const (
	GKEConfigProject          = "gke-project"
	GKEConfigLocation         = "gke-location"
	GKEConfigCluster          = "gke-cluster"
	GKEConfigProfiles         = "gke-profiles"
	GKEConfigNodeLocations    = "gke-node-locations"
	GKEConfigReliableFallback = "gke-reliable-fallback-enabled"
)

const gkeNodePoolLabel = "cloud.google.com/gke-nodepool"

func init() {
	RegisterComputeProvider("gke", func(ctx context.Context, config ProviderConfig) (ComputeAdapter, error) {
		profiles, err := ParseGKEProfiles(config[GKEConfigProfiles])
		if err != nil {
			return nil, err
		}
		adapter, err := NewGKEAdapter(ctx, config[GKEConfigProject], config[GKEConfigLocation], config[GKEConfigCluster])
		if err != nil {
			return nil, err
		}
		adapter.profiles = profiles
		if raw := config[GKEConfigNodeLocations]; raw != "" {
			if err := json.Unmarshal([]byte(raw), &adapter.nodeLocations); err != nil {
				return nil, fmt.Errorf("parsing GKE node locations: %w", err)
			}
			if err := validateGKENodeLocations(adapter.Location, adapter.nodeLocations); err != nil {
				return nil, err
			}
		}
		if raw := config[GKEConfigReliableFallback]; raw != "" {
			enabled, err := strconv.ParseBool(raw)
			if err != nil {
				return nil, fmt.Errorf("parsing GKE reliable fallback setting: %w", err)
			}
			adapter.reliableFallback = enabled
		}
		fallbackThreshold, err := parseFallbackThreshold(config[ComputeConfigFallbackThreshold])
		if err != nil {
			return nil, err
		}
		adapter.fallbackThreshold = fallbackThreshold
		return adapter, nil
	})
}

type gkeNodePoolsClient interface {
	Get(context.Context, string) (*container.NodePool, error)
	Create(context.Context, string, *container.NodePool) (*container.Operation, error)
	SetSize(context.Context, string, int64) (*container.Operation, error)
	SetAutoscaling(context.Context, string, *container.NodePoolAutoscaling) (*container.Operation, error)
	Delete(context.Context, string) (*container.Operation, error)
}

type realGKENodePoolsClient struct {
	service *container.Service
}

func (r *realGKENodePoolsClient) Get(ctx context.Context, name string) (*container.NodePool, error) {
	return r.service.Projects.Locations.Clusters.NodePools.Get(name).Context(ctx).Do()
}
func (r *realGKENodePoolsClient) Create(ctx context.Context, parent string, pool *container.NodePool) (*container.Operation, error) {
	return r.service.Projects.Locations.Clusters.NodePools.Create(parent, &container.CreateNodePoolRequest{NodePool: pool}).Context(ctx).Do()
}
func (r *realGKENodePoolsClient) SetSize(ctx context.Context, name string, size int64) (*container.Operation, error) {
	return r.service.Projects.Locations.Clusters.NodePools.SetSize(name, &container.SetNodePoolSizeRequest{NodeCount: size}).Context(ctx).Do()
}
func (r *realGKENodePoolsClient) SetAutoscaling(ctx context.Context, name string, autoscaling *container.NodePoolAutoscaling) (*container.Operation, error) {
	return r.service.Projects.Locations.Clusters.NodePools.SetAutoscaling(name, &container.SetNodePoolAutoscalingRequest{Autoscaling: autoscaling}).Context(ctx).Do()
}
func (r *realGKENodePoolsClient) Delete(ctx context.Context, name string) (*container.Operation, error) {
	return r.service.Projects.Locations.Clusters.NodePools.Delete(name).Context(ctx).Do()
}

// GKEAdapter observes externally managed, size-one GKE node pools. Mutation is
// intentionally disabled until observation and identity have been validated
// against a target cluster.
type GKEAdapter struct {
	ProjectID         string
	Location          string
	Cluster           string
	client            gkeNodePoolsClient
	profiles          map[string]GKEProfile
	nodeLocations     []string
	reliableFallback  bool
	now               func() time.Time
	fallbackThreshold time.Duration
}

func validateGKENodeLocations(clusterLocation string, locations []string) error {
	seen := make(map[string]struct{}, len(locations))
	for _, location := range locations {
		if location == "" {
			return fmt.Errorf("validating GKE node locations: location is empty")
		}
		if _, ok := seen[location]; ok {
			return fmt.Errorf("validating GKE node locations: duplicate location %q", location)
		}
		seen[location] = struct{}{}
		if !strings.HasPrefix(location, clusterLocation+"-") {
			return fmt.Errorf("validating GKE node locations: %q is outside cluster location %q", location, clusterLocation)
		}
	}
	return nil
}

// GKEProfile is an installer-curated operator promise plus its private GKE
// realization. Provider-native fields never cross the public API boundary.
type GKEProfile struct {
	ID                  string   `json:"id"`
	DisplayName         string   `json:"displayName"`
	Description         string   `json:"description,omitempty"`
	CPU                 string   `json:"cpu"`
	Memory              string   `json:"memory"`
	AvailabilityClasses []string `json:"availabilityClasses"`
	Recommended         bool     `json:"recommended,omitempty"`
	MachineType         string   `json:"machineType"`
	DiskSizeGB          int64    `json:"diskSizeGb"`
	DiskType            string   `json:"diskType"`
	ImageType           string   `json:"imageType"`
}

func ParseGKEProfiles(raw string) (map[string]GKEProfile, error) {
	if raw == "" {
		return map[string]GKEProfile{}, nil
	}
	var entries []GKEProfile
	if err := json.Unmarshal([]byte(raw), &entries); err != nil {
		return nil, fmt.Errorf("parsing GKE profiles: %w", err)
	}
	out := make(map[string]GKEProfile, len(entries))
	for _, entry := range entries {
		if entry.ID == "" || entry.CPU == "" || entry.Memory == "" || entry.MachineType == "" || entry.DiskSizeGB < 10 || entry.DiskType == "" || entry.ImageType == "" {
			return nil, fmt.Errorf("parsing GKE profiles: profile %q is incomplete", entry.ID)
		}
		if _, exists := out[entry.ID]; exists {
			return nil, fmt.Errorf("parsing GKE profiles: duplicate profile %q", entry.ID)
		}
		out[entry.ID] = entry
	}
	return out, nil
}

func NewGKEAdapter(ctx context.Context, project, location, cluster string, opts ...option.ClientOption) (*GKEAdapter, error) {
	if project == "" || location == "" || cluster == "" {
		return nil, fmt.Errorf("creating GKE adapter: project, location, and cluster are required")
	}
	service, err := container.NewService(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("creating GKE service: %w", err)
	}
	return &GKEAdapter{ProjectID: project, Location: location, Cluster: cluster, client: &realGKENodePoolsClient{service: service}, now: func() time.Time { return time.Now().UTC() }, fallbackThreshold: 5 * time.Minute}, nil
}

func (g *GKEAdapter) Type() string                       { return "gke" }
func (g *GKEAdapter) NodeAttachment() NodeAttachmentMode { return NodeAttachmentManaged }
func (g *GKEAdapter) Close() error                       { return nil }

func (g *GKEAdapter) Capabilities(context.Context) (Capabilities, error) {
	managed := len(g.profiles) > 0
	suspendMode, deletionMode := SuspendUnsupported, UnregisterOnly
	if managed {
		suspendMode, deletionMode = SuspendCapacity, DeleteCapacity
	}
	return Capabilities{
		CanProvision: managed, CanDiscoverExisting: true,
		SuspendMode: suspendMode, DeletionMode: deletionMode,
		SupportsReliable: true, SupportsInterruptible: true, SupportsLocations: true,
		RequiresSchedulerDemand: g.regional(),
		ReliableFallbackMode: func() ReliableFallbackMode {
			if managed && g.reliableFallback {
				return ReliableFallbackAutomatic
			}
			return ReliableFallbackUnsupported
		}(),
	}, nil
}

func (g *GKEAdapter) Profiles(context.Context) ([]Profile, error) {
	out := make([]Profile, 0, len(g.profiles))
	for _, profile := range g.profiles {
		out = append(out, Profile{
			ID: profile.ID, DisplayName: profile.DisplayName, Description: profile.Description,
			CPU: profile.CPU, Memory: profile.Memory,
			AvailabilityClasses: append([]string(nil), profile.AvailabilityClasses...), Recommended: profile.Recommended,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (g *GKEAdapter) Locations(context.Context) ([]string, error) {
	return []string{g.Location}, nil
}

func (g *GKEAdapter) regional() bool { return len(g.nodeLocations) > 1 }

// NeedsSchedulerDemand reports whether this adapter's managed capacity uses
// regional GKE autoscaling. GKE scales these zero-node pools only in response
// to an unschedulable Pod; configured minimums are not proactive desired size.
func (g *GKEAdapter) NeedsSchedulerDemand() bool { return g.regional() }

func (g *GKEAdapter) onlineAutoscaling() *container.NodePoolAutoscaling {
	return &container.NodePoolAutoscaling{Enabled: true, LocationPolicy: "ANY", TotalMinNodeCount: 1, TotalMaxNodeCount: 1}
}

func (g *GKEAdapter) Validate(_ context.Context, desired DesiredMachine) error {
	if desired.Managed {
		profile, ok := g.profiles[desired.Profile]
		if !ok {
			return fmt.Errorf("validating GKE capacity: unknown profile %q", desired.Profile)
		}
		requestedClass := "reliable"
		if desired.Interruptible {
			requestedClass = "costOptimized"
		}
		supported := false
		for _, class := range profile.AvailabilityClasses {
			if class == requestedClass {
				supported = true
				break
			}
		}
		if !supported {
			return fmt.Errorf("validating GKE capacity: profile %q does not support availability class %q", desired.Profile, requestedClass)
		}
	}
	switch desired.Availability {
	case DesiredOnline, DesiredDeleted:
		return nil
	case DesiredOffline:
		if desired.Managed {
			return nil
		}
		return fmt.Errorf("validating GKE capacity: offline is unsupported for external capacity")
	default:
		return fmt.Errorf("validating GKE capacity: unsupported desired availability %q", desired.Availability)
	}
}

func (g *GKEAdapter) Reconcile(ctx context.Context, identity MachineIdentity, desired DesiredMachine, ref ProviderRef) (CapacityObservation, error) {
	if err := g.Validate(ctx, desired); err != nil {
		return CapacityObservation{}, err
	}
	if desired.Managed && desired.Interruptible && g.reliableFallback {
		return g.reconcileCostOptimized(ctx, identity, desired, ref)
	}
	pool, err := g.resolvePool(identity, ref)
	if err != nil {
		return CapacityObservation{}, err
	}
	stableRef := g.providerRef(pool)
	selector := map[string]string{gkeNodePoolLabel: pool}
	if desired.Availability == DesiredDeleted && !desired.Managed {
		return CapacityObservation{State: CapacityAbsent, Reason: ReasonDeleted}, nil
	}

	nodePool, err := g.client.Get(ctx, g.resourceName(pool))
	if err != nil {
		if isGKENotFound(err) {
			if desired.Availability == DesiredDeleted {
				return CapacityObservation{State: CapacityAbsent, Reason: ReasonDeleted}, nil
			}
			if desired.Availability == DesiredOffline {
				return CapacityObservation{State: CapacityOffline, Reason: ReasonStopped, ProviderRef: stableRef, Location: g.Location, NodeSelector: selector}, nil
			}
			if desired.Managed {
				profile := g.profiles[desired.Profile]
				_, createErr := g.client.Create(ctx, g.clusterName(), g.managedNodePool(pool, profile, desired))
				if createErr != nil && !isGKEConflict(createErr) {
					return CapacityObservation{}, fmt.Errorf("creating GKE node pool %s: %w", pool, createErr)
				}
				return CapacityObservation{State: CapacityPending, Reason: ReasonProvisioning, ProviderRef: stableRef, Location: g.Location, NodeSelector: selector}, nil
			}
			return CapacityObservation{
				State: CapacityPending, Reason: ReasonExternalWait,
				Message: "waiting for installer-managed GKE node pool", ProviderRef: stableRef,
				Location: g.Location, NodeSelector: selector,
			}, nil
		}
		return CapacityObservation{}, fmt.Errorf("getting GKE node pool %s: %w", pool, err)
	}

	if desired.Managed && !g.owned(nodePool, identity.Name) {
		return CapacityObservation{State: CapacityFailed, Reason: ReasonProviderError, Message: "refusing to mutate GKE node pool without Kyber ownership labels", ProviderRef: stableRef, Location: g.Location, NodeSelector: selector}, nil
	}
	if desired.Managed {
		switch desired.Availability {
		case DesiredDeleted:
			if !desired.AttachmentObserved {
				return CapacityObservation{State: CapacityRecovering, Reason: ReasonStopping, ProviderRef: stableRef, Location: g.Location, NodeSelector: selector}, nil
			}
			if desired.AttachedNodes > 0 {
				if g.regional() && autoscalingEnabled(nodePool.Autoscaling) {
					if _, err := g.client.SetAutoscaling(ctx, g.resourceName(pool), &container.NodePoolAutoscaling{Enabled: false}); err != nil && !isGKEConflict(err) {
						return CapacityObservation{}, fmt.Errorf("disabling GKE node pool autoscaling %s for deletion: %w", pool, err)
					}
				} else if _, err := g.client.SetSize(ctx, g.resourceName(pool), 0); err != nil && !isGKEConflict(err) {
					return CapacityObservation{}, fmt.Errorf("resizing GKE node pool %s to zero for deletion: %w", pool, err)
				}
				return CapacityObservation{State: CapacityRecovering, Reason: ReasonStopping, ProviderRef: stableRef, Location: g.Location, NodeSelector: selector}, nil
			}
			if nodePool.Status != "STOPPING" {
				if _, err := g.client.Delete(ctx, g.resourceName(pool)); err != nil && !isGKENotFound(err) && !isGKEConflict(err) {
					return CapacityObservation{}, fmt.Errorf("deleting GKE node pool %s: %w", pool, err)
				}
			}
			return CapacityObservation{State: CapacityRecovering, Reason: ReasonStopping, ProviderRef: stableRef, Location: g.Location, NodeSelector: selector}, nil
		case DesiredOffline:
			if desired.AttachmentObserved && desired.AttachedNodes == 0 && nodePool.Status == "RUNNING" {
				return CapacityObservation{State: CapacityOffline, Reason: ReasonStopped, ProviderRef: stableRef, Location: g.Location, NodeSelector: selector}, nil
			}
			if nodePool.Status == "RUNNING" && autoscalingEnabled(nodePool.Autoscaling) {
				if _, err := g.client.SetAutoscaling(ctx, g.resourceName(pool), &container.NodePoolAutoscaling{Enabled: false}); err != nil && !isGKEConflict(err) {
					return CapacityObservation{}, fmt.Errorf("disabling GKE node pool autoscaling %s: %w", pool, err)
				}
				return CapacityObservation{State: CapacityRecovering, Reason: ReasonStopping, ProviderRef: stableRef, Location: g.Location, NodeSelector: selector}, nil
			}
			if nodePool.Status == "RUNNING" {
				if _, err := g.client.SetSize(ctx, g.resourceName(pool), 0); err != nil && !isGKEConflict(err) {
					return CapacityObservation{}, fmt.Errorf("resizing GKE node pool %s to zero: %w", pool, err)
				}
			}
			return CapacityObservation{State: CapacityRecovering, Reason: ReasonStopping, ProviderRef: stableRef, Location: g.Location, NodeSelector: selector}, nil
		case DesiredOnline:
			if g.regional() && !regionalAutoscalingOnline(nodePool.Autoscaling) {
				if _, err := g.client.SetAutoscaling(ctx, g.resourceName(pool), g.onlineAutoscaling()); err != nil && !isGKEConflict(err) {
					return CapacityObservation{}, fmt.Errorf("enabling regional GKE node pool autoscaling %s: %w", pool, err)
				}
				return CapacityObservation{State: CapacityRecovering, Reason: ReasonProvisioning, Message: "Selecting available capacity across configured zones.", ProviderRef: stableRef, Location: g.Location, NodeSelector: selector}, nil
			}
			if !g.regional() && autoscalingEnabled(nodePool.Autoscaling) {
				if _, err := g.client.SetAutoscaling(ctx, g.resourceName(pool), &container.NodePoolAutoscaling{Enabled: false}); err != nil && !isGKEConflict(err) {
					return CapacityObservation{}, fmt.Errorf("disabling GKE node pool autoscaling %s: %w", pool, err)
				}
				return CapacityObservation{State: CapacityRecovering, Reason: ReasonRepairing, Message: "Restoring fixed-size capacity in the configured location.", ProviderRef: stableRef, Location: g.Location, NodeSelector: selector}, nil
			}
			if desired.AttachmentObserved && desired.AttachedNodes == 0 && nodePool.Status == "RUNNING" {
				// The NodePool API does not provide a reliable current target-size
				// observation: InitialNodeCount is creation intent and is not required
				// to reflect SetSize. Reissuing this idempotent desired size is noisy,
				// but it is safer than falsely declaring a zero-size pool repaired.
				if !g.regional() {
					if _, err := g.client.SetSize(ctx, g.resourceName(pool), 1); err != nil && !isGKEConflict(err) {
						return CapacityObservation{}, fmt.Errorf("resizing GKE node pool %s to one: %w", pool, err)
					}
				}
				return CapacityObservation{
					State: CapacityRecovering, Reason: ReasonRepairing,
					Message:     "Waiting for provider capacity to attach a Ready node. Kyber will keep retrying automatically.",
					ProviderRef: stableRef, Location: g.Location, NodeSelector: selector,
				}, nil
			}
		}
	}

	observation := CapacityObservation{
		ProviderRef: stableRef, Location: g.Location, NodeSelector: selector,
		Message: nodePool.StatusMessage,
	}
	switch nodePool.Status {
	case "RUNNING":
		observation.State, observation.Reason = CapacityAvailable, ReasonReady
	case "PROVISIONING":
		observation.State, observation.Reason = CapacityPending, ReasonProvisioning
	case "RECONCILING", "STOPPING":
		observation.State, observation.Reason = CapacityRecovering, ReasonRepairing
	case "RUNNING_WITH_ERROR":
		observation.State, observation.Reason = CapacityRecovering, ReasonProviderError
	case "ERROR":
		observation.State, observation.Reason = CapacityFailed, ReasonProviderError
	default:
		observation.State, observation.Reason = CapacityUnknown, ReasonUnknown
	}
	return observation, nil
}

func (g *GKEAdapter) reconcileCostOptimized(ctx context.Context, identity MachineIdentity, desired DesiredMachine, ref ProviderRef) (CapacityObservation, error) {
	base := gkePairBase(identity.Name)
	if ref != "" {
		resolved, err := g.resolvePool(identity, ref)
		if err != nil {
			return CapacityObservation{}, err
		}
		base = resolved
	}
	stable := g.providerRef(base)
	selector := map[string]string{MachineLabelKey: identity.Name}
	// Enabling fallback on an existing installation must not reinterpret or
	// orphan a legacy single pool. It keeps its original lifecycle; paired
	// pools are used only when the stable base name itself is absent.
	legacyPool, err := g.getPool(ctx, base)
	if err != nil {
		return CapacityObservation{}, err
	}
	if legacyPool != nil {
		legacy := *g
		legacy.reliableFallback = false
		return legacy.Reconcile(ctx, identity, desired, ref)
	}
	spotName, reliableName := base+"-spot", base+"-reliable"
	spot, err := g.getPool(ctx, spotName)
	if err != nil {
		return CapacityObservation{}, err
	}
	reliable, err := g.getPool(ctx, reliableName)
	if err != nil {
		return CapacityObservation{}, err
	}
	if desired.Availability == DesiredDeleted {
		return g.deletePair(ctx, identity, desired, stable, selector, spotName, reliableName, spot, reliable)
	}
	profile := g.profiles[desired.Profile]
	if spot == nil {
		pool := g.managedPairPool(spotName, identity.Name, profile, true, true)
		if _, err := g.client.Create(ctx, g.clusterName(), pool); err != nil && !isGKEConflict(err) {
			return CapacityObservation{}, fmt.Errorf("creating GKE Spot node pool %s: %w", spotName, err)
		}
		return g.gkePairObservation(stable, selector, desired, CapacityPending, ReasonProvisioning, "costOptimized"), nil
	}
	if reliable == nil {
		pool := g.managedPairPool(reliableName, identity.Name, profile, false, false)
		if _, err := g.client.Create(ctx, g.clusterName(), pool); err != nil && !isGKEConflict(err) {
			return CapacityObservation{}, fmt.Errorf("creating GKE reliable node pool %s: %w", reliableName, err)
		}
		return g.gkePairObservation(stable, selector, desired, CapacityPending, ReasonProvisioning, "costOptimized"), nil
	}
	if err := g.validatePairPool(spot, identity.Name, true); err != nil {
		return g.failedPairObservation(stable, selector, desired, err), nil
	}
	if err := g.validatePairPool(reliable, identity.Name, false); err != nil {
		return g.failedPairObservation(stable, selector, desired, err), nil
	}
	if desired.Availability == DesiredOffline {
		if desired.AttachmentObserved && desired.AttachedNodes == 0 {
			return g.gkePairObservation(stable, selector, desired, CapacityOffline, ReasonStopped, desired.EffectiveAvailabilityClass), nil
		}
		name, pool := spotName, spot
		if desired.EffectiveAvailabilityClass == "reliable" {
			name, pool = reliableName, reliable
		}
		return g.mutatePairSize(ctx, name, pool, false, stable, selector, desired, desired.EffectiveAvailabilityClass)
	}

	effective := desired.EffectiveAvailabilityClass
	if effective == "" {
		effective = "costOptimized"
	}
	if desired.CostOptimizedRetryRequest != "" && desired.CostOptimizedRetryRequest != desired.CostOptimizedRetryObserved {
		if desired.CostOptimizedRetrySince.IsZero() {
			o := g.gkePairObservation(stable, selector, desired, CapacityAvailable, ReasonReady, "reliable")
			o.CostOptimizedRetrySince = g.clockNow()
			return o, nil
		}
		if g.clockNow().Sub(desired.CostOptimizedRetrySince) >= g.threshold() {
			if effective == "costOptimized" {
				return g.mutatePairSize(ctx, spotName, spot, false, stable, selector, desired, "reliable")
			}
			if desired.AttachmentObserved && desired.AttachedNodes > 0 {
				o := g.gkePairObservation(stable, selector, desired, CapacityAvailable, ReasonReady, "reliable")
				o.FallbackReason = "Cost-optimized retry unavailable; reliable fallback retained"
				o.CostOptimizedRetryObserved = desired.CostOptimizedRetryRequest
				o.CostOptimizedRetrySince = time.Time{}
				return o, nil
			}
			return g.mutatePairSize(ctx, reliableName, reliable, true, stable, selector, desired, "reliable")
		}
		if effective == "reliable" {
			if desired.AttachmentObserved && desired.AttachedNodes > 0 {
				return g.mutatePairSize(ctx, reliableName, reliable, false, stable, selector, desired, "reliable")
			}
			return g.mutatePairSize(ctx, spotName, spot, true, stable, selector, desired, "costOptimized")
		}
		if desired.AttachmentObserved && desired.AttachedNodes > 0 {
			o := g.gkePairObservation(stable, selector, desired, CapacityAvailable, ReasonReady, "costOptimized")
			o.CostOptimizedRetryObserved = desired.CostOptimizedRetryRequest
			o.CostOptimizedRetrySince = time.Time{}
			o.FallbackSince, o.CostOptimizedUnavailableSince, o.FallbackReason = time.Time{}, time.Time{}, ""
			return o, nil
		}
		return g.gkePairObservation(stable, selector, desired, CapacityRecovering, ReasonRepairing, "costOptimized"), nil
	}

	if !desired.FallbackSince.IsZero() && effective != "reliable" {
		if desired.AttachmentObserved && desired.AttachedNodes > 0 {
			o := g.gkePairObservation(stable, selector, desired, CapacityRecovering, ReasonRepairing, "costOptimized")
			o.FallbackReason = fmt.Sprintf("Cost-optimized capacity unavailable for %s", g.threshold())
			return o, nil
		}
		return g.mutatePairSize(ctx, reliableName, reliable, true, stable, selector, desired, "reliable")
	}
	if effective == "reliable" {
		o := g.gkePairObservation(stable, selector, desired, CapacityRecovering, ReasonRepairing, "reliable")
		o.FallbackReason = fmt.Sprintf("Cost-optimized capacity unavailable for %s", g.threshold())
		if desired.AttachmentObserved && desired.AttachedNodes > 0 {
			o.State, o.Reason = CapacityAvailable, ReasonReady
			return o, nil
		}
		return g.mutatePairSize(ctx, reliableName, reliable, true, stable, selector, desired, "reliable")
	}
	if desired.AttachmentObserved && desired.AttachedNodes > 0 {
		o := g.gkePairObservation(stable, selector, desired, CapacityAvailable, ReasonReady, "costOptimized")
		o.FallbackSince, o.CostOptimizedUnavailableSince, o.FallbackReason = time.Time{}, time.Time{}, ""
		return o, nil
	}
	unavailable := desired.CostOptimizedUnavailableSince
	if unavailable.IsZero() {
		unavailable = g.clockNow()
	}
	if g.clockNow().Sub(unavailable) < g.threshold() {
		o := g.gkePairObservation(stable, selector, desired, CapacityRecovering, ReasonInterrupted, "costOptimized")
		o.CostOptimizedUnavailableSince = unavailable
		return o, nil
	}
	o, err := g.mutatePairSize(ctx, spotName, spot, false, stable, selector, desired, "costOptimized")
	if err == nil {
		o.CostOptimizedUnavailableSince = unavailable
		o.FallbackSince = g.clockNow()
		o.FallbackReason = fmt.Sprintf("Cost-optimized capacity unavailable for %s", g.threshold())
	}
	return o, err
}

func (g *GKEAdapter) clockNow() time.Time {
	if g.now != nil {
		return g.now()
	}
	return time.Now().UTC()
}

func (g *GKEAdapter) threshold() time.Duration {
	if g.fallbackThreshold > 0 {
		return g.fallbackThreshold
	}
	return 5 * time.Minute
}

func gkePairBase(machine string) string {
	base := strings.ToLower(machine)
	if len(base) > 31 {
		base = base[:31]
	}
	return base
}

func (g *GKEAdapter) getPool(ctx context.Context, name string) (*container.NodePool, error) {
	pool, err := g.client.Get(ctx, g.resourceName(name))
	if isGKENotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("getting GKE node pool %s: %w", name, err)
	}
	return pool, nil
}

func (g *GKEAdapter) managedPairPool(name, machine string, profile GKEProfile, spot, active bool) *container.NodePool {
	d := DesiredMachine{Interruptible: spot}
	pool := g.managedNodePool(name, profile, d)
	pool.Config.Labels[MachineLabelKey] = machine
	pool.Config.ResourceLabels["kyber-machine"] = machine
	if !active {
		pool.InitialNodeCount = 0
		pool.Autoscaling = &container.NodePoolAutoscaling{Enabled: false}
	}
	return pool
}

func (g *GKEAdapter) validatePairPool(pool *container.NodePool, machine string, spot bool) error {
	if !g.owned(pool, machine) {
		return fmt.Errorf("refusing to mutate GKE node pool without Kyber ownership labels")
	}
	if pool.Config.Spot != spot {
		return fmt.Errorf("GKE node pool has incompatible capacity type")
	}
	return nil
}

func (g *GKEAdapter) mutatePairSize(ctx context.Context, name string, pool *container.NodePool, active bool, ref ProviderRef, selector map[string]string, desired DesiredMachine, effective string) (CapacityObservation, error) {
	if g.regional() {
		want := &container.NodePoolAutoscaling{Enabled: false}
		if active {
			want = g.onlineAutoscaling()
		}
		if active || autoscalingEnabled(pool.Autoscaling) {
			if _, err := g.client.SetAutoscaling(ctx, g.resourceName(name), want); err != nil && !isGKEConflict(err) {
				return CapacityObservation{}, fmt.Errorf("setting GKE node pool %s autoscaling: %w", name, err)
			}
			return g.gkePairObservation(ref, selector, desired, CapacityRecovering, ReasonRepairing, effective), nil
		}
	}
	size := int64(0)
	if active {
		size = 1
	}
	if _, err := g.client.SetSize(ctx, g.resourceName(name), size); err != nil && !isGKEConflict(err) {
		return CapacityObservation{}, fmt.Errorf("resizing GKE node pool %s: %w", name, err)
	}
	return g.gkePairObservation(ref, selector, desired, CapacityRecovering, ReasonRepairing, effective), nil
}

func (g *GKEAdapter) gkePairObservation(ref ProviderRef, selector map[string]string, desired DesiredMachine, state AvailabilityState, reason AvailabilityReason, effective string) CapacityObservation {
	o := pairObservation(ref, selector, desired, state, reason, effective)
	o.Location = g.Location
	return o
}

func (g *GKEAdapter) failedPairObservation(ref ProviderRef, selector map[string]string, desired DesiredMachine, err error) CapacityObservation {
	o := g.gkePairObservation(ref, selector, desired, CapacityFailed, ReasonProviderError, desired.EffectiveAvailabilityClass)
	o.Message = err.Error()
	return o
}

func (g *GKEAdapter) deletePair(ctx context.Context, identity MachineIdentity, desired DesiredMachine, ref ProviderRef, selector map[string]string, spotName, reliableName string, spot, reliable *container.NodePool) (CapacityObservation, error) {
	if !desired.AttachmentObserved {
		return g.gkePairObservation(ref, selector, desired, CapacityRecovering, ReasonStopping, desired.EffectiveAvailabilityClass), nil
	}
	if desired.AttachedNodes > 0 {
		name, pool := spotName, spot
		if desired.EffectiveAvailabilityClass == "reliable" {
			name, pool = reliableName, reliable
		}
		if pool == nil && reliable != nil {
			name, pool = reliableName, reliable
		}
		if pool == nil && spot != nil {
			name, pool = spotName, spot
		}
		if pool != nil {
			return g.mutatePairSize(ctx, name, pool, false, ref, selector, desired, desired.EffectiveAvailabilityClass)
		}
		return g.gkePairObservation(ref, selector, desired, CapacityRecovering, ReasonStopping, desired.EffectiveAvailabilityClass), nil
	}
	for i, pool := range []*container.NodePool{spot, reliable} {
		if pool == nil {
			continue
		}
		if err := g.validatePairPool(pool, identity.Name, i == 0); err != nil {
			return g.failedPairObservation(ref, selector, desired, err), nil
		}
		name := []string{spotName, reliableName}[i]
		if _, err := g.client.Delete(ctx, g.resourceName(name)); err != nil && !isGKENotFound(err) && !isGKEConflict(err) {
			return CapacityObservation{}, fmt.Errorf("deleting GKE node pool %s: %w", name, err)
		}
		return g.gkePairObservation(ref, selector, desired, CapacityRecovering, ReasonStopping, desired.EffectiveAvailabilityClass), nil
	}
	return CapacityObservation{State: CapacityAbsent, Reason: ReasonDeleted}, nil
}

func (g *GKEAdapter) clusterName() string {
	return fmt.Sprintf("projects/%s/locations/%s/clusters/%s", g.ProjectID, g.Location, g.Cluster)
}

func (g *GKEAdapter) managedNodePool(name string, profile GKEProfile, desired DesiredMachine) *container.NodePool {
	locations := append([]string(nil), g.nodeLocations...)
	if len(locations) == 0 {
		locations = []string{g.Location}
	}
	autoscaling := &container.NodePoolAutoscaling{Enabled: false}
	initialNodeCount := int64(1)
	if g.regional() {
		autoscaling = g.onlineAutoscaling()
		initialNodeCount = 0
	}
	return &container.NodePool{Name: name, InitialNodeCount: initialNodeCount, Locations: locations,
		Autoscaling: autoscaling,
		Management:  &container.NodeManagement{AutoRepair: true, AutoUpgrade: true},
		Config: &container.NodeConfig{MachineType: profile.MachineType, DiskSizeGb: profile.DiskSizeGB,
			DiskType: profile.DiskType, ImageType: profile.ImageType, Spot: desired.Interruptible,
			Labels:         map[string]string{"kyber.io/managed-by": "kyber", MachineLabelKey: name},
			ResourceLabels: map[string]string{"managed-by": "kyber", "kyber-machine": name}},
	}
}

func regionalAutoscalingOnline(a *container.NodePoolAutoscaling) bool {
	return a != nil && a.Enabled && a.LocationPolicy == "ANY" && a.TotalMinNodeCount == 1 && a.TotalMaxNodeCount == 1
}

func autoscalingEnabled(a *container.NodePoolAutoscaling) bool {
	return a != nil && a.Enabled
}

func (g *GKEAdapter) owned(pool *container.NodePool, machine string) bool {
	return pool != nil && pool.Config != nil && pool.Config.Labels["kyber.io/managed-by"] == "kyber" && pool.Config.Labels[MachineLabelKey] == machine
}

func (g *GKEAdapter) NodeSelector(identity MachineIdentity, ref ProviderRef) map[string]string {
	pool, err := g.resolvePool(identity, ref)
	if err != nil {
		return nil
	}
	return map[string]string{gkeNodePoolLabel: pool}
}

func (g *GKEAdapter) resourceName(pool string) string {
	return fmt.Sprintf("projects/%s/locations/%s/clusters/%s/nodePools/%s", g.ProjectID, g.Location, g.Cluster, pool)
}

func (g *GKEAdapter) providerRef(pool string) ProviderRef {
	return ProviderRef((&url.URL{Scheme: "gke", Host: g.ProjectID, Path: "/" + g.Location + "/" + g.Cluster + "/nodePools/" + pool}).String())
}

func (g *GKEAdapter) resolvePool(identity MachineIdentity, ref ProviderRef) (string, error) {
	if ref == "" {
		if identity.Name == "" {
			return "", fmt.Errorf("resolving GKE node pool: machine name is required")
		}
		return identity.Name, nil
	}
	u, err := url.Parse(string(ref))
	if err != nil || u.Scheme != "gke" || u.Host != g.ProjectID {
		return "", fmt.Errorf("resolving GKE node pool: invalid provider reference")
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) != 4 || parts[0] != g.Location || parts[1] != g.Cluster || parts[2] != "nodePools" || parts[3] == "" {
		return "", fmt.Errorf("resolving GKE node pool: provider reference is outside the configured cluster")
	}
	return parts[3], nil
}

func isGKENotFound(err error) bool {
	var gerr *googleapi.Error
	return errors.As(err, &gerr) && gerr.Code == 404
}

func isGKEConflict(err error) bool {
	var gerr *googleapi.Error
	return errors.As(err, &gerr) && (gerr.Code == 409 || gerr.Code == 429)
}

func (g *GKEAdapter) CreateInstance(context.Context, MachineSpec) (string, error) {
	return "", fmt.Errorf("creating GKE capacity: unsupported in observation-only mode")
}
func (g *GKEAdapter) StartInstance(context.Context, string) error {
	return fmt.Errorf("starting GKE capacity: unsupported in observation-only mode")
}
func (g *GKEAdapter) StopInstance(context.Context, string) error {
	return fmt.Errorf("stopping GKE capacity: unsupported in observation-only mode")
}
func (g *GKEAdapter) DeleteInstance(context.Context, string) error { return nil }
func (g *GKEAdapter) Observe(context.Context, string) (InstanceObservation, error) {
	return InstanceObservation{}, fmt.Errorf("observing GKE capacity through legacy interface is unsupported")
}

var _ ComputeAdapter = (*GKEAdapter)(nil)
var _ CapacityProvider = (*GKEAdapter)(nil)
var _ CapacityNodeSelector = (*GKEAdapter)(nil)
var _ CapacityLocations = (*GKEAdapter)(nil)

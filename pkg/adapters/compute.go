// Package adapters provides cloud provider adapters for the Kyber platform.
// The ComputeAdapter interface abstracts VM lifecycle operations, allowing the
// Machine Controller to work with different cloud providers (GCE, mock).
package adapters

import (
	"context"
	"time"
)

// DesiredAvailability is Kyber's provider-neutral intent for one logical unit
// of Machine capacity. Providers decide how that intent maps to their native
// resources.
type DesiredAvailability string

const (
	DesiredOnline  DesiredAvailability = "Online"
	DesiredOffline DesiredAvailability = "Offline"
	DesiredDeleted DesiredAvailability = "Deleted"
)

// AvailabilityState is Kyber's provider-neutral observation of logical
// Machine capacity. Native resource kinds and status strings must not cross
// the provider boundary.
type AvailabilityState string

const (
	CapacityPending    AvailabilityState = "Pending"
	CapacityAvailable  AvailabilityState = "Available"
	CapacityRecovering AvailabilityState = "Recovering"
	CapacityOffline    AvailabilityState = "Offline"
	CapacityAbsent     AvailabilityState = "Absent"
	CapacityFailed     AvailabilityState = "Failed"
	CapacityUnknown    AvailabilityState = "Unknown"
)

// AvailabilityReason gives controllers and operators a portable explanation
// for the current availability state.
type AvailabilityReason string

const (
	ReasonReady         AvailabilityReason = "Ready"
	ReasonProvisioning  AvailabilityReason = "Provisioning"
	ReasonNodeJoining   AvailabilityReason = "NodeJoining"
	ReasonInterrupted   AvailabilityReason = "Interrupted"
	ReasonRepairing     AvailabilityReason = "Repairing"
	ReasonStopping      AvailabilityReason = "Stopping"
	ReasonStopped       AvailabilityReason = "Stopped"
	ReasonDeleted       AvailabilityReason = "Deleted"
	ReasonExternalWait  AvailabilityReason = "ExternalWait"
	ReasonProviderError AvailabilityReason = "ProviderError"
	ReasonUnknown       AvailabilityReason = "Unknown"
)

// SuspendMode describes what Offline intent means for one provider.
type SuspendMode string

const (
	SuspendCapacity    SuspendMode = "Capacity"
	SuspendLogicalOnly SuspendMode = "LogicalOnly"
	SuspendUnsupported SuspendMode = "Unsupported"
)

// DeletionMode describes whether deleting a Machine also deletes backing
// capacity or only unregisters external capacity from Kyber.
type DeletionMode string

const (
	DeleteCapacity DeletionMode = "DeleteCapacity"
	UnregisterOnly DeletionMode = "UnregisterOnly"
)

// ReliableFallbackMode describes whether a provider can replace unavailable
// cost-optimized capacity with reliable capacity. The terms are deliberately
// provider-neutral; native purchasing models stay behind the adapter.
type ReliableFallbackMode string

const (
	ReliableFallbackUnsupported ReliableFallbackMode = "Unsupported"
	ReliableFallbackManual      ReliableFallbackMode = "Manual"
	ReliableFallbackAutomatic   ReliableFallbackMode = "Automatic"
)

// ProviderRef is an opaque provider-owned identifier. Code outside the
// provider that produced it may persist and return it but must not parse it.
type ProviderRef string

// Capabilities describes portable operator actions. It intentionally omits
// provider resource kinds such as VM, node pool, or managed instance group.
type Capabilities struct {
	CanProvision            bool
	CanDiscoverExisting     bool
	SuspendMode             SuspendMode
	DeletionMode            DeletionMode
	SupportsReliable        bool
	SupportsInterruptible   bool
	SupportsLocations       bool
	RequiresSchedulerDemand bool
	ReliableFallbackMode    ReliableFallbackMode
}

// Profile is an installer-curated capacity promise exposed to operators. The
// provider-specific realization stays inside provider configuration.
type Profile struct {
	ID                  string
	DisplayName         string
	Description         string
	CPU                 string
	Memory              string
	AvailabilityClasses []string
	Recommended         bool
}

// MachineIdentity is the stable Kyber identity passed to a provider.
type MachineIdentity struct {
	Name string
}

// NodeBootstrap contains provider-neutral inputs needed for newly created
// capacity to join the Kubernetes cluster. Providers decide how to deliver
// these values (for example instance metadata or a node-pool bootstrap path).
// The values are runtime credentials and must never appear in provider refs,
// observations, logs, or API responses.
type NodeBootstrap struct {
	ServerURL string
	JoinToken string
}

// DesiredMachine is the provider-neutral resolved intent for one Machine.
// Profile is a stable installer-defined ID, not necessarily a cloud SKU.
type DesiredMachine struct {
	Availability  DesiredAvailability
	Profile       string
	DiskSizeGb    int
	Interruptible bool
	// AvailabilityClass is the provider-neutral requested class. Interruptible
	// remains populated during the compatibility period for existing providers.
	AvailabilityClass string
	// CostOptimizedRetryRequest is an opaque, durable one-shot request token.
	// Providers observe and acknowledge it without interpreting its contents.
	CostOptimizedRetryRequest string
	Location                  string
	Labels                    map[string]string
	NodeBootstrap             NodeBootstrap
	Managed                   bool
	// AttachmentObserved distinguishes an authoritative zero Nodes from an
	// unavailable Kubernetes observation.
	AttachmentObserved bool
	AttachedNodes      int
}

// CapacityObservation is the portable result of one provider reconciliation
// step. Reconcile calls are bounded and idempotent; controllers requeue until
// the observation converges to Available, Offline, Absent, or Failed.
type CapacityObservation struct {
	State        AvailabilityState
	Reason       AvailabilityReason
	Message      string
	ProviderRef  ProviderRef
	Location     string
	NodeSelector map[string]string
	ExternalIP   string
	InternalIP   string
	CreatedAt    time.Time
	// EffectiveAvailabilityClass reports the class currently serving the
	// Machine when it differs from, or confirms, requested intent.
	EffectiveAvailabilityClass    string
	FallbackReason                string
	FallbackSince                 time.Time
	CostOptimizedUnavailableSince time.Time
	CostOptimizedRetryObserved    string
}

// CapacityProvider reconciles one logical unit of Machine capacity. The
// provider owns native provisioning, replacement, repair, and deletion
// mechanics; controllers deal only in desired availability and observations.
type CapacityProvider interface {
	Type() string
	Capabilities(context.Context) (Capabilities, error)
	Profiles(context.Context) ([]Profile, error)
	Validate(context.Context, DesiredMachine) error
	Reconcile(context.Context, MachineIdentity, DesiredMachine, ProviderRef) (CapacityObservation, error)
}

// CapacityNodeSelector is an optional provider extension for capacity whose
// Kubernetes Node identity is expressed by provider-owned labels rather than
// Kyber's direct-VM machine label.
type CapacityNodeSelector interface {
	NodeSelector(MachineIdentity, ProviderRef) map[string]string
}

// CapacityNeedsSchedulerDemand is an optional provider extension for managed
// capacity that relies on an unschedulable Pod to trigger provider autoscaling.
// The Machine controller supplies that demand while Agents are deliberately
// parked without pods during capacity recovery.
type CapacityNeedsSchedulerDemand interface {
	NeedsSchedulerDemand() bool
}

type CapacityLocations interface {
	Locations(context.Context) ([]string, error)
}

// ComputeAdapter abstracts cloud VM operations.
// V1 implements GCE. Future: AWS EC2, Azure VMs.
type ComputeAdapter interface {
	// Type returns the provider identifier implemented by this adapter.
	Type() string
	// NodeAttachment reports whether provider-created instances register their
	// own Kubernetes nodes or use a Ready node supplied by the local cluster.
	NodeAttachment() NodeAttachmentMode
	// CreateInstance provisions a new VM with the given spec. Returns the provider-assigned instance ID.
	CreateInstance(ctx context.Context, spec MachineSpec) (instanceID string, err error)
	// StartInstance starts a stopped instance.
	StartInstance(ctx context.Context, instanceID string) error
	// StopInstance stops a running instance.
	StopInstance(ctx context.Context, instanceID string) error
	// DeleteInstance permanently deletes an instance.
	DeleteInstance(ctx context.Context, instanceID string) error
	// Observe returns the provider-neutral observed state of an instance.
	// Provider-native state strings must be translated at the adapter boundary.
	Observe(ctx context.Context, instanceID string) (InstanceObservation, error)
}

// SimulationController is an optional development-only control surface
// implemented by deterministic provider simulators. Production adapters must
// not implement it.
type SimulationController interface {
	ListSimulatedInstances() []SimulatedInstance
	ApplySimulationScenario(machineName string, scenario SimulationScenario) error
}

type SimulationScenario string

const (
	SimulationPending   SimulationScenario = "pending"
	SimulationRunning   SimulationScenario = "running"
	SimulationStopped   SimulationScenario = "stopped"
	SimulationPreempted SimulationScenario = "preempted"
	SimulationFailed    SimulationScenario = "failed"
	// SimulationCostOptimizedUnavailable models a completed same-location
	// fallback to reliable capacity while retaining the provider reference.
	SimulationCostOptimizedUnavailable SimulationScenario = "cost-optimized-unavailable"
	// SimulationFailNextCostOptimizedRetry keeps reliable fallback active while
	// acknowledging the next retry request, modelling rollback after Spot stays
	// unavailable.
	SimulationFailNextCostOptimizedRetry SimulationScenario = "fail-next-cost-optimized-retry"
	SimulationFailNextCreate             SimulationScenario = "fail-next-create"
	SimulationFailNextStart              SimulationScenario = "fail-next-start"
	SimulationFailNextStop               SimulationScenario = "fail-next-stop"
	SimulationFailNextDelete             SimulationScenario = "fail-next-delete"
	SimulationFailNextObserve            SimulationScenario = "fail-next-observe"
)

type SimulatedInstance struct {
	MachineName string              `json:"machineName"`
	ProviderID  string              `json:"providerId"`
	Spec        MachineSpec         `json:"spec"`
	Observation InstanceObservation `json:"observation"`
}

type NodeAttachmentMode string

const (
	NodeAttachmentManaged  NodeAttachmentMode = "Managed"
	NodeAttachmentExisting NodeAttachmentMode = "Existing"
)

// InstanceState is Kyber's provider-neutral view of a cloud VM lifecycle.
type InstanceState string

const (
	InstanceStatePending InstanceState = "Pending"
	InstanceStateRunning InstanceState = "Running"
	InstanceStateStopped InstanceState = "Stopped"
	InstanceStateFailed  InstanceState = "Failed"
	InstanceStateUnknown InstanceState = "Unknown"
)

// InterruptionState records whether a provider initiated the instance stop.
type InterruptionState string

const (
	InterruptionNone      InterruptionState = "None"
	InterruptionPreempted InterruptionState = "Preempted"
)

// InstanceObservation holds the provider-neutral observed state of a VM.
type InstanceObservation struct {
	State        InstanceState
	Interruption InterruptionState
	// Location is the provider location containing the instance (for example,
	// a GCE zone). Controllers treat it as an opaque display value.
	Location string
	// ExternalIP is the external IP address of the instance, if assigned.
	ExternalIP string
	// InternalIP is the internal IP address of the instance within the VPC.
	InternalIP string
	// CreatedAt is the time the instance was created.
	CreatedAt time.Time
}

// CapacityObservationFromInstance maps the legacy VM-shaped observation into
// the provider-neutral capacity vocabulary. It is the compatibility seam used
// while existing providers move to CapacityProvider; it does not decide how a
// provider repairs or replaces interrupted capacity.
func CapacityObservationFromInstance(ref ProviderRef, nodeSelector map[string]string, observation InstanceObservation) CapacityObservation {
	result := CapacityObservation{
		ProviderRef:  ref,
		Location:     observation.Location,
		NodeSelector: copyStringMap(nodeSelector),
		ExternalIP:   observation.ExternalIP,
		InternalIP:   observation.InternalIP,
		CreatedAt:    observation.CreatedAt,
	}

	if observation.Interruption == InterruptionPreempted {
		result.State = CapacityRecovering
		result.Reason = ReasonInterrupted
		return result
	}

	switch observation.State {
	case InstanceStatePending:
		result.State = CapacityPending
		result.Reason = ReasonProvisioning
	case InstanceStateRunning:
		result.State = CapacityAvailable
		result.Reason = ReasonReady
	case InstanceStateStopped:
		result.State = CapacityOffline
		result.Reason = ReasonStopped
	case InstanceStateFailed:
		result.State = CapacityFailed
		result.Reason = ReasonProviderError
	default:
		result.State = CapacityUnknown
		result.Reason = ReasonUnknown
	}
	return result
}

func copyStringMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

// MachineLabelKey is the k8s node label applied by the VM startup script to identify
// which Machine CRD corresponds to a given k8s node. Must match the value in the
// machine controller package (controllers/machine.MachineLabelKey).
const MachineLabelKey = "kyber.io/machine"

// MachineSpec describes the desired VM to provision.
// This is the compute-layer representation; the controller converts
// the Machine CRD spec into a MachineSpec before calling CreateInstance.
type MachineSpec struct {
	// Name is the logical machine name used for provider resource identity.
	Name string `json:"name"`
	// Profile is the provider-specific compute profile selected from its catalog.
	Profile string `json:"profile"`
	// DiskSizeGb is the boot disk size in gigabytes.
	DiskSizeGb int `json:"diskSizeGb"`
	// Interruptible permits the provider to reclaim the instance.
	Interruptible bool `json:"interruptible"`
	// Location is an opaque provider location identifier.
	Location string `json:"location"`
	// JoinToken is the k3s agent join token, passed as instance metadata for the startup script.
	JoinToken string `json:"-"`
	// ServerURL is the k3s server URL, passed as instance metadata for the startup script.
	ServerURL string `json:"-"`
	// Labels are additional cloud provider labels to apply to the instance.
	Labels map[string]string `json:"labels,omitempty"`
}

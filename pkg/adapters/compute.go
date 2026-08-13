// Package adapters provides cloud provider adapters for the Kyber platform.
// The ComputeAdapter interface abstracts VM lifecycle operations, allowing the
// Machine Controller to work with different cloud providers (GCE, mock).
package adapters

import (
	"context"
	"time"
)

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
	SimulationPending         SimulationScenario = "pending"
	SimulationRunning         SimulationScenario = "running"
	SimulationStopped         SimulationScenario = "stopped"
	SimulationPreempted       SimulationScenario = "preempted"
	SimulationFailed          SimulationScenario = "failed"
	SimulationFailNextCreate  SimulationScenario = "fail-next-create"
	SimulationFailNextStart   SimulationScenario = "fail-next-start"
	SimulationFailNextStop    SimulationScenario = "fail-next-stop"
	SimulationFailNextDelete  SimulationScenario = "fail-next-delete"
	SimulationFailNextObserve SimulationScenario = "fail-next-observe"
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

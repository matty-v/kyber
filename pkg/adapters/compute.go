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
	// Name is the logical machine name (used as a label and for naming the GCE instance).
	Name string
	// MachineType is the cloud provider machine type (e.g., "n2-standard-4").
	MachineType string
	// DiskSizeGb is the boot disk size in gigabytes.
	DiskSizeGb int
	// Spot indicates whether to use spot/preemptible pricing.
	Spot bool
	// Zone is the zone to create the instance in (e.g., "us-central1-a").
	Zone string
	// JoinToken is the k3s agent join token, passed as instance metadata for the startup script.
	JoinToken string
	// ServerURL is the k3s server URL, passed as instance metadata for the startup script.
	ServerURL string
	// Labels are additional cloud provider labels to apply to the instance.
	Labels map[string]string
}

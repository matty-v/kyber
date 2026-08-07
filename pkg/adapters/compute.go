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
	// CreateInstance provisions a new VM with the given spec. Returns the provider-assigned instance ID.
	CreateInstance(ctx context.Context, spec MachineSpec) (instanceID string, err error)
	// StartInstance starts a stopped instance.
	StartInstance(ctx context.Context, instanceID string) error
	// StopInstance stops a running instance.
	StopInstance(ctx context.Context, instanceID string) error
	// DeleteInstance permanently deletes an instance.
	DeleteInstance(ctx context.Context, instanceID string) error
	// GetInstanceStatus returns the current status of an instance.
	GetInstanceStatus(ctx context.Context, instanceID string) (InstanceStatus, error)
	// IsPreempted returns true if the instance was preempted by the cloud provider
	// (i.e., it is a spot/preemptible instance that was forcibly terminated).
	IsPreempted(ctx context.Context, instanceID string) (bool, error)
}

// InstanceStatus holds the observed state of a cloud VM instance.
type InstanceStatus struct {
	// Status is the cloud provider status string (e.g., RUNNING, STOPPED, TERMINATED, STAGING).
	Status string
	// Zone is the zone where the instance is located.
	Zone string
	// ExternalIP is the external IP address of the instance, if assigned.
	ExternalIP string
	// InternalIP is the internal IP address of the instance within the VPC.
	InternalIP string
	// CreatedAt is the time the instance was created.
	CreatedAt time.Time
}

// GCE instance status strings.
const (
	InstanceStatusRunning    = "RUNNING"
	InstanceStatusStopped    = "STOPPED"
	InstanceStatusTerminated = "TERMINATED"
	InstanceStatusStaging    = "STAGING"
	InstanceStatusProvisioning = "PROVISIONING"
)

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

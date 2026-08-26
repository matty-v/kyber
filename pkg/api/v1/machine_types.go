package v1

import (
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// MachineCapacity is the declared resource budget the operator allocates to
// this Machine. Populated from Spec.MachineType on gce (via machinecaps lookup)
// or supplied directly by the user on mock.
type MachineCapacity struct {
	// CPU is the total CPU budget (e.g., "4", "500m").
	CPU resource.Quantity `json:"cpu"`
	// Memory is the total memory budget (e.g., "16Gi", "512Mi").
	Memory resource.Quantity `json:"memory"`
	// EphemeralStorage is the total ephemeral-storage budget for agent disk
	// requests (e.g., "200Gi"). Sourced from node.status.allocatable on
	// observed/assignable; the difference between assignable and the sum of
	// agent.spec.resources.disk on this machine is published as available.
	// Optional for backward compat — pre-#129 PR-C resources omitted it
	// entirely; reading code MUST treat the zero value as "unknown."
	// +optional
	EphemeralStorage resource.Quantity `json:"ephemeralStorage,omitempty"`
}

// MachinePhase represents the current lifecycle phase of a Machine.
type MachinePhase string

const (
	// MachinePhaseProvisioning means the GCE instance is being created and the k8s node is not yet Ready.
	MachinePhaseProvisioning MachinePhase = "Provisioning"
	// MachinePhaseReady means the VM is running, the k8s node is Ready, and no agents are scheduled yet.
	MachinePhaseReady MachinePhase = "Ready"
	// MachinePhaseRunning means the VM is running and has at least one agent pod scheduled on it.
	MachinePhaseRunning MachinePhase = "Running"
	// MachinePhaseStopping means agents are being drained and the VM is shutting down.
	MachinePhaseStopping MachinePhase = "Stopping"
	// MachinePhaseStopped means the VM is stopped and the k8s node is absent.
	MachinePhaseStopped MachinePhase = "Stopped"
	// MachinePhasePreempted means a spot VM was preempted by GCE and a replacement is being provisioned.
	MachinePhasePreempted MachinePhase = "Preempted"
	// MachinePhaseReplacing means a replacement VM is being provisioned after preemption.
	MachinePhaseReplacing MachinePhase = "Replacing"
	// MachinePhaseFailed means the machine entered an unrecoverable error state.
	MachinePhaseFailed MachinePhase = "Failed"
	// MachinePhaseDeleted means the machine has been fully deleted including the GCE instance.
	MachinePhaseDeleted MachinePhase = "Deleted"
)

// MachineProvider identifies the cloud provider for this machine.
// +kubebuilder:validation:Enum=gce;gke;eks;static;fake;mock
type MachineProvider string

const (
	// MachineProviderGCE is Google Compute Engine.
	MachineProviderGCE MachineProvider = "gce"
	// MachineProviderGKE manages or observes capacity through GKE node pools.
	MachineProviderGKE MachineProvider = "gke"
	// MachineProviderEKS manages capacity through EKS managed node groups.
	MachineProviderEKS MachineProvider = "eks"
	// MachineProviderStatic attaches to a Kubernetes node provisioned outside
	// Kyber and does not manage an external VM lifecycle.
	MachineProviderStatic MachineProvider = "static"
	// MachineProviderFake runs the managed Machine lifecycle against the local,
	// deterministic fake compute provider.
	MachineProviderFake MachineProvider = "fake"
	// MachineProviderMock represents the in-process / in-cluster mock adapter.
	// Deprecated compatibility alias for MachineProviderStatic.
	MachineProviderMock MachineProvider = "mock"
)

// MachineAvailabilityClass describes the interruption contract requested by
// an operator without exposing a provider-specific purchasing model.
// +kubebuilder:validation:Enum=reliable;costOptimized
type MachineAvailabilityClass string

const (
	MachineAvailabilityReliable      MachineAvailabilityClass = "reliable"
	MachineAvailabilityCostOptimized MachineAvailabilityClass = "costOptimized"
)

// ReliableFallbackMode describes the provider's portable fallback behavior.
// +kubebuilder:validation:Enum=Unsupported;Manual;Automatic
type ReliableFallbackMode string

const (
	ReliableFallbackUnsupported ReliableFallbackMode = "Unsupported"
	ReliableFallbackManual      ReliableFallbackMode = "Manual"
	ReliableFallbackAutomatic   ReliableFallbackMode = "Automatic"
)

// MachineManagementMode distinguishes Kyber-managed capacity from capacity
// registered by an installer and managed outside Kyber.
// +kubebuilder:validation:Enum=Managed;External
type MachineManagementMode string

const (
	MachineManagementManaged  MachineManagementMode = "Managed"
	MachineManagementExternal MachineManagementMode = "External"
)

// MachineAvailability is the provider-neutral observed capacity state.
// +kubebuilder:validation:Enum=Pending;Available;Recovering;Offline;Absent;Failed;Unknown
type MachineAvailability string

// ResolvedMachineProfile snapshots the operator-facing profile properties
// used when this Machine was created. Installer mapping changes therefore do
// not silently change the declared capacity of an existing Machine.
type ResolvedMachineProfile struct {
	ID          string          `json:"id"`
	DisplayName string          `json:"displayName,omitempty"`
	Description string          `json:"description,omitempty"`
	Capacity    MachineCapacity `json:"capacity"`
}

// MachineSpec defines the desired state of a Machine.
type MachineSpec struct {
	// Provider is the compute provider for this machine. "gce" provisions real
	// cloud VMs; "gke" and "eks" manage cloud Kubernetes capacity; "fake"
	// simulates that lifecycle locally; "static" attaches to an existing
	// Kubernetes node; "mock" is a deprecated compatibility alias.
	Provider MachineProvider `json:"provider"`

	// Capacity is the declared resource budget for this Machine. Optional at
	// the CRD schema level for backward-compat; required at the REST API
	// admission layer. Populated from Spec.MachineType for gce/fake; user-supplied
	// for static/mock.
	// +optional
	Capacity MachineCapacity `json:"capacity,omitempty"`

	// Profile is the stable installer-defined compute profile. It is preferred
	// over the deprecated provider-native MachineType field.
	// +optional
	Profile string `json:"profile,omitempty"`

	// AvailabilityClass requests reliable or cost-optimized interruptible
	// capacity without naming a provider-specific purchasing model.
	// +optional
	AvailabilityClass MachineAvailabilityClass `json:"availabilityClass,omitempty"`

	// Location is an opaque provider location identifier. It is preferred over
	// the deprecated Zone field.
	// +optional
	Location string `json:"location,omitempty"`

	// ManagementMode records whether Kyber owns the backing capacity lifecycle
	// or only registers externally managed capacity.
	// +optional
	ManagementMode MachineManagementMode `json:"managementMode,omitempty"`

	// MachineType is the provider machine type (e.g., "n2-standard-4" for GCE).
	// Maps to the vCPU and memory configuration for the VM. Required for
	// gce/fake and absent for static/mock.
	// +optional
	MachineType string `json:"machineType,omitempty"`

	// DiskSizeGb is the boot disk size in gigabytes. Required for gce/fake;
	// absent for static/mock.
	// +optional
	// +kubebuilder:validation:Minimum=10
	DiskSizeGb int32 `json:"diskSizeGb,omitempty"`

	// Spot indicates whether the VM should use spot (preemptible) pricing.
	// Spot VMs are significantly cheaper but may be preempted at any time.
	// Rejected for static/mock.
	// +optional
	Spot bool `json:"spot,omitempty"`

	// Zone is the provider location where the VM is created (e.g., "us-central1-a").
	// Zone-local PVs require replacement VMs to use the same zone. Required for
	// gce/fake and absent for static/mock.
	// +optional
	Zone string `json:"zone,omitempty"`

	// DesiredPhase is written by the API module to signal a lifecycle intent.
	// Valid values: Running, Stopped.
	// +kubebuilder:validation:Enum=Running;Stopped
	// +optional
	DesiredPhase MachinePhase `json:"desiredPhase,omitempty"`

	// CostOptimizedRetryRequest is an opaque idempotency token written by the
	// retry-cost-optimized API action. Providers acknowledge it in status.
	// +optional
	// +kubebuilder:validation:MaxLength=128
	CostOptimizedRetryRequest string `json:"costOptimizedRetryRequest,omitempty"`
}

// MachineStatus defines the observed state of a Machine.
type MachineStatus struct {
	// Phase is the current lifecycle phase of the machine.
	Phase MachinePhase `json:"phase,omitempty"`

	// InstanceId is the cloud provider instance ID (e.g., GCE instance ID).
	// +optional
	InstanceId string `json:"instanceId,omitempty"`

	// ProviderRef is an opaque, provider-owned reference to backing capacity or
	// an in-flight provider operation. Consumers must not parse it.
	// +optional
	ProviderRef string `json:"providerRef,omitempty"`

	// Availability is the provider-neutral observed capacity state.
	// +optional
	Availability MachineAvailability `json:"availability,omitempty"`

	// EffectiveAvailabilityClass is the class currently serving the Machine.
	// It may differ from spec.availabilityClass during reliable fallback.
	// +optional
	EffectiveAvailabilityClass MachineAvailabilityClass `json:"effectiveAvailabilityClass,omitempty"`

	// FallbackReason is a provider-neutral explanation of the active or most
	// recent fallback transition.
	// +optional
	FallbackReason string `json:"fallbackReason,omitempty"`

	// FallbackSince is when reliable fallback began.
	// +optional
	FallbackSince *metav1.Time `json:"fallbackSince,omitempty"`

	// CostOptimizedUnavailableSince is when requested cost-optimized capacity
	// first became unavailable. It persists across controller restarts.
	// +optional
	CostOptimizedUnavailableSince *metav1.Time `json:"costOptimizedUnavailableSince,omitempty"`

	// CostOptimizedRetryObserved acknowledges the latest retry request token
	// processed by the provider/controller state machine.
	// +optional
	CostOptimizedRetryObserved string `json:"costOptimizedRetryObserved,omitempty"`

	// CostOptimizedRetrySince is when the current manual retry attempt began.
	// It persists across control-plane restarts and clears on success/rollback.
	// +optional
	CostOptimizedRetrySince *metav1.Time `json:"costOptimizedRetrySince,omitempty"`

	// ResolvedProfile snapshots the operator-facing profile used at creation.
	// +optional
	ResolvedProfile *ResolvedMachineProfile `json:"resolvedProfile,omitempty"`

	// ExternalIP is the external IP address of the VM, if assigned.
	// +optional
	ExternalIP string `json:"externalIP,omitempty"`

	// InternalIP is the internal IP address of the VM within the VPC.
	// +optional
	InternalIP string `json:"internalIP,omitempty"`

	// NodeName is the name of the k8s node corresponding to this machine.
	// Set once the VM boots and the k3s agent joins the cluster.
	// +optional
	NodeName string `json:"nodeName,omitempty"`

	// AgentCount is the number of agent pods currently scheduled on this machine's node.
	// Informational; used for the fleet dashboard and Ready vs Running phase transitions.
	// +optional
	AgentCount int32 `json:"agentCount,omitempty"`

	// LastHealthCheck is the time of the most recent successful health check.
	// +optional
	LastHealthCheck *metav1.Time `json:"lastHealthCheck,omitempty"`

	// LastTransition is the time of the most recent phase transition.
	// +optional
	LastTransition *metav1.Time `json:"lastTransition,omitempty"`

	// ReplacementCount is the number of times this machine has been replaced (spot preemption replacements).
	// +optional
	ReplacementCount int32 `json:"replacementCount,omitempty"`

	// Message is a human-readable description of the current state or last error.
	// +optional
	Message string `json:"message,omitempty"`

	// Conditions holds standard k8s conditions for this Machine.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ObservedCapacity mirrors the backing Node's status.allocatable.
	// Populated once the Machine has a backing node (NodeName is set).
	// This is the upper bound of what could be allocated to agents before
	// platform overhead is reserved.
	// +optional
	ObservedCapacity *MachineCapacity `json:"observedCapacity,omitempty"`

	// AssignableCapacity is ObservedCapacity minus the chart-configured
	// platform reservation (controlPlane.platformReservation.{cpu,memory,ephemeralStorage}).
	// This is the budget against which agent resource requests are summed.
	// +optional
	AssignableCapacity *MachineCapacity `json:"assignableCapacity,omitempty"`

	// AvailableCapacity is AssignableCapacity minus the sum of all agents'
	// spec.resources requests on this machine. This is the remaining budget
	// a new agent can claim — the API server's /create-agent 409 check
	// reads this directly.
	// +optional
	AvailableCapacity *MachineCapacity `json:"availableCapacity,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=mc
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Provider",type=string,JSONPath=`.spec.provider`
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.machineType`
// +kubebuilder:printcolumn:name="Zone",type=string,JSONPath=`.spec.zone`
// +kubebuilder:printcolumn:name="Spot",type=boolean,JSONPath=`.spec.spot`
// +kubebuilder:printcolumn:name="Agents",type=integer,JSONPath=`.status.agentCount`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Machine is the schema for the machines API.
// A Machine CRD represents a cloud VM managed by the Kyber platform, hosting agent runtime pods.
type Machine struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   MachineSpec   `json:"spec"`
	Status MachineStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// MachineList contains a list of Machine.
type MachineList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Machine `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Machine{}, &MachineList{})
}

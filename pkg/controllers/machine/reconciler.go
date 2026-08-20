package machine

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/matty-v/kyber/pkg/adapters"
	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/telemetry"
)

// MachineFinalizer is the finalizer name used to gate cleanup of machine resources.
// The spec (2026-04-10-machine-controller-design.md) uses the placeholder domain
// "platform.example.com/machine-cleanup"; we use "kyber.io" to match the CRD group
// and label prefix. This is an intentional deviation from the spec's placeholder text,
// consistent with the AgentFinalizer convention.
const MachineFinalizer = "kyber.io/machine-cleanup"

// MachineLabelKey is the k8s node label applied by the VM startup script to identify
// which Machine CRD corresponds to a given k8s node.
const MachineLabelKey = "kyber.io/machine"

// AgentLabelKey is the label used to identify agent pods on a machine's node.
const AgentLabelKey = "platform.example.com/agent"

const (
	// requeueProvisioning is how long to wait between checks during Provisioning.
	requeueProvisioning = 30 * time.Second
	// requeueStopping is how long to wait between checks during Stopping.
	requeueStopping = 15 * time.Second
	// requeueReplacing is how long to wait between checks during Replacing.
	requeueReplacing = 30 * time.Second
	// requeueImmediate is used after transitions that should quickly lead to another.
	requeueImmediate = 2 * time.Second
	// replacementRetryDelay is how long to wait between replacement retry attempts.
	replacementRetryDelay = 30 * time.Second
)

// MachineReconciler reconciles Machine CRDs.
//
// +kubebuilder:rbac:groups=kyber.io,resources=machines,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kyber.io,resources=machines/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kyber.io,resources=machines/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
type MachineReconciler struct {
	client.Client
	Scheme         *runtime.Scheme
	Recorder       record.EventRecorder
	ComputeAdapter adapters.ComputeAdapter
	// CapacityProvider is the declarative provider contract used during the
	// provider-neutral migration. When nil, capacityProvider lazily adopts a
	// ComputeAdapter that also implements the new contract. Direct GCE remains
	// on ComputeAdapter until its provider migration is complete.
	CapacityProvider adapters.CapacityProvider
	// AllowSimulatedNodes accepts explicitly labelled synthetic Nodes without
	// kubelet heartbeats. It is enabled only by the local GCE emulator profile.
	AllowSimulatedNodes bool

	// AlertSink receives alerts for notable machine events (e.g., Preempted, Failed).
	// If nil, alerts are silently dropped.
	AlertSink telemetry.AlertSink

	// K3sJoinToken is the k3s cluster join token passed to newly provisioned node VMs.
	// Read from KYBER_K3S_JOIN_TOKEN env var at startup (set by Helm from the api-credentials
	// Secret). Empty string is valid — the mock adapter ignores it; the GCE adapter logs a
	// warning and the provisioned node will fail to join the cluster.
	K3sJoinToken string

	// K3sServerURL is the k3s API server URL (https://<host>:6443) passed to new node VMs.
	// Read from KYBER_K3S_SERVER_URL env var at startup. Same empty-string semantics as
	// K3sJoinToken above.
	K3sServerURL string

	// AgentCountFn, if non-nil, overrides countAgentPods. Used in tests to avoid
	// the field indexer dependency (countAgentPods uses MatchingFields which requires
	// an IndexField call at manager setup time, which envtest tests don't register).
	// Production always leaves this nil.
	AgentCountFn func(ctx context.Context, machine *kyberv1.Machine) (int, error)

	// Namespace is the k8s namespace in which agents and machines are managed.
	// Read from KYBER_NAMESPACE env var at startup. Used by HandlePreemptionNotice
	// to scope the agent list query.
	Namespace string

	// PlatformReservation is the per-machine carve-out for kube-system / kyber-system /
	// k3s overhead. ObservedCapacity − PlatformReservation = AssignableCapacity. Sourced
	// from KYBER_PLATFORM_RESERVATION_{CPU,MEMORY} env vars in cmd/control-plane/main.go;
	// chart values default to 1 CPU / 1Gi.
	PlatformReservation kyberv1.MachineCapacity
}

// NewMachineReconciler constructs a MachineReconciler and reads k3s credentials from
// environment variables. Call this instead of constructing MachineReconciler directly
// in production code.
func NewMachineReconciler(c client.Client, scheme *runtime.Scheme, recorder record.EventRecorder, adapter adapters.ComputeAdapter) *MachineReconciler {
	setupLog := log.Log.WithName("machine-reconciler")

	joinToken := os.Getenv("KYBER_K3S_JOIN_TOKEN")
	serverURL := os.Getenv("KYBER_K3S_SERVER_URL")

	if joinToken == "" {
		setupLog.Info("warning: KYBER_K3S_JOIN_TOKEN not set — provisioned nodes will not join the cluster")
	}
	if serverURL == "" {
		setupLog.Info("warning: KYBER_K3S_SERVER_URL not set — provisioned nodes will not join the cluster")
	}

	r := &MachineReconciler{
		Client:              c,
		Scheme:              scheme,
		Recorder:            recorder,
		ComputeAdapter:      adapter,
		K3sJoinToken:        joinToken,
		K3sServerURL:        serverURL,
		AllowSimulatedNodes: os.Getenv("KYBER_ALLOW_SIMULATED_NODES") == "true",
	}
	if provider, ok := adapter.(adapters.CapacityProvider); ok {
		r.CapacityProvider = provider
	}
	return r
}

// Reconcile is the main reconciliation function called by controller-runtime.
func (r *MachineReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// 1. Fetch the Machine CRD.
	machine := &kyberv1.Machine{}
	if err := r.Get(ctx, req.NamespacedName, machine); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("fetching machine: %w", err)
	}

	// Record reconcile latency. Capture phase before any transition.
	reconcileStart := time.Now()
	phaseAtStart := string(machine.Status.Phase)
	defer func() {
		if telemetry.MachineReconcileLatency != nil {
			telemetry.MachineReconcileLatency.Record(ctx,
				time.Since(reconcileStart).Seconds(),
				metric.WithAttributes(attribute.String("phase", phaseAtStart)))
		}
	}()

	// 2. Handle deletion: run finalizer if the CRD is being deleted.
	if !machine.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, machine)
	}

	// 2a. Short-circuit for existing-node providers: standalone/static path.
	if machine.Spec.Provider == kyberv1.MachineProviderMock ||
		machine.Spec.Provider == kyberv1.MachineProviderStatic {
		return r.reconcileStatic(ctx, machine)
	}

	// 3. Ensure finalizer is registered.
	if err := r.ensureFinalizer(ctx, machine); err != nil {
		return ctrl.Result{}, err
	}

	// 4. Classify the event to drive the state machine.
	event, requeueAfter, err := r.classifyEvent(ctx, machine)
	if err != nil {
		return ctrl.Result{}, err
	}
	if event == "" {
		// Stable state — refresh node-derived status fields anyway and requeue.
		// Without this, a Machine that's been stable in Ready/Running for a while
		// shows nil observedCapacity / assignableCapacity / availableCapacity
		// forever — the operator UI then renders zeros for capacity even though
		// the node has plenty (kyber#240). The state-machine action path only
		// fires updateNodeInfo on transitions; periodic refresh has to live here.
		// Best-effort: log + continue if either helper errors.
		if machine.Status.Phase == kyberv1.MachinePhaseReady ||
			machine.Status.Phase == kyberv1.MachinePhaseRunning {
			if err := r.updateNodeInfo(ctx, machine); err != nil {
				logger.Info("stable-state updateNodeInfo failed (non-fatal)", "machine", machine.Name, "err", err)
			}
			if err := r.updateAgentCount(ctx, machine); err != nil {
				logger.Info("stable-state updateAgentCount failed (non-fatal)", "machine", machine.Name, "err", err)
			}
		}
		return ctrl.Result{RequeueAfter: requeueAfterForPhase(machine.Status.Phase)}, nil
	}

	// 5. Call the state machine (pure function).
	result, err := NextPhase(machine.Status.Phase, event)
	if err != nil {
		logger.Error(err, "invalid state machine transition",
			"machine", machine.Name,
			"phase", machine.Status.Phase,
			"event", event)
		r.Recorder.Eventf(machine, corev1.EventTypeWarning, "InvalidTransition",
			"Phase=%s Event=%s: %v", machine.Status.Phase, event, err)
		return ctrl.Result{}, nil
	}

	logger.Info("state transition",
		"machine", machine.Name,
		"from", machine.Status.Phase,
		"event", event,
		"action", result.Action,
		"to", result.NextPhase)

	// Only emit StateTransition when the phase actually changes — some transitions
	// keep the same phase (e.g., Provisioning+InstanceRunningNodeNotReady → Provisioning).
	if result.NextPhase != machine.Status.Phase {
		r.Recorder.Eventf(machine, corev1.EventTypeNormal, "StateTransition",
			"Phase changed: %s → %s (event: %s, action: %s)",
			machine.Status.Phase, result.NextPhase, event, result.Action)

		if telemetry.MachineStateTransitions != nil {
			telemetry.MachineStateTransitions.Add(ctx, 1,
				metric.WithAttributes(
					attribute.String("from", string(machine.Status.Phase)),
					attribute.String("to", string(result.NextPhase)),
				))
		}

		// Alert on preemption and failure transitions.
		switch result.NextPhase {
		case kyberv1.MachinePhasePreempted:
			if telemetry.MachinePreemptions != nil {
				telemetry.MachinePreemptions.Add(ctx, 1,
					metric.WithAttributes(
						attribute.String("machine", machine.Name),
						attribute.String("location", machine.Spec.Zone),
					))
			}
			r.fireAlert(ctx, machine, "warning", "Preempted",
				map[string]string{"location": machine.Spec.Zone, "instanceId": machine.Status.InstanceId})
		case kyberv1.MachinePhaseFailed:
			r.fireAlert(ctx, machine, "critical", "Failed",
				map[string]string{"from": string(machine.Status.Phase), "event": string(event)})
		}
	}

	// 6. Execute the transition action.
	actionRequeue, message, err := r.executeAction(ctx, machine, result.Action)
	if err != nil {
		// Non-fatal: log, surface the error in status.message, and requeue.
		// The next reconcile will re-classify and retry.
		//
		// Without the status.message update, a freshly-created Machine whose
		// first action fails would have `phase: ""` (no transition committed)
		// and no message — the UI would render "unknown" with no hint of what
		// went wrong. Writing the error to status.message lets operators see
		// "my-vm: CreateInstance failed: GCE Insert(...) ..." in the PWA.
		logger.Error(err, "action failed — will requeue",
			"machine", machine.Name,
			"action", result.Action)
		r.Recorder.Eventf(machine, corev1.EventTypeWarning, "ActionFailed",
			"Action %s failed: %v", result.Action, err)
		errMsg := fmt.Sprintf("action %s failed: %v", result.Action, err)
		if updateErr := r.updateMessage(ctx, machine, errMsg); updateErr != nil {
			logger.Error(updateErr, "failed to write error message to status")
		}
		return ctrl.Result{RequeueAfter: requeueProvisioning}, nil
	}

	// 7. Update the CRD status with the new phase.
	if err := r.updatePhase(ctx, machine, result.NextPhase, message); err != nil {
		return ctrl.Result{}, err
	}

	// Use explicit requeue from action; fall back to event-classified requeue.
	if actionRequeue > 0 {
		return ctrl.Result{RequeueAfter: actionRequeue}, nil
	}
	if requeueAfter > 0 {
		return ctrl.Result{RequeueAfter: requeueAfter}, nil
	}
	return ctrl.Result{RequeueAfter: requeueAfterForPhase(result.NextPhase)}, nil
}

// classifyEvent inspects the machine's current state to determine the next event.
// Returns ("", 0, nil) when the machine is stable and no transition is needed.
// Returns ("", duration, nil) when no transition is needed but a requeue is warranted.
func (r *MachineReconciler) classifyEvent(
	ctx context.Context,
	machine *kyberv1.Machine,
) (Event, time.Duration, error) {
	phase := machine.Status.Phase
	desired := machine.Spec.DesiredPhase

	// New machine — no prior phase.
	if phase == "" {
		return EventCRDCreated, 0, nil
	}

	// Operator intent signals (check desired phase first).
	switch phase {
	case kyberv1.MachinePhaseReady, kyberv1.MachinePhaseRunning:
		if desired == kyberv1.MachinePhaseStopped {
			return EventDesiredStopped, 0, nil
		}
	case kyberv1.MachinePhaseStopped:
		if desired == kyberv1.MachinePhaseRunning {
			return EventDesiredRunning, 0, nil
		}
		return "", 0, nil
	case kyberv1.MachinePhaseFailed:
		if desired == kyberv1.MachinePhaseRunning {
			return EventDesiredRunning, 0, nil
		}
		return "", 0, nil
	}

	// Phase-specific classification.
	switch phase {
	case kyberv1.MachinePhaseProvisioning:
		return r.classifyProvisioning(ctx, machine)

	case kyberv1.MachinePhaseReady:
		return r.classifyReady(ctx, machine)

	case kyberv1.MachinePhaseRunning:
		return r.classifyRunning(ctx, machine)

	case kyberv1.MachinePhaseStopping:
		return r.classifyStopping(ctx, machine)

	case kyberv1.MachinePhasePreempted:
		// If ActionCreateReplacementInstance failed on a previous reconcile, the phase
		// stays Preempted (updatePhase is skipped on action error) but ReplacementCount
		// was already incremented. Check the limit before attempting another create.
		if ReplacementRetryExceeded(machine.Status.ReplacementCount) {
			return EventReplacementRetryLimitReached, 0, nil
		}
		// Auto-trigger replacement immediately.
		return EventReplacementStarted, 0, nil

	case kyberv1.MachinePhaseReplacing:
		return r.classifyReplacing(ctx, machine)
	}

	return "", 0, nil
}

// classifyProvisioning determines the event when the machine is in Provisioning phase.
func (r *MachineReconciler) classifyProvisioning(ctx context.Context, machine *kyberv1.Machine) (Event, time.Duration, error) {
	// Check timeout first.
	if machine.Status.LastTransition != nil {
		if time.Since(machine.Status.LastTransition.Time) > ProvisioningTimeout {
			return EventProvisioningTimeout, 0, nil
		}
	}

	// If no instance ID yet, we're still creating — wait.
	if machine.Status.InstanceId == "" {
		return "", requeueProvisioning, nil
	}

	observation, err := r.observeMachineCapacity(ctx, machine, adapters.DesiredOnline)
	if err != nil {
		// Instance not found or transient error — requeue.
		return "", requeueProvisioning, nil
	}

	if observation.State != adapters.CapacityAvailable {
		// Still starting up.
		return EventInstanceRunningNodeNotReady, requeueProvisioning, nil
	}
	// Instance is running — check if the k8s node is Ready.
	node, err := r.getNodeForMachine(ctx, machine)
	if err != nil {
		return "", 0, err
	}
	if node == nil || !r.isManagedNodeReady(node) {
		return EventInstanceRunningNodeNotReady, requeueProvisioning, nil
	}

	return EventInstanceRunningNodeReady, 0, nil
}

// classifyReady determines the event when the machine is in Ready phase.
func (r *MachineReconciler) classifyReady(ctx context.Context, machine *kyberv1.Machine) (Event, time.Duration, error) {
	if event, requeueAfter := r.classifyExistingNodeProviderState(ctx, machine); event != "" || requeueAfter != 0 {
		return event, requeueAfter, nil
	}
	node, err := r.getNodeForMachine(ctx, machine)
	if err != nil {
		return "", 0, err
	}

	if node == nil || !r.isManagedNodeReady(node) {
		// Node disappeared — check if preempted.
		return r.classifyNodeDisappeared(ctx, machine)
	}

	// Count agent pods on this node.
	agentCount, err := r.countAgentPods(ctx, machine)
	if err != nil {
		return "", 0, err
	}
	if agentCount > 0 {
		return EventAgentScheduled, 0, nil
	}

	// Stable Ready state.
	return "", 0, nil
}

// classifyRunning determines the event when the machine is in Running phase.
func (r *MachineReconciler) classifyRunning(ctx context.Context, machine *kyberv1.Machine) (Event, time.Duration, error) {
	if event, requeueAfter := r.classifyExistingNodeProviderState(ctx, machine); event != "" || requeueAfter != 0 {
		return event, requeueAfter, nil
	}
	node, err := r.getNodeForMachine(ctx, machine)
	if err != nil {
		return "", 0, err
	}

	if node == nil || !r.isManagedNodeReady(node) {
		return r.classifyNodeDisappeared(ctx, machine)
	}

	// Count agent pods on this node.
	agentCount, err := r.countAgentPods(ctx, machine)
	if err != nil {
		return "", 0, err
	}
	if agentCount == 0 {
		return EventAllAgentsRemoved, 0, nil
	}

	// Stable Running state — update agent count in status.
	return "", 0, nil
}

// classifyExistingNodeProviderState makes provider state authoritative for
// managed simulators that borrow a real cluster node. That node cannot safely
// disappear during a simulated interruption, so relying only on node absence
// would make preempted/failed/stopped scenarios invisible.
func (r *MachineReconciler) classifyExistingNodeProviderState(ctx context.Context, machine *kyberv1.Machine) (Event, time.Duration) {
	if r.ComputeAdapter.NodeAttachment() != adapters.NodeAttachmentExisting || machine.Status.InstanceId == "" {
		return "", 0
	}
	observation, err := r.ComputeAdapter.Observe(ctx, machine.Status.InstanceId)
	if err != nil {
		return "", requeueAfterForPhase(machine.Status.Phase)
	}
	if observation.Interruption == adapters.InterruptionPreempted && machine.Spec.Spot {
		return EventNodeDisappearedPreempted, 0
	}
	switch observation.State {
	case adapters.InstanceStateRunning:
		return "", 0
	case adapters.InstanceStatePending, adapters.InstanceStateUnknown:
		return "", requeueAfterForPhase(machine.Status.Phase)
	default:
		return EventNodeDisappearedFailed, 0
	}
}

// classifyStopping determines the event when the machine is in Stopping phase.
// The stop sequence is: wait for agent pods to drain, then wait for VM to stop.
// A timeout forces the VM stop regardless of agent state.
func (r *MachineReconciler) classifyStopping(ctx context.Context, machine *kyberv1.Machine) (Event, time.Duration, error) {
	// Check stopping timeout first — if exceeded, force-stop regardless of agents.
	if machine.Status.LastTransition != nil {
		if time.Since(machine.Status.LastTransition.Time) > StoppingTimeout {
			return EventStoppingTimeout, 0, nil
		}
	}

	// Wait for agent pods to drain before considering the VM stopped.
	agentCount, err := r.countAgentPods(ctx, machine)
	if err != nil {
		// Can't determine agent count — requeue so we don't act on bad data.
		return "", requeueStopping, nil
	}
	if agentCount > 0 {
		// Agents still running — wait for drain.
		return "", requeueStopping, nil
	}

	// No agents remain — check if VM is stopped.
	if machine.Status.InstanceId != "" {
		observation, err := r.observeMachineCapacity(ctx, machine, adapters.DesiredOffline)
		if err == nil && observation.State == adapters.CapacityOffline {
			return EventAllAgentsStoppedVMStopped, 0, nil
		}
	}

	return "", requeueStopping, nil
}

// classifyReplacing determines the event when the machine is in Replacing phase.
func (r *MachineReconciler) classifyReplacing(ctx context.Context, machine *kyberv1.Machine) (Event, time.Duration, error) {
	// Check if replacement count has exceeded the limit.
	if ReplacementRetryExceeded(machine.Status.ReplacementCount) {
		return EventReplacementRetryLimitReached, 0, nil
	}

	if machine.Status.InstanceId == "" {
		return "", requeueReplacing, nil
	}

	// Check if the new instance is running and the node is Ready.
	observation, err := r.observeMachineCapacity(ctx, machine, adapters.DesiredOnline)
	if err != nil {
		return "", requeueReplacing, nil
	}
	// If the replacement instance was itself terminated (double preemption),
	// fire ReplacementFailed so the state machine can retry with budget.
	if observation.Reason == adapters.ReasonInterrupted {
		return EventReplacementFailed, 0, nil
	}
	if observation.State != adapters.CapacityAvailable {
		return "", requeueReplacing, nil
	}

	node, err := r.getNodeForMachine(ctx, machine)
	if err != nil {
		return "", 0, err
	}
	if node == nil || !r.isManagedNodeReady(node) {
		return "", requeueReplacing, nil
	}

	return EventReplacementSucceeded, 0, nil
}

// classifyNodeDisappeared checks the provider observation to determine if a
// missing node is due to interruption or an unexpected failure.
func (r *MachineReconciler) classifyNodeDisappeared(ctx context.Context, machine *kyberv1.Machine) (Event, time.Duration, error) {
	if machine.Status.InstanceId == "" {
		return EventNodeDisappearedFailed, 0, nil
	}

	observation, err := r.observeMachineCapacity(ctx, machine, adapters.DesiredOnline)
	if err != nil {
		// Can't determine — treat as failed.
		return EventNodeDisappearedFailed, 0, nil
	}

	if observation.Reason == adapters.ReasonInterrupted && machineInterruptible(machine) {
		return EventNodeDisappearedPreempted, 0, nil
	}
	return EventNodeDisappearedFailed, 0, nil
}

func (r *MachineReconciler) observeMachineCapacity(
	ctx context.Context,
	machine *kyberv1.Machine,
	desired adapters.DesiredAvailability,
) (adapters.CapacityObservation, error) {
	if r.capacityProviderFor(machine) != nil {
		return r.reconcileCapacity(ctx, machine, desired)
	}
	observation, err := r.ComputeAdapter.Observe(ctx, machine.Status.InstanceId)
	if err != nil {
		return adapters.CapacityObservation{}, err
	}
	return adapters.CapacityObservationFromInstance(
		adapters.ProviderRef(machine.Status.InstanceId),
		map[string]string{MachineLabelKey: machine.Name},
		observation,
	), nil
}

// executeAction performs the cloud and k8s operations required by the transition action.
// Returns (requeueAfter, statusMessage, error).
func (r *MachineReconciler) executeAction(
	ctx context.Context,
	machine *kyberv1.Machine,
	action Action,
) (time.Duration, string, error) {
	logger := log.FromContext(ctx)

	switch action {
	case ActionCreateInstance:
		if r.capacityProviderFor(machine) != nil {
			observation, err := r.reconcileCapacity(ctx, machine, adapters.DesiredOnline)
			if err != nil {
				return 0, fmt.Sprintf("provisioning capacity failed: %v", err), err
			}
			logger.Info("capacity provision reconciled", "machine", machine.Name,
				"providerRef", observation.ProviderRef, "state", observation.State)
			r.Recorder.Eventf(machine, corev1.EventTypeNormal, "CapacityProvisioning",
				"Machine capacity provisioning requested")
			return requeueProvisioning, "", nil
		}
		// Clean any stale k8s Node + node-password Secret bound to a previous
		// instance with this machine's label. Without this, a re-provision
		// (preemption recovery, ProvisioningTimeout retry) hits k3s's
		// duplicate-hostname rejection: the new VM tries to register with the
		// same hostname as the dead one, k3s server still has the old
		// node-password recorded, registration fails forever. See kyber-gcp
		// incident 2026-04-27 (smallguy preemption + 26h pod stuck Terminating).
		//
		// Only fires on RE-provision (InstanceId non-empty). Skipping on
		// first-ever-provision keeps tests that pre-create a Node-as-setup
		// shortcut working, and avoids a redundant List on the happy path.
		if machine.Status.InstanceId != "" {
			if err := r.cleanupStaleNodeArtifacts(ctx, machine); err != nil {
				logger.Error(err, "cleanupStaleNodeArtifacts failed (non-fatal)", "machine", machine.Name)
			}
		}
		spec := r.buildMachineSpec(machine)
		instanceID, err := r.ComputeAdapter.CreateInstance(ctx, spec)
		if err != nil {
			return 0, fmt.Sprintf("CreateInstance failed: %v", err), err
		}
		// Patch instanceId into status immediately so classifyProvisioning can use it.
		patch := client.MergeFrom(machine.DeepCopy())
		machine.Status.InstanceId = instanceID
		if err := r.Status().Patch(ctx, machine, patch); err != nil {
			return 0, "", fmt.Errorf("patching instanceId: %w", err)
		}
		logger.Info("instance created", "machine", machine.Name, "instanceID", instanceID)
		r.Recorder.Eventf(machine, corev1.EventTypeNormal, "InstanceCreated",
			"Compute instance created: %s", instanceID)
		return requeueProvisioning, "", nil

	case ActionWaitForNode:
		return requeueProvisioning, "", nil

	case ActionUpdateStatus:
		// Status update (phase, nodeName, agentCount) handled by updatePhase and updateNodeInfo.
		if err := r.updateNodeInfo(ctx, machine); err != nil {
			logger.Error(err, "failed to update node info (non-fatal)", "machine", machine.Name)
		}
		if err := r.updateAgentCount(ctx, machine); err != nil {
			logger.Error(err, "failed to update agent count (non-fatal)", "machine", machine.Name)
		}
		return 0, "", nil

	case ActionMarkFailed:
		r.Recorder.Eventf(machine, corev1.EventTypeWarning, "MachineFailed",
			"Machine entered Failed state")
		return 0, "machine entered failed state", nil

	case ActionStopVM:
		if r.capacityProviderFor(machine) != nil {
			observation, err := r.reconcileCapacity(ctx, machine, adapters.DesiredOffline)
			if err != nil {
				return 0, "", fmt.Errorf("stopping capacity: %w", err)
			}
			logger.Info("offline intent reconciled", "machine", machine.Name,
				"providerRef", observation.ProviderRef, "state", observation.State)
			r.Recorder.Eventf(machine, corev1.EventTypeNormal, "CapacityStopping",
				"Machine capacity stop requested")
			return requeueStopping, "", nil
		}
		if machine.Status.InstanceId == "" {
			return 0, "stopped (no instance)", nil
		}
		if err := r.ComputeAdapter.StopInstance(ctx, machine.Status.InstanceId); err != nil {
			return 0, "", fmt.Errorf("stopping instance: %w", err)
		}
		logger.Info("stop issued to instance", "machine", machine.Name, "instanceID", machine.Status.InstanceId)
		r.Recorder.Eventf(machine, corev1.EventTypeNormal, "InstanceStopping",
			"Compute instance stop requested: %s", machine.Status.InstanceId)
		return requeueStopping, "", nil

	case ActionForceStopVM:
		if r.capacityProviderFor(machine) != nil {
			if _, err := r.reconcileCapacity(ctx, machine, adapters.DesiredOffline); err != nil {
				logger.Error(err, "force-stop capacity reconcile failed (non-fatal, proceeding to Stopped)", "machine", machine.Name)
			}
			r.Recorder.Eventf(machine, corev1.EventTypeWarning, "StoppingTimeout",
				"Stopping timeout exceeded — forcing capacity offline")
			return 0, "force-stopped after timeout", nil
		}
		if machine.Status.InstanceId != "" {
			if err := r.ComputeAdapter.StopInstance(ctx, machine.Status.InstanceId); err != nil {
				logger.Error(err, "force-stop failed (non-fatal, proceeding to Stopped)", "machine", machine.Name)
			}
		}
		r.Recorder.Eventf(machine, corev1.EventTypeWarning, "StoppingTimeout",
			"Stopping timeout exceeded — force-stopping VM")
		return 0, "force-stopped after timeout", nil

	case ActionStartInstance:
		if r.capacityProviderFor(machine) != nil {
			observation, err := r.reconcileCapacity(ctx, machine, adapters.DesiredOnline)
			if err != nil {
				return 0, "", fmt.Errorf("starting capacity: %w", err)
			}
			logger.Info("online intent reconciled", "machine", machine.Name,
				"providerRef", observation.ProviderRef, "state", observation.State)
			r.Recorder.Eventf(machine, corev1.EventTypeNormal, "CapacityStarting",
				"Machine capacity start requested")
			return requeueProvisioning, "", nil
		}
		if machine.Status.InstanceId == "" {
			return 0, "", fmt.Errorf("cannot start: no instance ID recorded")
		}
		if err := r.ComputeAdapter.StartInstance(ctx, machine.Status.InstanceId); err != nil {
			return 0, "", fmt.Errorf("starting instance: %w", err)
		}
		logger.Info("start issued to instance", "machine", machine.Name, "instanceID", machine.Status.InstanceId)
		r.Recorder.Eventf(machine, corev1.EventTypeNormal, "InstanceStarting",
			"Compute instance start requested: %s", machine.Status.InstanceId)
		return requeueProvisioning, "", nil

	case ActionBeginReplacement:
		r.Recorder.Eventf(machine, corev1.EventTypeWarning, "PreemptionDetected",
			"Interruptible instance reclaimed in location %s (instanceID: %s) — initiating replacement",
			machine.Spec.Zone, machine.Status.InstanceId)
		logger.Info("preemption detected — beginning replacement",
			"machine", machine.Name,
			"location", machine.Spec.Zone)
		return requeueImmediate, "preempted — replacement pending", nil

	case ActionCreateReplacementInstance:
		// Increment replacement count first so failures are observable via the retry
		// limit. This is a separate status patch because the create might fail and we
		// still want the count to persist — otherwise classifyReplacing would never see
		// ReplacementRetryExceeded return true, leaving the machine looping forever.
		countPatch := client.MergeFrom(machine.DeepCopy())
		machine.Status.ReplacementCount++
		if err := r.Status().Patch(ctx, machine, countPatch); err != nil {
			return 0, "", fmt.Errorf("patching replacement count: %w", err)
		}

		if r.capacityProviderFor(machine) != nil {
			observation, err := r.reconcileCapacity(ctx, machine, adapters.DesiredOnline)
			if err != nil {
				r.Recorder.Eventf(machine, corev1.EventTypeWarning, "ReplacementFailed",
					"Replacement capacity reconciliation failed (attempt %d/%d): %v",
					machine.Status.ReplacementCount, MaxReplacementRetries, err)
				return 0, "", fmt.Errorf("reconciling replacement capacity: %w", err)
			}
			logger.Info("replacement capacity reconciled",
				"machine", machine.Name,
				"providerRef", observation.ProviderRef,
				"replacementCount", machine.Status.ReplacementCount)
			r.Recorder.Eventf(machine, corev1.EventTypeNormal, "ReplacementReconciled",
				"Replacement capacity reconciled (attempt %d/%d)",
				machine.Status.ReplacementCount, MaxReplacementRetries)
			return requeueReplacing, "", nil
		}

		// Same cleanup as ActionCreateInstance — preemption replacement
		// reuses the hostname, so the old node-password Secret has to go
		// before the new VM registers. See ActionCreateInstance for context.
		if err := r.cleanupStaleNodeArtifacts(ctx, machine); err != nil {
			logger.Error(err, "cleanupStaleNodeArtifacts failed (non-fatal)", "machine", machine.Name)
		}

		// Attempt the create. Replacement uses the same location (PV locality requirement).
		spec := r.buildMachineSpec(machine)
		instanceID, err := r.ComputeAdapter.CreateInstance(ctx, spec)
		if err != nil {
			// Count was already incremented above; the next reconcile will re-enter this
			// path if retries remain, or transition to Failed if ReplacementRetryExceeded.
			r.Recorder.Eventf(machine, corev1.EventTypeWarning, "ReplacementFailed",
				"Replacement instance creation failed (attempt %d/%d): %v",
				machine.Status.ReplacementCount, MaxReplacementRetries, err)
			return 0, "", fmt.Errorf("creating replacement instance: %w", err)
		}

		// Instance created successfully — persist the new instance ID.
		idPatch := client.MergeFrom(machine.DeepCopy())
		machine.Status.InstanceId = instanceID
		if err := r.Status().Patch(ctx, machine, idPatch); err != nil {
			return 0, "", fmt.Errorf("patching new instance ID: %w", err)
		}
		logger.Info("replacement instance created",
			"machine", machine.Name,
			"instanceID", instanceID,
			"replacementCount", machine.Status.ReplacementCount)
		r.Recorder.Eventf(machine, corev1.EventTypeNormal, "ReplacementCreated",
			"Replacement instance created: %s (attempt %d/%d)",
			instanceID, machine.Status.ReplacementCount, MaxReplacementRetries)
		return requeueReplacing, "", nil

	case ActionCompleteReplacement:
		if err := r.updateNodeInfo(ctx, machine); err != nil {
			logger.Error(err, "failed to update node info after replacement (non-fatal)", "machine", machine.Name)
		}
		r.Recorder.Eventf(machine, corev1.EventTypeNormal, "ReplacementComplete",
			"Replacement instance is ready (instanceID: %s)", machine.Status.InstanceId)
		return 0, "", nil

	case ActionRetryReplacement:
		// Replacement failed — requeue after delay so classifyReplacing can retry.
		r.Recorder.Eventf(machine, corev1.EventTypeWarning, "ReplacementRetrying",
			"Replacement failed — retrying in %s (attempt %d/%d)",
			replacementRetryDelay, machine.Status.ReplacementCount, MaxReplacementRetries)
		return replacementRetryDelay, "", nil

	case ActionMarkReplacementFailed:
		r.Recorder.Eventf(machine, corev1.EventTypeWarning, "ReplacementExhausted",
			"All %d replacement attempts failed — manual intervention required. "+
				"Consider changing the provider location or increasing quota.", MaxReplacementRetries)
		return 0, fmt.Sprintf("replacement failed after %d attempts", MaxReplacementRetries), nil

	default:
		return 0, "", fmt.Errorf("unknown action: %q", action)
	}
}

func (r *MachineReconciler) capacityProviderFor(machine *kyberv1.Machine) adapters.CapacityProvider {
	// Direct GCE deliberately retains the legacy action-oriented path. Its
	// state machine distinguishes an operator stop/start from provider-driven
	// preemption and performs stale k3s Node/password cleanup before replacement.
	// The declarative GCE adapter is not safe to adopt until observation is
	// separated from mutation and those invariants have equivalent coverage.
	if machine.Spec.Provider == kyberv1.MachineProviderGCE {
		return nil
	}
	if r.CapacityProvider != nil {
		if r.CapacityProvider.Type() == string(machine.Spec.Provider) {
			return r.CapacityProvider
		}
		return nil
	}
	provider, _ := r.ComputeAdapter.(adapters.CapacityProvider)
	if provider != nil && provider.Type() != string(machine.Spec.Provider) {
		return nil
	}
	return provider
}

func (r *MachineReconciler) reconcileCapacity(
	ctx context.Context,
	machine *kyberv1.Machine,
	availability adapters.DesiredAvailability,
) (adapters.CapacityObservation, error) {
	provider := r.capacityProviderFor(machine)
	if provider == nil {
		return adapters.CapacityObservation{}, fmt.Errorf("capacity provider is not configured")
	}
	desired := adapters.DesiredMachine{
		Availability:  availability,
		Profile:       machineProfile(machine),
		DiskSizeGb:    int(machine.Spec.DiskSizeGb),
		Interruptible: machineInterruptible(machine),
		Location:      machineLocation(machine),
		Labels:        map[string]string{MachineLabelKey: machine.Name},
		NodeBootstrap: adapters.NodeBootstrap{
			JoinToken: r.K3sJoinToken,
			ServerURL: r.K3sServerURL,
		},
		Managed: machine.Spec.ManagementMode != kyberv1.MachineManagementExternal,
	}
	if selectorProvider, ok := provider.(adapters.CapacityNodeSelector); ok {
		selector := selectorProvider.NodeSelector(adapters.MachineIdentity{Name: machine.Name}, adapters.ProviderRef(providerReference(machine)))
		if len(selector) > 0 {
			var nodes corev1.NodeList
			if err := r.List(ctx, &nodes, client.MatchingLabels(selector)); err != nil {
				return adapters.CapacityObservation{}, fmt.Errorf("observing attached provider nodes: %w", err)
			}
			desired.AttachmentObserved = true
			desired.AttachedNodes = len(nodes.Items)
		}
	}
	observation, err := provider.Reconcile(
		ctx,
		adapters.MachineIdentity{Name: machine.Name},
		desired,
		adapters.ProviderRef(providerReference(machine)),
	)
	if err != nil {
		return adapters.CapacityObservation{}, fmt.Errorf("reconciling %s capacity: %w", availability, err)
	}

	ref := string(observation.ProviderRef)
	observedAvailability := kyberv1.MachineAvailability(observation.State)
	resolvedProfileMissing := desired.Profile != "" && machine.Status.ResolvedProfile == nil
	if machine.Status.ProviderRef != ref || machine.Status.InstanceId != ref || machine.Status.Availability != observedAvailability || resolvedProfileMissing || machine.Status.InternalIP != observation.InternalIP || machine.Status.ExternalIP != observation.ExternalIP {
		patch := client.MergeFrom(machine.DeepCopy())
		machine.Status.ProviderRef = ref
		// Dual-write during the compatibility window. Legacy readers continue
		// using instanceId until every provider and client has migrated.
		machine.Status.InstanceId = ref
		machine.Status.Availability = observedAvailability
		machine.Status.InternalIP = observation.InternalIP
		machine.Status.ExternalIP = observation.ExternalIP
		if machine.Status.ResolvedProfile == nil && desired.Profile != "" {
			machine.Status.ResolvedProfile = &kyberv1.ResolvedMachineProfile{
				ID: desired.Profile, Capacity: machine.Spec.Capacity,
			}
		}
		if err := r.Status().Patch(ctx, machine, patch); err != nil {
			return adapters.CapacityObservation{}, fmt.Errorf("patching provider reference: %w", err)
		}
	}
	return observation, nil
}

func machineProfile(machine *kyberv1.Machine) string {
	if machine.Spec.Profile != "" {
		return machine.Spec.Profile
	}
	return machine.Spec.MachineType
}

func machineLocation(machine *kyberv1.Machine) string {
	if machine.Spec.Location != "" {
		return machine.Spec.Location
	}
	return machine.Spec.Zone
}

func machineInterruptible(machine *kyberv1.Machine) bool {
	if machine.Spec.AvailabilityClass != "" {
		return machine.Spec.AvailabilityClass == kyberv1.MachineAvailabilityCostOptimized
	}
	return machine.Spec.Spot
}

func providerReference(machine *kyberv1.Machine) string {
	if machine.Status.ProviderRef != "" {
		return machine.Status.ProviderRef
	}
	return machine.Status.InstanceId
}

// handleDeletion runs the machine cleanup finalizer.
func (r *MachineReconciler) handleDeletion(ctx context.Context, machine *kyberv1.Machine) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if !containsString(machine.Finalizers, MachineFinalizer) {
		return ctrl.Result{}, nil
	}

	// If the namespace itself is Terminating, skip external deprovisioning and
	// self-remove the finalizer. The controller's own pod is typically being
	// torn down in the same teardown — holding the finalizer for a cloud call
	// that may fail or time out just deadlocks the namespace terminator.
	// Operators accept ownership of orphaned external VMs in this escape path.
	// See issue #64.
	if terminating, err := r.isNamespaceTerminating(ctx, machine.Namespace); err != nil {
		logger.Error(err, "checking namespace DeletionTimestamp (treating as not terminating)", "namespace", machine.Namespace)
	} else if terminating {
		// Loudly flag any external resource we're about to orphan. The Machine CR
		// and its namespace-scoped events are about to be GC'd along with the
		// namespace, so operators need this to land in cluster-wide event sinks
		// (central logging, datadog, kube-state-metrics) before it vanishes.
		if machine.Status.InstanceId != "" {
			logger.Error(nil, "ORPHANING external VM: namespace teardown bypassed finalizer, manual cleanup required",
				"machine", machine.Name,
				"namespace", machine.Namespace,
				"instanceId", machine.Status.InstanceId,
				"location", machine.Spec.Zone,
				"provider", machine.Spec.Provider)
			r.Recorder.Eventf(machine, corev1.EventTypeWarning, "ExternalResourceOrphaned",
				"Provider %s instance %s (location %s) left running: namespace %s teardown bypassed finalizer; reclaim it with provider tooling",
				machine.Spec.Provider, machine.Status.InstanceId, machine.Spec.Zone, machine.Namespace)
		} else {
			logger.Info("namespace is Terminating; no external VM to orphan, self-removing finalizer",
				"machine", machine.Name, "namespace", machine.Namespace)
		}
		patch := client.MergeFrom(machine.DeepCopy())
		machine.Finalizers = removeString(machine.Finalizers, MachineFinalizer)
		if err := r.Patch(ctx, machine, patch); err != nil {
			return ctrl.Result{}, fmt.Errorf("removing finalizer during namespace teardown: %w", err)
		}
		return ctrl.Result{}, nil
	}

	logger.Info("running machine finalizer", "machine", machine.Name)

	if r.capacityProviderFor(machine) != nil {
		observation, err := r.reconcileCapacity(ctx, machine, adapters.DesiredDeleted)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("deleting capacity during finalizer: %w", err)
		}
		if observation.State != adapters.CapacityAbsent {
			logger.Info("waiting for provider capacity deletion", "machine", machine.Name,
				"state", observation.State, "reason", observation.Reason)
			return ctrl.Result{RequeueAfter: requeueProvisioning}, nil
		}
		logger.Info("provider capacity deleted or unregistered", "machine", machine.Name)
	} else if machine.Status.InstanceId != "" {
		// Delete the legacy provider instance if one exists.
		// Attempt stop first (graceful), then delete.
		if err := r.ComputeAdapter.StopInstance(ctx, machine.Status.InstanceId); err != nil {
			// Non-fatal — proceed to delete.
			logger.Error(err, "stop during finalizer failed (non-fatal)", "machine", machine.Name)
		}
		if err := r.ComputeAdapter.DeleteInstance(ctx, machine.Status.InstanceId); err != nil {
			// If the instance is already gone, continue. Otherwise return error.
			if !strings.Contains(err.Error(), "not found") {
				return ctrl.Result{}, fmt.Errorf("deleting instance during finalizer: %w", err)
			}
		}
		logger.Info("Compute instance deleted", "machine", machine.Name, "instanceID", machine.Status.InstanceId)
	}

	// Only managed-node providers own their Kubernetes Node. Existing-node
	// providers borrow a real cluster node, which must survive Machine deletion.
	if r.ComputeAdapter.NodeAttachment() == adapters.NodeAttachmentManaged {
		node, err := r.getNodeForMachine(ctx, machine)
		if err != nil {
			logger.Error(err, "listing nodes during finalizer (non-fatal)", "machine", machine.Name)
		} else if node != nil {
			if err := r.Delete(ctx, node); err != nil && !errors.IsNotFound(err) {
				logger.Error(err, "deleting stale node during finalizer (non-fatal)", "machine", machine.Name)
			} else {
				logger.Info("stale k8s node deleted", "machine", machine.Name, "node", node.Name)
			}
		}
	}

	// Remove finalizer to allow CRD deletion to complete.
	patch := client.MergeFrom(machine.DeepCopy())
	machine.Finalizers = removeString(machine.Finalizers, MachineFinalizer)
	if err := r.Patch(ctx, machine, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("removing finalizer: %w", err)
	}

	r.Recorder.Eventf(machine, corev1.EventTypeNormal, "MachineDeleted",
		"Machine cleanup complete — provider capacity released, finalizer removed")

	return ctrl.Result{}, nil
}

// isNamespaceTerminating reports whether the given namespace has been marked
// for deletion.
func (r *MachineReconciler) isNamespaceTerminating(ctx context.Context, name string) (bool, error) {
	ns := &corev1.Namespace{}
	if err := r.Get(ctx, types.NamespacedName{Name: name}, ns); err != nil {
		if errors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return !ns.DeletionTimestamp.IsZero(), nil
}

// ensureFinalizer adds the machine finalizer if not already present.
func (r *MachineReconciler) ensureFinalizer(ctx context.Context, machine *kyberv1.Machine) error {
	if containsString(machine.Finalizers, MachineFinalizer) {
		return nil
	}
	patch := client.MergeFrom(machine.DeepCopy())
	machine.Finalizers = append(machine.Finalizers, MachineFinalizer)
	return r.Patch(ctx, machine, patch)
}

// updatePhase patches the machine's status.phase and updates LastTransition.
func (r *MachineReconciler) updatePhase(
	ctx context.Context,
	machine *kyberv1.Machine,
	newPhase kyberv1.MachinePhase,
	message string,
) error {
	if machine.Status.Phase == newPhase && machine.Status.Message == message {
		return nil // nothing to update
	}
	// Capture the patch base BEFORE mutating — all mutations below appear in the diff.
	patch := client.MergeFrom(machine.DeepCopy())
	now := metav1.Now()
	machine.Status.Phase = newPhase
	machine.Status.LastTransition = &now
	machine.Status.Message = message
	return r.Status().Patch(ctx, machine, patch)
}

// updateMessage patches only status.message without changing the phase.
// Used when an action fails and we want the error surfaced to the UI
// without committing a phase transition.
func (r *MachineReconciler) updateMessage(
	ctx context.Context,
	machine *kyberv1.Machine,
	message string,
) error {
	if machine.Status.Message == message {
		return nil
	}
	patch := client.MergeFrom(machine.DeepCopy())
	machine.Status.Message = message
	return r.Status().Patch(ctx, machine, patch)
}

// updateNodeInfo reads the node for the machine and patches status.nodeName, IPs,
// and capacity fields. When the node is not found, clears all three capacity fields so
// stale capacity data is not shown (e.g., after preemption).
func (r *MachineReconciler) updateNodeInfo(ctx context.Context, machine *kyberv1.Machine) error {
	node, err := r.getNodeForMachine(ctx, machine)
	if err != nil {
		return err
	}

	patch := client.MergeFrom(machine.DeepCopy())

	if node == nil {
		// Node gone (preempted/deleted) — clear all three capacity fields so
		// the PWA / 409 path knows we don't have ground truth.
		machine.Status.ObservedCapacity = nil
		machine.Status.AssignableCapacity = nil
		machine.Status.AvailableCapacity = nil
		return r.Status().Patch(ctx, machine, patch)
	}

	machine.Status.NodeName = node.Name

	// Observed: live node.status.allocatable. EphemeralStorage is included since
	// #129 PR-C — the field is optional/omitempty so a node that doesn't report
	// it (some legacy kubelet configs) still serialises cleanly with a zero
	// quantity in the disk slot.
	observedCPU := node.Status.Allocatable[corev1.ResourceCPU]
	observedMem := node.Status.Allocatable[corev1.ResourceMemory]
	observedDisk := node.Status.Allocatable[corev1.ResourceEphemeralStorage]
	observed := kyberv1.MachineCapacity{CPU: observedCPU, Memory: observedMem, EphemeralStorage: observedDisk}
	machine.Status.ObservedCapacity = &observed

	// Assignable: observed minus platform reservation.
	assignable := ComputeAssignable(observed, r.PlatformReservation)
	machine.Status.AssignableCapacity = &assignable

	// Available: assignable minus sum of agents' requests on this machine.
	// List all agents in the namespace; ComputeAvailable filters by Spec.Machine.
	agentList := &kyberv1.AgentList{}
	if err := r.List(ctx, agentList, client.InNamespace(machine.Namespace)); err != nil {
		// Don't fail the whole status update if agent list errors transiently —
		// surface the assignable bound and leave Available unset until next reconcile.
		log.FromContext(ctx).Error(err, "listing agents for AvailableCapacity (continuing)", "machine", machine.Name)
		machine.Status.AvailableCapacity = nil
	} else {
		available := ComputeAvailable(assignable, agentList.Items, machine.Name)
		machine.Status.AvailableCapacity = &available
	}

	// Update IPs from the provider observation.
	if machine.Status.InstanceId != "" && r.capacityProviderFor(machine) == nil {
		observation, err := r.ComputeAdapter.Observe(ctx, machine.Status.InstanceId)
		if err == nil {
			machine.Status.InternalIP = observation.InternalIP
			machine.Status.ExternalIP = observation.ExternalIP
		}
	}

	return r.Status().Patch(ctx, machine, patch)
}

// updateAgentCount queries pods on the machine's node and patches status.agentCount.
func (r *MachineReconciler) updateAgentCount(ctx context.Context, machine *kyberv1.Machine) error {
	count, err := r.countAgentPods(ctx, machine)
	if err != nil {
		return err
	}
	if int32(count) == machine.Status.AgentCount {
		return nil
	}
	patch := client.MergeFrom(machine.DeepCopy())
	machine.Status.AgentCount = int32(count)
	return r.Status().Patch(ctx, machine, patch)
}

// cleanupStaleNodeArtifacts removes the k8s Node object AND its
// k3s.cattle.io/node-password Secret in kube-system for any node currently
// labeled with this machine. Both are tied to the dead VM's hostname; a
// re-provisioned VM with the same hostname would otherwise hit k3s's
// "Node password rejected, duplicate hostname" registration error, then
// loop forever waiting for a node that will never be Ready.
//
// Idempotent — when the Machine has never had a node (first-ever
// CreateInstance) this is a no-op. NotFound on either delete is fine.
//
// Non-fatal from the caller's perspective: any error here logs but does
// not abort the create. The worst case is the new VM hits the same
// rejection the operator hit pre-fix, which is no regression.
func (r *MachineReconciler) cleanupStaleNodeArtifacts(ctx context.Context, machine *kyberv1.Machine) error {
	logger := log.FromContext(ctx)
	nodeList := &corev1.NodeList{}
	if err := r.List(ctx, nodeList, client.MatchingLabels{
		MachineLabelKey: machine.Name,
	}); err != nil {
		return fmt.Errorf("listing nodes for cleanup: %w", err)
	}
	if len(nodeList.Items) == 0 {
		return nil
	}
	for i := range nodeList.Items {
		node := &nodeList.Items[i]
		// k3s stores per-node passwords as a Secret in kube-system named
		// "<node-name>.node-password.k3s". Delete it BEFORE the Node
		// object so a racing reconcile doesn't observe a Node-less
		// dangling Secret. NotFound is fine.
		passwordSecret := &corev1.Secret{}
		passwordSecret.Namespace = "kube-system"
		passwordSecret.Name = node.Name + ".node-password.k3s"
		if err := r.Delete(ctx, passwordSecret); err != nil && !errors.IsNotFound(err) {
			logger.Error(err, "deleting stale node-password Secret (non-fatal)",
				"machine", machine.Name, "secret", passwordSecret.Name)
		} else if err == nil {
			logger.Info("stale node-password Secret deleted",
				"machine", machine.Name, "secret", passwordSecret.Name)
		}
		if err := r.Delete(ctx, node); err != nil && !errors.IsNotFound(err) {
			logger.Error(err, "deleting stale node (non-fatal)",
				"machine", machine.Name, "node", node.Name)
		} else if err == nil {
			logger.Info("stale k8s node deleted",
				"machine", machine.Name, "node", node.Name)
			r.Recorder.Eventf(machine, corev1.EventTypeNormal, "StaleNodeCleaned",
				"Removed stale k8s Node %s + node-password Secret prior to re-provision",
				node.Name)
		}
	}
	return nil
}

// getNodeForMachine lists k8s nodes with the machine label and returns the first ready one.
// Returns nil if no node is found.
func (r *MachineReconciler) getNodeForMachine(ctx context.Context, machine *kyberv1.Machine) (*corev1.Node, error) {
	if selectorProvider, ok := r.capacityProviderFor(machine).(adapters.CapacityNodeSelector); ok {
		selector := selectorProvider.NodeSelector(
			adapters.MachineIdentity{Name: machine.Name},
			adapters.ProviderRef(providerReference(machine)),
		)
		if len(selector) > 0 {
			nodes := &corev1.NodeList{}
			if err := r.List(ctx, nodes, client.MatchingLabels(selector)); err != nil {
				return nil, fmt.Errorf("listing provider nodes for machine %s: %w", machine.Name, err)
			}
			for i := range nodes.Items {
				if isNodeReady(&nodes.Items[i]) {
					return &nodes.Items[i], nil
				}
			}
			return nil, nil
		}
	}
	nodeList := &corev1.NodeList{}
	if err := r.List(ctx, nodeList, client.MatchingLabels{
		MachineLabelKey: machine.Name,
	}); err != nil {
		return nil, fmt.Errorf("listing nodes for machine %s: %w", machine.Name, err)
	}
	if len(nodeList.Items) == 0 {
		// The fake provider deliberately reuses one real local node while still
		// traversing managed lifecycle operations. It never mutates Node labels,
		// which keeps the production control-plane RBAC read-only for this path.
		if machine.Spec.Provider == kyberv1.MachineProviderFake &&
			r.ComputeAdapter.NodeAttachment() == adapters.NodeAttachmentExisting {
			var nodes corev1.NodeList
			if err := r.List(ctx, &nodes); err != nil {
				return nil, fmt.Errorf("listing existing nodes for fake machine %s: %w", machine.Name, err)
			}
			for i := range nodes.Items {
				if isNodeReady(&nodes.Items[i]) {
					return &nodes.Items[i], nil
				}
			}
		}
		return nil, nil
	}
	// Return the first node (there should only be one per machine).
	return &nodeList.Items[0], nil
}

// countAgentPods counts pods with the agent label on this machine's node.
// It requires a spec.nodeName field indexer registered at manager setup time.
// If AgentCountFn is set on the reconciler, it is called instead (used in tests).
func (r *MachineReconciler) countAgentPods(ctx context.Context, machine *kyberv1.Machine) (int, error) {
	if r.AgentCountFn != nil {
		return r.AgentCountFn(ctx, machine)
	}
	if machine.Status.NodeName == "" {
		return 0, nil
	}
	podList := &corev1.PodList{}
	if err := r.List(ctx, podList,
		client.InNamespace(""), // all namespaces
		client.MatchingFields{"spec.nodeName": machine.Status.NodeName},
	); err != nil {
		// Return the error so the reconciler can requeue rather than acting on stale data.
		// Swallowing the error could cause classifyRunning to emit EventAllAgentsRemoved
		// and drop the machine from Running to Ready even though agents still exist.
		logger := log.FromContext(ctx)
		logger.Error(err, "listing agent pods", "machine", machine.Name, "node", machine.Status.NodeName)
		return 0, fmt.Errorf("listing agent pods for machine %s: %w", machine.Name, err)
	}
	count := 0
	for i := range podList.Items {
		if _, ok := podList.Items[i].Labels[AgentLabelKey]; ok {
			count++
		}
	}
	return count, nil
}

// HandlePreemptionNotice is called by the internal API when a node agent
// detects imminent preemption. It annotates all running agents on the machine
// with "kyber.dev/preemption-notice" so the agent reconciler fires
// EventPreemptionNotice on the next reconcile.
func (r *MachineReconciler) HandlePreemptionNotice(ctx context.Context, machineName, instanceId string) {
	logger := log.FromContext(ctx).WithValues("machine", machineName)
	logger.Info("preemption notice received", "instanceId", instanceId)

	// List agents assigned to this machine.
	var agents kyberv1.AgentList
	if err := r.Client.List(ctx, &agents,
		client.InNamespace(r.Namespace),
	); err != nil {
		logger.Error(err, "failed to list agents for preemption drain")
		return
	}

	// Annotate each running/starting agent to trigger drain.
	for i := range agents.Items {
		agent := &agents.Items[i]
		if agent.Spec.Machine != machineName {
			continue
		}
		if agent.Status.Phase != kyberv1.AgentPhaseRunning &&
			agent.Status.Phase != kyberv1.AgentPhaseStarting {
			continue
		}
		if agent.Annotations == nil {
			agent.Annotations = make(map[string]string)
		}
		agent.Annotations["kyber.dev/preemption-notice"] = time.Now().UTC().Format(time.RFC3339)
		if err := r.Client.Update(ctx, agent); err != nil {
			logger.Error(err, "failed to annotate agent for drain", "agent", agent.Name)
		} else {
			logger.Info("annotated agent for preemption drain", "agent", agent.Name)
		}
	}
}

// buildMachineSpec converts a Machine CRD into an adapters.MachineSpec for the compute adapter.
// K3sJoinToken and K3sServerURL are read from env vars at reconciler startup (wired via Helm
// Secret in D1) and stored on the reconciler struct. The mock adapter ignores them; the GCE
// adapter uses them to pass k3s join parameters to the VM startup script.
func (r *MachineReconciler) buildMachineSpec(machine *kyberv1.Machine) adapters.MachineSpec {
	return adapters.MachineSpec{
		Name:          machine.Name,
		Profile:       machine.Spec.MachineType,
		DiskSizeGb:    int(machine.Spec.DiskSizeGb),
		Interruptible: machine.Spec.Spot,
		Location:      machine.Spec.Zone,
		JoinToken:     r.K3sJoinToken,
		ServerURL:     r.K3sServerURL,
		Labels: map[string]string{
			"kyber-machine": machine.Name,
		},
	}
}

// fireAlert sends an alert to the configured AlertSink. Errors are logged but
// never returned — alert delivery must not disrupt the reconcile loop.
func (r *MachineReconciler) fireAlert(ctx context.Context, machine *kyberv1.Machine, severity, reason string, details map[string]string) {
	if r.AlertSink == nil {
		return
	}
	logger := log.FromContext(ctx)
	if err := r.AlertSink.Fire(ctx, telemetry.Alert{
		Severity: severity,
		Kind:     "machine",
		Name:     machine.Name,
		Reason:   reason,
		Details:  details,
	}); err != nil {
		logger.Error(err, "alert sink error (non-fatal)", "machine", machine.Name, "reason", reason)
	}
}

// isNodeReady returns true if the node has the Ready condition set to True.
func isNodeReady(node *corev1.Node) bool {
	for _, cond := range node.Status.Conditions {
		if cond.Type == corev1.NodeReady {
			return cond.Status == corev1.ConditionTrue
		}
	}
	return false
}

func (r *MachineReconciler) isManagedNodeReady(node *corev1.Node) bool {
	if r.AllowSimulatedNodes && node.Labels["kyber.io/simulated"] == "true" {
		return true
	}
	return isNodeReady(node)
}

// selectMockNode resolves the node a provider=mock Machine attaches to.
//
// Tier 1: a node explicitly labelled kyber.io/machine=<machine-name>. This
// is deterministic and survives re-reconciles, so it is the correct choice
// on any cluster with more than one node.
//
// Tier 2: the first Ready node, preserving the original standalone
// behaviour for single-node installs that carry no such label.
//
// Returns (nil, nil) when no node is eligible yet — the caller requeues.
// A labelled-but-NotReady node deliberately does NOT fall through to tier
// 2: the operator has stated which node this Machine belongs to, and
// silently migrating agent pods to a different node while that one is
// briefly NotReady would be worse than waiting for it to come back.
func (r *MachineReconciler) selectMockNode(ctx context.Context, m *kyberv1.Machine) (*corev1.Node, error) {
	labelled, err := r.getNodeForMachine(ctx, m)
	if err != nil {
		return nil, err
	}
	if labelled != nil {
		if !isNodeReady(labelled) {
			return nil, nil
		}
		return labelled, nil
	}

	var nodes corev1.NodeList
	if err := r.Client.List(ctx, &nodes); err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}
	for i := range nodes.Items {
		// Never let an unlabelled compatibility Machine claim a node that the
		// operator explicitly assigned to another Machine.
		assigned := nodes.Items[i].Labels[MachineLabelKey]
		if assigned == "" && isNodeReady(&nodes.Items[i]) {
			return &nodes.Items[i], nil
		}
	}
	return nil, nil
}

// reconcileStatic is the short-circuit path for static and compatibility mock Machines.
// Standalone installs have exactly one k3s node (the control-plane host
// itself); mock Machines attach to it directly and never invoke a cloud
// adapter.
//
// Node selection is two-tier. An operator may pre-label a node
// kyber.io/machine=<machine-name> to bind this Machine to it explicitly;
// that label wins. Otherwise the first Ready node is used, which is the
// original standalone behaviour and stays the default for single-node
// installs where no label exists.
//
// The explicit tier exists because the fallback is not stable on
// multi-node clusters: the cached client's List returns nodes in
// unspecified order, so "first Ready" can resolve differently between
// reconciles, and this function re-runs every 2 minutes. Since
// status.nodeName is what BuildPodSpec hard-pins agent pods to
// (kubernetes.io/hostname affinity), an unstable pick would migrate the
// Machine's node out from under newly created agent pods. Labelling makes
// the binding deterministic and is what a managed-Kubernetes install
// (GKE, EKS) needs to separate platform nodes from agent nodes. It also
// matches the GCE path, where the VM startup script writes the same label
// and getNodeForMachine reads it.
//
// Since kyber#240 this path also publishes ObservedCapacity /
// AssignableCapacity / AvailableCapacity, sourced directly from the
// chosen node's status.allocatable. Without this, the standalone
// MachineDetail page renders all-zero capacity bars even when the node
// has plenty.
func (r *MachineReconciler) reconcileStatic(ctx context.Context, m *kyberv1.Machine) (ctrl.Result, error) {
	node, err := r.selectMockNode(ctx, m)
	if err != nil {
		return ctrl.Result{}, err
	}
	if node == nil {
		// No Ready node yet — try again soon.
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}
	nodeName := node.Name

	// Compute capacity from this node's allocatable + the controller's
	// platform reservation + agent requests on this machine. Mirrors what
	// updateNodeInfo does on the GCE path; we inline it here because the
	// node may have been chosen by the unlabelled fallback, which
	// getNodeForMachine would not find.
	observed := kyberv1.MachineCapacity{
		CPU:              node.Status.Allocatable[corev1.ResourceCPU],
		Memory:           node.Status.Allocatable[corev1.ResourceMemory],
		EphemeralStorage: node.Status.Allocatable[corev1.ResourceEphemeralStorage],
	}
	assignable := ComputeAssignable(observed, r.PlatformReservation)

	var agentList kyberv1.AgentList
	var available *kyberv1.MachineCapacity
	if err := r.List(ctx, &agentList, client.InNamespace(m.Namespace)); err != nil {
		// Best-effort: leave Available unset. The PWA tolerates a missing
		// availableCapacity (treats it as "capacity unknown for now").
		log.FromContext(ctx).Error(err, "listing agents for AvailableCapacity (continuing)", "machine", m.Name)
	} else {
		a := ComputeAvailable(assignable, agentList.Items, m.Name)
		available = &a
	}

	// Count agents on this machine — the operator UI surfaces this.
	agentCount := int32(0)
	for i := range agentList.Items {
		if agentList.Items[i].Spec.Machine == m.Name {
			agentCount++
		}
	}

	patch := client.MergeFrom(m.DeepCopy())
	changed := false
	if m.Status.Phase != kyberv1.MachinePhaseReady {
		m.Status.Phase = kyberv1.MachinePhaseReady
		now := metav1.Now()
		m.Status.LastTransition = &now
		changed = true
	}
	if m.Status.NodeName != nodeName {
		m.Status.NodeName = nodeName
		changed = true
	}
	// Capacity fields refresh on every reconcile so a node-side resource
	// change (e.g. operator unmounted a disk) shows up promptly. MergeFrom
	// produces an empty patch when nothing actually differs, so this is
	// not a write storm.
	m.Status.ObservedCapacity = &observed
	m.Status.AssignableCapacity = &assignable
	m.Status.AvailableCapacity = available
	if m.Status.AgentCount != agentCount {
		m.Status.AgentCount = agentCount
		changed = true
	}
	// Always patch — capacity fields are pointer-typed and will diff on the
	// first reconcile (going from nil to populated). Subsequent reconciles
	// where nothing changed produce an empty merge patch and don't bump
	// resourceVersion.
	_ = changed
	if err := r.Client.Status().Patch(ctx, m, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("patch mock machine status: %w", err)
	}
	return ctrl.Result{RequeueAfter: 2 * time.Minute}, nil
}

// requeueAfterForPhase returns how long to wait before the next reconcile for a given phase.
// Phases that are actively transitioning use shorter intervals.
func requeueAfterForPhase(phase kyberv1.MachinePhase) time.Duration {
	switch phase {
	case kyberv1.MachinePhaseProvisioning, kyberv1.MachinePhaseReplacing:
		return requeueProvisioning
	case kyberv1.MachinePhaseStopping:
		return requeueStopping
	case kyberv1.MachinePhasePreempted:
		return requeueImmediate
	default:
		// Ready, Running, Stopped, Failed — use the 2-minute resync cadence.
		return 2 * time.Minute
	}
}

// containsString returns true if the slice contains the given string.
func containsString(slice []string, s string) bool {
	for _, v := range slice {
		if v == s {
			return true
		}
	}
	return false
}

// removeString returns a new slice with the given string removed.
func removeString(slice []string, s string) []string {
	result := make([]string, 0, len(slice))
	for _, v := range slice {
		if v != s {
			result = append(result, v)
		}
	}
	return result
}

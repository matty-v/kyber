package agent

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/tokenreport"
)

// Telegram sidecar convergence (kyber#688).
//
// AppendTelegramSidecar runs when a pod is BUILT. Until this file existed that
// was the only place it ran, which meant two things that looked done were not:
//
//  1. An agent migrated off the retired in-process plugin kept running the
//     plugin until something else recreated its pod. The control plane
//     backfilled the Secret, wired the inbound binding and cleared
//     TelegramUnavailable — the Agent CR said "migrated", the pod said "still
//     the plugin", and nothing reconciled the two. Observed in production: `dave`,
//     68 minutes.
//  2. An image.telegramSidecar digest bump never reached a running pod.
//     Observed in production: `r2-d2` served from the pre-convergence bridge — the
//     one without the attachment-volume and port fixes — until a hand-rolled
//     `kubectl delete pod`.
//
// Both self-heal on the next pod recreate for any other reason, which is what
// makes them quiet: bump the pin, see a green pod, and nothing tells you the
// bridge underneath is stale.
//
// The fix is not new machinery. convergeSidecarImage already has the right
// shape — idle gate, one-delete-at-a-time cluster-wide, observed-evidence
// pullability canary. This generalizes the predicates it stands on
// (isContainerImageMismatched / isContainerReady) and reuses the gates verbatim,
// with one addition the status sidecar does not need: CONTAINER PRESENCE is
// drift. A pod with telegramEnabled and no kyber-mcp-telegram container is not
// merely out of date, it is running the wrong implementation of the channel.

// isTelegramSidecarDrifted reports whether pod needs rolling to converge onto
// desiredImage. Absence counts as drift — see isContainerImageMismatched.
func isTelegramSidecarDrifted(pod *corev1.Pod, desiredImage string) bool {
	return isContainerImageMismatched(pod, TelegramSidecarContainerName, desiredImage, true)
}

// isTelegramSidecarReady reports whether the bridge container is running and
// Ready per kubelet — the canary's positive signal.
func isTelegramSidecarReady(pod *corev1.Pod) bool {
	return isContainerReady(pod, TelegramSidecarContainerName)
}

// telegramCanaryWindow returns the active canary window for the Telegram roll —
// caller-configured if TelegramSidecarCanaryWindow > 0, package default
// otherwise. Mirror of canaryWindow / runtimeCanaryWindow.
func (r *AgentReconciler) telegramCanaryWindow() time.Duration {
	if r.TelegramSidecarCanaryWindow > 0 {
		return r.TelegramSidecarCanaryWindow
	}
	return sidecarImageCanaryDefaultWindow
}

// recordTelegramSidecarRollHeld emits a Normal-type Event documenting why a
// Telegram convergence delete was deferred. Single Reason string, same shape as
// recordSidecarImageRollHeld / recordRuntimeImageRollHeld: operators grep one
// Reason and triage from the message.
func (r *AgentReconciler) recordTelegramSidecarRollHeld(agent *kyberv1.Agent, podName, image, detail string) {
	if r.Recorder == nil {
		return
	}
	r.Recorder.Eventf(agent, corev1.EventTypeNormal, "TelegramSidecarRollHeld",
		"Telegram sidecar convergence held for pod %s on image %s: %s; kyber#688",
		podName, image, detail,
	)
}

// convergeTelegramSidecar rolls a running pod onto the Telegram bridge the
// controller currently wants — whether the pod has the wrong image or no bridge
// container at all. Returns (deleted, err); deleted=true means the caller
// requeues and skips the rest of the reconcile, and the next pass rebuilds the
// pod through createPod with the current env.
//
// Gate order, and why each one is where it is:
//
//  1. Channel gate — an agent that never asked for Telegram is never rolled for
//     it. This is checked before drift because for such an agent "no bridge
//     container" is the correct state, not drift.
//  2. Empty-image guard — never delete a pod over an unset env var. Load-bearing
//     for exactly the install this bug was found on: a cluster that has not
//     pinned image.telegramSidecar would otherwise see every Telegram agent
//     become permanently drifted and roll forever, rebuilding an identical
//     bridge-less pod each time. reconcileTelegramCondition already surfaces
//     that install fault as TelegramUnavailable/NoTelegramSidecarImage.
//  3. Drift — wrong image, or (uniquely here) no bridge container at all.
//  4. Allowlist gate — the sidecar refuses to start without an allowlist, so
//     rolling an agent that has none trades a working agent for a crash-looping
//     container and no Telegram either way. Hold instead and leave
//     TelegramUnavailable/NoTelegramAllowlist as the operator's signal. The pod
//     converges the moment the allowlist lands.
//  5. Idle gate — never interrupt an agent the runtime reports as working.
//     Unknown state blocks too: hold a stale bridge rather than roll a pod we
//     cannot characterize.
//  6. Concurrency cap — the SAME countAgentPodsBeingDeleted budget the 5c
//     auto-roll, the 5d status-sidecar convergence and the runtime-image roll
//     share, so all four causes together keep one agent-pod delete in flight
//     cluster-wide.
//  7. Pullability canary — per-image observed evidence. The first eligible
//     delete is the canary; the rest of the Telegram fleet holds until some pod
//     is seen Ready on that image (verified, steady-state convergence) or the
//     window elapses (failed, rolls held behind TelegramSidecarRollHeld until
//     the operator repins or the controller restarts). Containing a bad pin to
//     one agent is the point: draining every Telegram agent at once would take
//     out the channel they would use to tell anyone.
func (r *AgentReconciler) convergeTelegramSidecar(
	ctx context.Context,
	agent *kyberv1.Agent,
	pod *corev1.Pod,
	wiring TelegramWiring,
) (bool, error) {
	if pod == nil || pod.DeletionTimestamp != nil {
		return false, nil
	}
	if !agent.Spec.Secrets.TelegramEnabled {
		return false, nil
	}
	target := r.TelegramSidecarImage
	if target == "" {
		return false, nil
	}
	if !isTelegramSidecarDrifted(pod, target) {
		return false, nil
	}
	if !wiring.HasAllowlist {
		return false, nil
	}
	if agent.Status.Activity == nil || agent.Status.Activity.State != tokenreport.ActivityIdle {
		return false, nil
	}
	inflight, err := r.countAgentPodsBeingDeleted(ctx, agent.Namespace)
	if err != nil {
		return false, fmt.Errorf("counting in-flight pod deletions: %w", err)
	}
	if inflight >= sidecarAutoRollDefaultMaxConcurrent {
		return false, nil
	}
	switch {
	case r.telegramCanary.failedCanary(target):
		r.recordTelegramSidecarRollHeld(agent, pod.Name, target,
			"canary window elapsed without a Ready bridge; operator must verify image.telegramSidecar")
		return false, nil
	case r.telegramCanary.wasVerified(target):
		// Image is proven pullable — steady-state convergence.
	default:
		started, inFlight := r.telegramCanary.canaryInFlight(target)
		switch {
		case inFlight && time.Since(started) > r.telegramCanaryWindow():
			r.telegramCanary.markCanaryFailed(target)
			r.recordTelegramSidecarRollHeld(agent, pod.Name, target,
				"canary window elapsed without a Ready bridge; further rolls held until the pin is fixed")
			return false, nil
		case inFlight:
			// Canary still mid-window — hold the rest of the fleet silently.
			return false, nil
		default:
			// No canary attempt yet for this image — THIS pod is the canary.
			r.telegramCanary.markCanaryStarted(target)
		}
	}
	had := extractContainerSpecImage(pod, TelegramSidecarContainerName)
	if had == "" {
		had = "<no bridge container>"
	}
	if err := r.Delete(ctx, pod); err != nil {
		if errors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("deleting pod for Telegram sidecar convergence: %w", err)
	}
	if r.Recorder != nil {
		r.Recorder.Eventf(agent, corev1.EventTypeNormal, "TelegramSidecarConverge",
			"deleted pod %s to converge the Telegram bridge (had %s, expected %s); kyber#688",
			pod.Name, had, target,
		)
	}
	return true, nil
}

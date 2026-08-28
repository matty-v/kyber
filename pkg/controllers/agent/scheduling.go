// Pod-event → Agent.status.scheduling population (kyber#210 PR-A).
//
// The agent reconciler's existing path treats a Pending pod as "still
// starting, give it more time." That works for the happy 5-15s the
// scheduler + kubelet need to bind + pull. It silently breaks down when
// the pod will NEVER schedule (node OOM, image gone, taint mismatch,
// PVC binding broken). Operators got no PWA signal — the agent CR sat
// in Starting indefinitely and the only ground truth lived in
// `kubectl describe pod`.
//
// This file pulls those Pod events up to Agent.status.scheduling so the
// PWA banner (PR-B) can surface them. Two helpers:
//
//   populateSchedulingStatus(ctx, pod, agent) — called from the
//     reconciler when phase=Starting and pod=Pending. Lists Events for
//     the pod, classifies the failure, writes the status. No-ops within
//     the schedulingGracePeriod so a normal cold-start doesn't trip it.
//
//   classifySchedulingCategory(reason, message) — pure-function category
//     classifier. Spec-aligned enum: Capacity | Placement | Image |
//     Storage | Other.

package agent

import (
	"context"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
)

// schedulingGracePeriod is how long a Pending pod is "still starting" before
// we treat scheduler / kubelet failures as worth surfacing. ~30s covers the
// normal cold-start window (image pull on a fresh node + PVC bind + pod
// scheduling) so we don't false-positive on routine boots.
const schedulingGracePeriod = 30 * time.Second

// stalledFallbackGracePeriod is how long a Pending pod may sit with NO
// classified event before we report what we can see anyway. Much longer than
// schedulingGracePeriod on purpose: the classified reasons are definitive the
// moment they appear, while "nothing has happened yet" is normal for minutes
// on a cold node pulling a large runtime image. Ten minutes is past any
// healthy boot observed on the fleet and far short of the eleven hours a test
// agent sat in Creating on kyber-canary with an entirely blank status.
const stalledFallbackGracePeriod = 10 * time.Minute

// Event reasons we care about for scheduling-status. Matches the issue's
// scope (capacity / placement / image / storage). Anything else is left to
// the existing reconciler logic.
var schedulingEventReasons = map[string]struct{}{
	"FailedScheduling":   {},
	"FailedMount":        {},
	"ImagePullBackOff":   {},
	"ErrImagePull":       {},
	"InvalidImageName":   {},
	"FailedAttachVolume": {},
}

// populateSchedulingStatus reads the recent Events for pod and, if any
// scheduling-relevant event has been observed past the grace window,
// writes a status entry to agent.Status.Scheduling. Returns true when
// the status was written (caller should patch the agent), false when no
// change is needed (within grace, no matching event, or the existing
// status is already up-to-date).
//
// The caller is responsible for the actual patch — keeping the patch in
// the reconcile loop's existing flow lets us batch multiple field
// updates into one Status().Patch call.
func populateSchedulingStatus(ctx context.Context, c client.Client, pod *corev1.Pod, agent *kyberv1.Agent) bool {
	// Don't false-positive on a routine cold start. Pod creation timestamp
	// is the right anchor — phase transitions can flap during boot.
	if time.Since(pod.CreationTimestamp.Time) < schedulingGracePeriod {
		return false
	}

	// List events in the pod's namespace. Filter to this pod's UID +
	// known scheduling reasons in Go — there are typically <50 events
	// per namespace per minute and we hit this path only when a pod is
	// actually stuck, so the overhead is negligible compared to wiring
	// a field-selector indexer just for this one helper.
	var events corev1.EventList
	if err := c.List(ctx, &events, client.InNamespace(pod.Namespace)); err != nil {
		// Log + bail — scheduling-status is best-effort; we should never
		// block the main reconcile path on the Events API being slow.
		return false
	}

	var latest *corev1.Event
	for i := range events.Items {
		ev := &events.Items[i]
		if ev.InvolvedObject.UID != pod.UID {
			continue
		}
		if _, ok := schedulingEventReasons[ev.Reason]; !ok {
			continue
		}
		// Pick the most-recent matching event. LastTimestamp is the
		// canonical "still happening" marker for cumulative events;
		// fall back to EventTime + CreationTimestamp for events that
		// don't set LastTimestamp.
		if latest == nil || eventTime(ev).After(eventTime(latest)) {
			latest = ev
		}
	}

	if latest == nil {
		// No matching event. Either the pod is genuinely still booting, or
		// it is stuck in a way this file does not classify — a container
		// that simply never starts emits no event at all, and only six
		// reasons are recognised above.
		//
		// Saying nothing is the worse failure. An operator looking at an
		// agent stuck for hours got an unchanging phase and an empty
		// reason, so the only ground truth was `kubectl describe pod` on
		// the box — exactly the gap this file was written to close, left
		// open for every failure outside the six. Report what is
		// observable instead, once it is clearly not a slow boot.
		//
		// Never overwrite a classified entry: a real reason always beats
		// this one, and clearing still happens only on the Running
		// transition.
		if agent.Status.Scheduling != nil ||
			time.Since(pod.CreationTimestamp.Time) < stalledFallbackGracePeriod {
			return false
		}
		now := metav1.NewTime(time.Now().UTC())
		agent.Status.Scheduling = &kyberv1.AgentSchedulingStatus{
			Category:        "Other",
			LastError:       describeStalledPod(pod),
			FirstObservedAt: &now,
		}
		return true
	}

	category := classifySchedulingCategory(latest.Reason, latest.Message)

	// Idempotency: skip the patch when the existing status already names
	// the same category + message. Avoids churning ResourceVersion on
	// every reconcile while a pod is stuck.
	if agent.Status.Scheduling != nil &&
		agent.Status.Scheduling.Category == category &&
		agent.Status.Scheduling.LastError == latest.Message {
		return false
	}

	// Preserve FirstObservedAt across category changes when the prior
	// entry was already present — operators care about "how long has
	// this been broken", not "when did the controller most recently
	// notice." Reset only when there's no prior entry.
	firstObserved := metav1.NewTime(time.Now().UTC())
	if agent.Status.Scheduling != nil && agent.Status.Scheduling.FirstObservedAt != nil {
		firstObserved = *agent.Status.Scheduling.FirstObservedAt
	}

	agent.Status.Scheduling = &kyberv1.AgentSchedulingStatus{
		Category:        category,
		LastError:       latest.Message,
		FirstObservedAt: &firstObserved,
	}
	return true
}

// describeStalledPod renders what can be seen about a Pod that has made no
// progress and produced no classified event: its phase, and which containers
// have not started, with whatever reason the kubelet did record on them.
//
// Deliberately descriptive rather than diagnostic. It cannot say WHY, because
// nothing in the cluster said why — but "container agent has not started
// (PodInitializing)" tells an operator where to look, and an empty banner
// tells them nothing at all.
func describeStalledPod(pod *corev1.Pod) string {
	var stalled []string
	for _, cs := range append(append([]corev1.ContainerStatus{},
		pod.Status.InitContainerStatuses...), pod.Status.ContainerStatuses...) {
		if cs.State.Running != nil {
			continue
		}
		if cs.State.Terminated != nil && cs.State.Terminated.ExitCode == 0 {
			continue
		}
		detail := "no state reported"
		if w := cs.State.Waiting; w != nil {
			detail = w.Reason
			if w.Message != "" {
				detail += ": " + w.Message
			}
		} else if t := cs.State.Terminated; t != nil {
			detail = t.Reason
		}
		stalled = append(stalled, "container "+cs.Name+" has not started ("+detail+")")
	}

	msg := "Pod has been " + string(pod.Status.Phase) +
		" since " + pod.CreationTimestamp.UTC().Format(time.RFC3339) +
		" with no scheduler or kubelet failure recorded."
	if len(stalled) > 0 {
		msg += " " + strings.Join(stalled, "; ") + "."
	}
	return msg
}

// clearSchedulingStatus drops the status entry. Caller should invoke this
// once the pod transitions to Running so the PWA banner clears. Returns
// true when a clear actually happened (caller should patch).
func clearSchedulingStatus(agent *kyberv1.Agent) bool {
	if agent.Status.Scheduling == nil {
		return false
	}
	agent.Status.Scheduling = nil
	return true
}

// classifySchedulingCategory maps a Pod event (reason + message) into the
// PWA-facing enum. Order matters: image / storage / placement reasons
// are checked before "Capacity" because the FailedScheduling reason can
// fire for any of the categories — its message is what disambiguates.
//
// Pure function; safe to call from tests + future debug surfaces.
func classifySchedulingCategory(reason, message string) string {
	msg := strings.ToLower(message)
	switch reason {
	case "ImagePullBackOff", "ErrImagePull", "InvalidImageName":
		return "Image"
	case "FailedMount", "FailedAttachVolume":
		return "Storage"
	}
	// FailedScheduling — disambiguate by message content.
	if strings.Contains(msg, "insufficient memory") ||
		strings.Contains(msg, "insufficient cpu") ||
		strings.Contains(msg, "insufficient ephemeral-storage") ||
		strings.Contains(msg, "insufficient pods") {
		return "Capacity"
	}
	if strings.Contains(msg, "untolerated taint") ||
		strings.Contains(msg, "didn't match pod's node affinity") ||
		strings.Contains(msg, "didn't match node selector") {
		return "Placement"
	}
	if strings.Contains(msg, "volume node affinity conflict") ||
		strings.Contains(msg, "unbound immediate persistentvolumeclaims") ||
		strings.Contains(msg, "had volume node affinity conflict") {
		return "Storage"
	}
	return "Other"
}

// eventTime returns the most informative timestamp from an Event. Events
// emitted by the scheduler set LastTimestamp; events from kubelet
// (newer style) set EventTime. Fall through to CreationTimestamp.
func eventTime(e *corev1.Event) time.Time {
	if !e.LastTimestamp.IsZero() {
		return e.LastTimestamp.Time
	}
	if !e.EventTime.IsZero() {
		return e.EventTime.Time
	}
	return e.CreationTimestamp.Time
}

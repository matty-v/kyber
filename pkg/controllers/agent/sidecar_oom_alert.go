// Sidecar OOM/flap alerting (kyber#584 Phase C). The agent pod's injected
// native sidecars (transcript-tailer, kyber-status-sidecar — kyber#575) run
// under RestartPolicy:Always, so the kubelet auto-restarts one that OOMKills.
// That self-heal is exactly what MASKED the transcript-tailer's file-count
// memory regression for hours (the #575 masking that hid #584 until the AC7
// re-check): an OOM-looping sidecar "looks healthy" because it keeps coming
// back. This file surfaces that condition as an alert on the existing path
// (fireAlert -> WebhookAlertSink -> Echo Base / Telegram) so a sidecar memory
// regression can no longer hide behind auto-restart.
//
// The agent-container OOM path (isOOMKilled / hasRecentKernelOOMKill, kyber#272/
// #285) is untouched — that drives the state machine. This is a separate,
// best-effort observability signal for the SIDECARS, which the state machine
// doesn't track.

package agent

import (
	"fmt"
	"sync"

	corev1 "k8s.io/api/core/v1"
)

// sidecarOOMRestartThreshold is the restartCount at/above which a monitored
// sidecar's flapping is treated as a memory regression worth alerting. Mirrors
// the agent's own retry posture (maxRestartRetries = 3): a blip or two
// self-heals, but a sustained climb is the #575-masked OOM loop surfacing. A
// package const (not a chart value) keeps the contract ADDITIVE — no new Helm
// key (kyber#584 is a no-config-change fix).
const sidecarOOMRestartThreshold = int32(3)

// monitoredSidecarNames are the injected native sidecars (kyber#575) whose
// OOM/flap must not be silently masked by RestartPolicy:Always auto-restart.
var monitoredSidecarNames = map[string]bool{
	TranscriptTailerContainerName: true, // "transcript-tailer" (kyber#446)
	StatusSidecarContainerName:    true, // "kyber-status-sidecar" (kyber#248)
	SessionSaverContainerName:     true, // "session-saver" — on-by-default native sidecar that parses JSON (jq); its OOM/flap must not hide behind auto-restart either
}

// containerStatusOOMKilled reports whether a container's CURRENT or LAST
// termination was tagged OOMKilled by the kubelet. Unlike the agent-container
// isOOMKilled (which intentionally inspects only the CURRENT termination), a
// flapping native sidecar has usually already restarted, so its OOM lives in
// LastTerminationState — both are checked here.
func containerStatusOOMKilled(cs corev1.ContainerStatus) bool {
	if cs.State.Terminated != nil && cs.State.Terminated.Reason == "OOMKilled" {
		return true
	}
	if cs.LastTerminationState.Terminated != nil && cs.LastTerminationState.Terminated.Reason == "OOMKilled" {
		return true
	}
	return false
}

// sidecarOOMOrFlapping scans the pod's monitored sidecars and reports whether any
// is OOM-killed or flapping at/above threshold, returning the offending
// sidecar's restartCount (the dedup key) and the alert details. Native sidecars
// (kyber#575) live in InitContainerStatuses; ContainerStatuses is scanned too
// for forward-compat. The worst offender (highest restartCount, OOM preferred on
// a tie) drives the result so the single dedup key tracks the escalation.
func sidecarOOMOrFlapping(pod *corev1.Pod, threshold int32) (fire bool, restartCount int32, details map[string]string) {
	if pod == nil {
		return false, 0, nil
	}

	var (
		bestName  string
		bestCount int32 = -1
		bestOOM   bool
		found     bool
	)
	consider := func(cs corev1.ContainerStatus) {
		if !monitoredSidecarNames[cs.Name] {
			return
		}
		oom := containerStatusOOMKilled(cs)
		flapping := cs.RestartCount >= threshold
		if !oom && !flapping {
			return
		}
		// Prefer the higher restartCount; break ties toward an OOM (a more
		// actionable signal than a plain restart climb).
		better := cs.RestartCount > bestCount || (cs.RestartCount == bestCount && oom && !bestOOM)
		if !found || better {
			found = true
			bestName = cs.Name
			bestCount = cs.RestartCount
			bestOOM = oom
		}
	}
	for i := range pod.Status.InitContainerStatuses {
		consider(pod.Status.InitContainerStatuses[i])
	}
	for i := range pod.Status.ContainerStatuses {
		consider(pod.Status.ContainerStatuses[i])
	}
	if !found {
		return false, 0, nil
	}

	condition := "flapping"
	if bestOOM {
		condition = "OOMKilled"
	}
	return true, bestCount, map[string]string{
		"sidecar":      bestName,
		"restartCount": fmt.Sprintf("%d", bestCount),
		"condition":    condition,
		"threshold":    fmt.Sprintf("%d", threshold),
	}
}

// sidecarAlertTracker dedups Phase C alerts so a flapping sidecar fires once per
// escalation rather than every reconcile. Keyed per agent on the restartCount
// last alerted at: a fresh alert fires only when the count climbs past what was
// last reported; a pod recreation that resets the count below re-arms it. State
// is in-memory and best-effort — re-initialized on controller restart (which
// re-arms against the current pod), exactly like the image canaries.
type sidecarAlertTracker struct {
	mu     sync.Mutex
	lastAt map[string]int32
}

// shouldFire reports whether an alert should fire for agentName at the given
// offending restartCount, recording it as the new high-water mark. Call only
// when sidecarOOMOrFlapping already returned fire==true.
func (s *sidecarAlertTracker) shouldFire(agentName string, restartCount int32) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastAt == nil {
		s.lastAt = make(map[string]int32)
	}
	prev, seen := s.lastAt[agentName]
	if seen && restartCount <= prev {
		// Already alerted at this level, or the pod was recreated and the count
		// reset below — track the lower value so a later climb re-alerts, but
		// don't fire now.
		s.lastAt[agentName] = restartCount
		return false
	}
	s.lastAt[agentName] = restartCount
	return true
}

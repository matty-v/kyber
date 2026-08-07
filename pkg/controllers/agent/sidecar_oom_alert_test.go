package agent

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// initStatus builds a native-sidecar (InitContainers) status with the given
// current/last termination reason and restart count.
func sidecarStatus(name string, restarts int32, currentReason, lastReason string) corev1.ContainerStatus {
	cs := corev1.ContainerStatus{Name: name, RestartCount: restarts}
	if currentReason != "" {
		cs.State.Terminated = &corev1.ContainerStateTerminated{Reason: currentReason, ExitCode: 137}
	}
	if lastReason != "" {
		cs.LastTerminationState.Terminated = &corev1.ContainerStateTerminated{Reason: lastReason, ExitCode: 137}
	}
	return cs
}

func podWithInitStatuses(css ...corev1.ContainerStatus) *corev1.Pod {
	return &corev1.Pod{Status: corev1.PodStatus{InitContainerStatuses: css}}
}

// TestSidecarOOMOrFlapping_Table covers the kyber#584 Phase C detection: a
// monitored sidecar that is OOMKilled (current OR last termination) or flapping
// past threshold fires; a healthy or below-threshold sidecar — and the AGENT
// container, which is handled by the separate state-machine OOM path — does not.
func TestSidecarOOMOrFlapping_Table(t *testing.T) {
	thr := sidecarOOMRestartThreshold
	cases := []struct {
		name     string
		pod      *corev1.Pod
		wantFire bool
		wantName string
		wantCond string
	}{
		{
			name:     "nil pod",
			pod:      nil,
			wantFire: false,
		},
		{
			name:     "healthy tailer",
			pod:      podWithInitStatuses(sidecarStatus(TranscriptTailerContainerName, 0, "", "")),
			wantFire: false,
		},
		{
			name:     "tailer OOM in current termination",
			pod:      podWithInitStatuses(sidecarStatus(TranscriptTailerContainerName, 1, "OOMKilled", "")),
			wantFire: true,
			wantName: TranscriptTailerContainerName,
			wantCond: "OOMKilled",
		},
		{
			name:     "tailer OOM in LAST termination (already auto-restarted — the #575 masking)",
			pod:      podWithInitStatuses(sidecarStatus(TranscriptTailerContainerName, 2, "", "OOMKilled")),
			wantFire: true,
			wantName: TranscriptTailerContainerName,
			wantCond: "OOMKilled",
		},
		{
			name:     "status-sidecar flapping past threshold, no OOM reason",
			pod:      podWithInitStatuses(sidecarStatus(StatusSidecarContainerName, thr, "", "")),
			wantFire: true,
			wantName: StatusSidecarContainerName,
			wantCond: "flapping",
		},
		{
			name:     "below threshold and no OOM does not fire",
			pod:      podWithInitStatuses(sidecarStatus(StatusSidecarContainerName, thr-1, "", "")),
			wantFire: false,
		},
		{
			name: "agent container OOM does NOT fire the sidecar alert (handled by state machine)",
			pod: &corev1.Pod{Status: corev1.PodStatus{
				ContainerStatuses: []corev1.ContainerStatus{sidecarStatus(AgentContainerName, 9, "OOMKilled", "OOMKilled")},
			}},
			wantFire: false,
		},
		{
			name: "worst offender (highest restartCount) drives the result",
			pod: podWithInitStatuses(
				sidecarStatus(TranscriptTailerContainerName, thr, "", ""),
				sidecarStatus(StatusSidecarContainerName, thr+5, "", "OOMKilled"),
			),
			wantFire: true,
			wantName: StatusSidecarContainerName,
			wantCond: "OOMKilled",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fire, rc, details := sidecarOOMOrFlapping(tc.pod, thr)
			if fire != tc.wantFire {
				t.Fatalf("fire = %v, want %v (rc=%d details=%v)", fire, tc.wantFire, rc, details)
			}
			if !tc.wantFire {
				return
			}
			if details["sidecar"] != tc.wantName {
				t.Errorf("sidecar = %q, want %q", details["sidecar"], tc.wantName)
			}
			if details["condition"] != tc.wantCond {
				t.Errorf("condition = %q, want %q", details["condition"], tc.wantCond)
			}
		})
	}
}

// TestSidecarOOMOrFlapping_ScansRegularContainers confirms a monitored sidecar
// found in the regular ContainerStatuses list (forward-compat, if a sidecar were
// ever a non-native container) is detected too.
func TestSidecarOOMOrFlapping_ScansRegularContainers(t *testing.T) {
	pod := &corev1.Pod{Status: corev1.PodStatus{
		ContainerStatuses: []corev1.ContainerStatus{sidecarStatus(TranscriptTailerContainerName, 1, "OOMKilled", "")},
	}}
	fire, _, details := sidecarOOMOrFlapping(pod, sidecarOOMRestartThreshold)
	if !fire || details["sidecar"] != TranscriptTailerContainerName {
		t.Errorf("regular-container sidecar OOM not detected: fire=%v details=%v", fire, details)
	}
}

// TestSidecarAlertTracker_Dedup is the kyber#584 Phase C dedup AC: a flapping
// sidecar alerts ONCE per escalation, not every reconcile — it re-fires only
// when the restartCount climbs, and re-arms when a pod recreation resets it.
func TestSidecarAlertTracker_Dedup(t *testing.T) {
	var tr sidecarAlertTracker
	const agent = "r2-d2"

	if !tr.shouldFire(agent, 3) {
		t.Fatal("first observation at count 3 must fire")
	}
	if tr.shouldFire(agent, 3) {
		t.Error("same count 3 must NOT re-fire (dedup — no per-reconcile spam)")
	}
	if tr.shouldFire(agent, 3) {
		t.Error("count 3 still must not re-fire")
	}
	if !tr.shouldFire(agent, 4) {
		t.Error("a climb to count 4 must re-fire (escalation)")
	}
	if tr.shouldFire(agent, 4) {
		t.Error("count 4 must not re-fire after firing once")
	}
	// Pod recreated — restartCount resets to 0. Must re-arm (not fire on the
	// reset itself), then fire again once it climbs back to threshold.
	if tr.shouldFire(agent, 0) {
		t.Error("a reset to 0 must NOT fire (re-arm only)")
	}
	if !tr.shouldFire(agent, 5) {
		t.Error("after re-arm, a fresh climb must fire again")
	}
}

// TestSidecarAlertTracker_PerAgentIsolation confirms one agent's alert state
// doesn't suppress another's.
func TestSidecarAlertTracker_PerAgentIsolation(t *testing.T) {
	var tr sidecarAlertTracker
	if !tr.shouldFire("alice", 3) {
		t.Fatal("alice first fire")
	}
	if !tr.shouldFire("bob", 3) {
		t.Error("bob must fire independently of alice's state")
	}
}

// TestIsOOMKilled_ContainerName is the kyber#584 generalization guard: the
// predicate now matches the named container (agent caller intact) and inspects
// only the CURRENT termination — a sidecar's last-termination OOM is NOT picked
// up here (that's the separate Phase C path).
func TestIsOOMKilled_ContainerName(t *testing.T) {
	pod := &corev1.Pod{Status: corev1.PodStatus{
		ContainerStatuses: []corev1.ContainerStatus{
			sidecarStatus(AgentContainerName, 1, "OOMKilled", ""),
		},
	}}
	if !isOOMKilled(pod, AgentContainerName) {
		t.Error("agent container current OOM must be detected")
	}
	if isOOMKilled(pod, TranscriptTailerContainerName) {
		t.Error("must not match a different container name")
	}
	// LAST-termination-only OOM must NOT be reported by this CURRENT-only predicate.
	lastOnly := &corev1.Pod{Status: corev1.PodStatus{
		ContainerStatuses: []corev1.ContainerStatus{sidecarStatus(AgentContainerName, 2, "", "OOMKilled")},
	}}
	if isOOMKilled(lastOnly, AgentContainerName) {
		t.Error("isOOMKilled must inspect only CURRENT termination (kyber#272 semantic preserved)")
	}
	if isOOMKilled(nil, AgentContainerName) {
		t.Error("nil pod must be false")
	}
}

package agent

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// kyber#575: the three pod sidecars (kyber-status-sidecar, transcript-tailer,
// transcript-pruner) are promoted from regular containers to k8s NATIVE
// sidecars — entries in spec.InitContainers carrying RestartPolicy:Always — so
// the kubelet restarts each on ANY exit independently of the pod-level
// RestartPolicy:Never. These tests are the regression guard for that contract:
// placement (Init list, not Containers), the restart policy, and the
// preservation of the pod-level Never (the #563 agent-container contract).

// findInitContainer returns the named init container, or nil.
func findInitContainer(spec *corev1.PodSpec, name string) *corev1.Container {
	for i := range spec.InitContainers {
		if spec.InitContainers[i].Name == name {
			return &spec.InitContainers[i]
		}
	}
	return nil
}

// mustStatusSidecar returns the status-sidecar from the Init list (kyber#575),
// failing the test if it is absent.
func mustStatusSidecar(t *testing.T, spec *corev1.PodSpec) corev1.Container {
	t.Helper()
	ic := findInitContainer(spec, StatusSidecarContainerName)
	if ic == nil {
		t.Fatalf("status-sidecar not found in spec.InitContainers")
	}
	return *ic
}

// mustInitContainerByName returns the named init container, failing if absent.
func mustInitContainerByName(t *testing.T, spec *corev1.PodSpec, name string) corev1.Container {
	t.Helper()
	ic := findInitContainer(spec, name)
	if ic == nil {
		t.Fatalf("%s not found in spec.InitContainers", name)
	}
	return *ic
}

// findContainer returns the named regular container, or nil.
func findContainer(spec *corev1.PodSpec, name string) *corev1.Container {
	for i := range spec.Containers {
		if spec.Containers[i].Name == name {
			return &spec.Containers[i]
		}
	}
	return nil
}

func TestStatusSidecar_IsNativeSidecar(t *testing.T) {
	spec := &corev1.PodSpec{
		Containers: []corev1.Container{{Name: AgentContainerName}},
	}
	AppendStatusSidecar(spec, SidecarConfig{AgentName: "alice", Image: "img:v1"})

	if c := findContainer(spec, StatusSidecarContainerName); c != nil {
		t.Fatalf("status-sidecar must NOT be a regular container; found it in spec.Containers")
	}
	ic := findInitContainer(spec, StatusSidecarContainerName)
	if ic == nil {
		t.Fatalf("status-sidecar must be a native sidecar in spec.InitContainers; not found there")
	}
	if ic.RestartPolicy == nil || *ic.RestartPolicy != corev1.ContainerRestartPolicyAlways {
		t.Errorf("status-sidecar RestartPolicy: got %v, want Always (native sidecar)", ic.RestartPolicy)
	}
}

// TestStatusSidecar_PrependedAheadOfSetup pins the design's ordering choice:
// the heartbeat must be live DURING the git-clone "setup" init container, so the
// status-sidecar is prepended to the front of InitContainers.
func TestStatusSidecar_PrependedAheadOfSetup(t *testing.T) {
	spec := &corev1.PodSpec{
		InitContainers: []corev1.Container{{Name: "setup"}},
		Containers:     []corev1.Container{{Name: AgentContainerName}},
	}
	AppendStatusSidecar(spec, SidecarConfig{AgentName: "alice", Image: "img:v1"})

	if len(spec.InitContainers) == 0 || spec.InitContainers[0].Name != StatusSidecarContainerName {
		t.Fatalf("status-sidecar must be PREPENDED (front of InitContainers, ahead of setup); order=%v", initNames(spec))
	}
	if findInitContainer(spec, "setup") == nil {
		t.Errorf("prepend must preserve the existing setup init container")
	}
}

func TestTranscriptTailer_IsNativeSidecar(t *testing.T) {
	spec := &corev1.PodSpec{
		Containers: []corev1.Container{{Name: AgentContainerName}},
	}
	AppendTranscriptTailer(spec, TranscriptTailerConfig{AgentName: "alice", RuntimeImage: "img:v1"})

	if c := findContainer(spec, TranscriptTailerContainerName); c != nil {
		t.Fatalf("transcript-tailer must NOT be a regular container")
	}
	ic := findInitContainer(spec, TranscriptTailerContainerName)
	if ic == nil {
		t.Fatalf("transcript-tailer must be a native sidecar in spec.InitContainers")
	}
	if ic.RestartPolicy == nil || *ic.RestartPolicy != corev1.ContainerRestartPolicyAlways {
		t.Errorf("transcript-tailer RestartPolicy: got %v, want Always", ic.RestartPolicy)
	}
}

func TestTranscriptPruner_IsNativeSidecar(t *testing.T) {
	spec := &corev1.PodSpec{
		Containers: []corev1.Container{{Name: AgentContainerName}},
	}
	AppendTranscriptPruner(spec, TranscriptPrunerConfig{
		AgentName:    "alice",
		RuntimeImage: "img:v1",
		Enabled:      true,
		MaxAgeDays:   7,
	})

	if c := findContainer(spec, TranscriptPrunerContainerName); c != nil {
		t.Fatalf("transcript-pruner must NOT be a regular container")
	}
	ic := findInitContainer(spec, TranscriptPrunerContainerName)
	if ic == nil {
		t.Fatalf("transcript-pruner must be a native sidecar in spec.InitContainers")
	}
	if ic.RestartPolicy == nil || *ic.RestartPolicy != corev1.ContainerRestartPolicyAlways {
		t.Errorf("transcript-pruner RestartPolicy: got %v, want Always", ic.RestartPolicy)
	}
}

// TestNativeSidecars_PodLevelRestartPolicyStaysNever is the #563-contract guard:
// the AC requires the pod-level RestartPolicy to remain Never even though the
// sidecars now self-heal. Build the full spec and assert it.
func TestNativeSidecars_PodLevelRestartPolicyStaysNever(t *testing.T) {
	t.Setenv("KYBER_CONTROL_PLANE_INTERNAL_URL", "http://kyber-control-plane.kyber-system:8082")
	agent := testAgent()
	adapter := testAdapter()

	spec, err := BuildPodSpec(agent, adapter, "node-01")
	if err != nil {
		t.Fatalf("BuildPodSpec: %v", err)
	}
	AppendStatusSidecar(&spec, SidecarConfig{AgentName: agent.Name, Image: "img:v1"})
	AppendTranscriptTailer(&spec, TranscriptTailerConfig{AgentName: agent.Name, RuntimeImage: spec.Containers[0].Image})
	AppendTranscriptPruner(&spec, TranscriptPrunerConfig{
		AgentName: agent.Name, RuntimeImage: spec.Containers[0].Image, Enabled: true, MaxAgeDays: 7,
	})

	if spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("pod-level RestartPolicy: got %q, want Never (#563 contract preserved)", spec.RestartPolicy)
	}
	// The agent container stays the sole regular container at index 0 — read-sites
	// (Containers[0].Image, the 'agent' death predicates) depend on this.
	if len(spec.Containers) != 1 || spec.Containers[0].Name != AgentContainerName {
		t.Errorf("agent must remain the sole regular container at index 0; got %v", containerNames(&spec))
	}
	for _, name := range []string{StatusSidecarContainerName, TranscriptTailerContainerName, TranscriptPrunerContainerName} {
		if findInitContainer(&spec, name) == nil {
			t.Errorf("native sidecar %q missing from InitContainers", name)
		}
	}
}

// --- read-site fan-out (kyber#575): the 2 controller paths that read the
// status-sidecar must now find it in the Init lists, or they silently regress.

// TestExtractSidecarSpecImage_FindsNativeSidecar is the #299-convergence guard:
// once the sidecar is a native sidecar, its spec lives in Spec.InitContainers,
// and extractSidecarSpecImage must read it there — else isSidecarSpecMismatched
// can no longer drive tag-pinned helm convergence.
func TestExtractSidecarSpecImage_FindsNativeSidecar(t *testing.T) {
	const img = "ghcr.io/matty-v/kyber-status-sidecar:v1.4.0"
	pod := &corev1.Pod{Spec: corev1.PodSpec{
		Containers: []corev1.Container{{Name: AgentContainerName, Image: "runtime:v1"}},
		InitContainers: []corev1.Container{
			{Name: StatusSidecarContainerName, Image: img},
			{Name: "setup"},
		},
	}}
	if got := extractSidecarSpecImage(pod); got != img {
		t.Errorf("extractSidecarSpecImage (sidecar in InitContainers): got %q, want %q", got, img)
	}
}

// TestExtractSidecarSpecImage_StillFindsLegacyRegularContainer is the rollover
// guard: pods built by a pre-#575 controller still carry the sidecar as a
// regular container until they are recreated. Convergence must keep working on
// them, so the read-site scans both lists.
func TestExtractSidecarSpecImage_StillFindsLegacyRegularContainer(t *testing.T) {
	const img = "ghcr.io/matty-v/kyber-status-sidecar:v1.0.0"
	pod := &corev1.Pod{Spec: corev1.PodSpec{
		Containers: []corev1.Container{
			{Name: AgentContainerName, Image: "runtime:v1"},
			{Name: StatusSidecarContainerName, Image: img},
		},
	}}
	if got := extractSidecarSpecImage(pod); got != img {
		t.Errorf("extractSidecarSpecImage (legacy regular container): got %q, want %q", got, img)
	}
}

// TestIsSidecarReady_FindsNativeSidecarStatus is the #371-canary guard: a native
// sidecar's runtime status reports under InitContainerStatuses, so isSidecarReady
// must scan there or the image-pullability canary loses its positive signal.
func TestIsSidecarReady_FindsNativeSidecarStatus(t *testing.T) {
	pod := &corev1.Pod{Status: corev1.PodStatus{
		InitContainerStatuses: []corev1.ContainerStatus{{
			Name:  StatusSidecarContainerName,
			Ready: true,
			State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
		}},
	}}
	if !isSidecarReady(pod) {
		t.Error("isSidecarReady must read InitContainerStatuses for a native sidecar; got false")
	}
}

// TestIsSidecarReady_NativeSidecarNotRunning — a native sidecar present but not
// yet Running/Ready is not a positive signal.
func TestIsSidecarReady_NativeSidecarNotRunning(t *testing.T) {
	pod := &corev1.Pod{Status: corev1.PodStatus{
		InitContainerStatuses: []corev1.ContainerStatus{{
			Name:  StatusSidecarContainerName,
			Ready: false,
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{}},
		}},
	}}
	if isSidecarReady(pod) {
		t.Error("isSidecarReady must be false for a not-yet-running native sidecar")
	}
}

// TestIsSidecarReady_StillFindsLegacyRegularStatus is the rollover guard for the
// canary: pre-#575 pods report the sidecar under ContainerStatuses.
func TestIsSidecarReady_StillFindsLegacyRegularStatus(t *testing.T) {
	pod := &corev1.Pod{Status: corev1.PodStatus{
		ContainerStatuses: []corev1.ContainerStatus{{
			Name:  StatusSidecarContainerName,
			Ready: true,
			State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
		}},
	}}
	if !isSidecarReady(pod) {
		t.Error("isSidecarReady must still read ContainerStatuses for a legacy regular-container sidecar")
	}
}

func initNames(spec *corev1.PodSpec) []string {
	out := make([]string, 0, len(spec.InitContainers))
	for _, c := range spec.InitContainers {
		out = append(out, c.Name)
	}
	return out
}

func containerNames(spec *corev1.PodSpec) []string {
	out := make([]string, 0, len(spec.Containers))
	for _, c := range spec.Containers {
		out = append(out, c.Name)
	}
	return out
}

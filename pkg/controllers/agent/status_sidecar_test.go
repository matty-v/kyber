package agent

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestAppendStatusSidecar_NoOpWhenImageEmpty(t *testing.T) {
	spec := &corev1.PodSpec{
		Containers: []corev1.Container{{Name: "agent"}},
	}
	AppendStatusSidecar(spec, SidecarConfig{AgentName: "alice", Image: ""})
	if len(spec.Containers) != 1 {
		t.Errorf("empty image must not inject; got %d containers", len(spec.Containers))
	}
}

func TestAppendStatusSidecar_AppendsContainer(t *testing.T) {
	spec := &corev1.PodSpec{
		Containers: []corev1.Container{{Name: "agent"}},
	}
	AppendStatusSidecar(spec, SidecarConfig{AgentName: "alice", Image: "ghcr.io/matty-v/kyber-status-sidecar:v1"})
	// kyber#575: the sidecar is now a native sidecar in InitContainers, so the
	// regular Containers slice is unchanged (still just the agent).
	if len(spec.Containers) != 1 {
		t.Fatalf("regular containers must be unchanged (agent only); got %d", len(spec.Containers))
	}
	side := mustStatusSidecar(t, spec)
	if side.Name != StatusSidecarContainerName {
		t.Errorf("container name: got %q, want %q", side.Name, StatusSidecarContainerName)
	}
	if side.Image != "ghcr.io/matty-v/kyber-status-sidecar:v1" {
		t.Errorf("image: got %q, want pinned ref", side.Image)
	}
}

func TestAppendStatusSidecar_EnvVarsMatchTokenReporterConvention(t *testing.T) {
	spec := &corev1.PodSpec{}
	AppendStatusSidecar(spec, SidecarConfig{AgentName: "alice", Image: "img:v1", DiskBytes: 2 * 1024 * 1024 * 1024})
	side := mustStatusSidecar(t, spec)
	envByName := map[string]string{}
	for _, e := range side.Env {
		envByName[e.Name] = e.Value
	}
	if envByName["AGENT_NAME"] != "alice" {
		t.Errorf("AGENT_NAME: got %q, want alice", envByName["AGENT_NAME"])
	}
	if _, ok := envByName["KYBER_CONTROL_PLANE_INTERNAL_URL"]; !ok {
		t.Error("KYBER_CONTROL_PLANE_INTERNAL_URL not set")
	}
	if got := envByName["KYBER_AGENT_DISK_BYTES"]; got != "2147483648" {
		t.Errorf("KYBER_AGENT_DISK_BYTES: got %q, want 2147483648", got)
	}
}

func TestAppendStatusSidecar_ConfiguresTaskResultsRoot(t *testing.T) {
	spec := &corev1.PodSpec{}
	AppendStatusSidecar(spec, SidecarConfig{AgentName: "alice", Image: "img:v1", TaskResultsRoot: "/persist/results"})
	side := mustStatusSidecar(t, spec)
	for _, env := range side.Env {
		if env.Name == "KYBER_TASK_RESULTS_ROOT" && env.Value == "/persist/results" {
			return
		}
	}
	t.Fatal("KYBER_TASK_RESULTS_ROOT not propagated to status sidecar")
}

func TestAppendStatusSidecar_ConfiguresA2APeersOnlyOnSidecar(t *testing.T) {
	spec := &corev1.PodSpec{Containers: []corev1.Container{{Name: "agent"}}}
	AppendStatusSidecar(spec, SidecarConfig{
		AgentName: "alice", Image: "img:v1", DiskBytes: 1024,
		A2APeers: []A2APeerConfig{{Name: "auditor", URL: "https://agents.example/auditor", CredentialSecret: "auditor-token", CredentialKey: "token"}},
	})
	sidecar := spec.InitContainers[0]
	env := make(map[string]corev1.EnvVar, len(sidecar.Env))
	for _, variable := range sidecar.Env {
		env[variable.Name] = variable
	}
	if got := env["KYBER_A2A_PEERS_JSON"].Value; !strings.Contains(got, `"name":"auditor"`) || strings.Contains(got, "auditor-token") {
		t.Fatalf("KYBER_A2A_PEERS_JSON = %q", got)
	}
	credential := env["KYBER_A2A_PEER_CREDENTIAL_0"].ValueFrom
	if credential == nil || credential.SecretKeyRef == nil || credential.SecretKeyRef.Name != "auditor-token" || credential.SecretKeyRef.Key != "token" {
		t.Fatalf("credential env = %#v", credential)
	}
	for _, variable := range spec.Containers[0].Env {
		if strings.HasPrefix(variable.Name, "KYBER_A2A_PEER_CREDENTIAL_") {
			t.Fatalf("runtime received credential env %q", variable.Name)
		}
	}
}

func TestAppendStatusSidecar_LivenessProbeOnHealthzPort(t *testing.T) {
	spec := &corev1.PodSpec{}
	AppendStatusSidecar(spec, SidecarConfig{AgentName: "alice", Image: "img:v1"})
	side := mustStatusSidecar(t, spec)
	if side.LivenessProbe == nil {
		t.Fatal("liveness probe missing")
	}
	if side.LivenessProbe.HTTPGet == nil || side.LivenessProbe.HTTPGet.Path != "/healthz" {
		t.Errorf("liveness probe path: got %+v, want /healthz", side.LivenessProbe.HTTPGet)
	}
	if side.LivenessProbe.HTTPGet.Port.IntVal != statusSidecarHealthPort {
		t.Errorf("liveness probe port: got %d, want %d", side.LivenessProbe.HTTPGet.Port.IntVal, statusSidecarHealthPort)
	}
}

func TestAppendStatusSidecar_ResourceLimits(t *testing.T) {
	spec := &corev1.PodSpec{}
	AppendStatusSidecar(spec, SidecarConfig{AgentName: "alice", Image: "img:v1"})
	side := mustStatusSidecar(t, spec)
	if side.Resources.Requests.Cpu().IsZero() {
		t.Error("CPU request unset")
	}
	if side.Resources.Limits.Memory().IsZero() {
		t.Error("memory limit unset")
	}
}

func TestAppendStatusSidecar_MountsPersistForManagedA2AResults(t *testing.T) {
	spec := &corev1.PodSpec{}
	AppendStatusSidecar(spec, SidecarConfig{AgentName: "alice", Image: "img:v1"})
	side := mustStatusSidecar(t, spec)
	for _, mount := range side.VolumeMounts {
		if mount.Name == "persist" {
			if mount.MountPath != "/persist" || mount.ReadOnly {
				t.Errorf("persist mount = %+v, want writable /persist", mount)
			}
			return
		}
	}
	t.Fatal("status sidecar has no persist mount")
}

func TestAppendStatusSidecar_SharesRuntimeControlVolume(t *testing.T) {
	spec := &corev1.PodSpec{Containers: []corev1.Container{{Name: "agent"}}}
	AppendStatusSidecar(spec, SidecarConfig{AgentName: "alice", Image: "img:v1"})

	assertMount := func(container corev1.Container) {
		t.Helper()
		for _, mount := range container.VolumeMounts {
			if mount.Name == runtimeControlVolumeName && mount.MountPath == runtimeControlMountPath && !mount.ReadOnly {
				return
			}
		}
		t.Errorf("container %q lacks writable runtime-control mount", container.Name)
	}
	assertMount(spec.Containers[0])
	assertMount(mustStatusSidecar(t, spec))
	for _, volume := range spec.Volumes {
		if volume.Name == runtimeControlVolumeName && volume.EmptyDir != nil {
			return
		}
	}
	t.Fatal("runtime-control EmptyDir volume missing")
}

// TestBuildPodSpec_PlusInjection verifies that the full assembly path
// (BuildPodSpec → AppendStatusSidecar) yields a pod with both the
// runtime container and the status sidecar, in the right order. This
// is the integration-style coverage the kyber#248 self-review flagged
// as missing — earlier tests exercise BuildPodSpec or AppendStatusSidecar
// in isolation, never together.
func TestBuildPodSpec_PlusInjection(t *testing.T) {
	t.Setenv("KYBER_CONTROL_PLANE_INTERNAL_URL", "http://kyber-control-plane.kyber-system:8082")
	agent := testAgent()
	adapter := testAdapter()

	spec, err := BuildPodSpec(agent, adapter, "node-01")
	if err != nil {
		t.Fatalf("BuildPodSpec: %v", err)
	}
	AppendStatusSidecar(&spec, SidecarConfig{AgentName: agent.Name, Image: "ghcr.io/matty-v/kyber-status-sidecar:v1"})

	// kyber#575: agent stays the sole regular container; the status-sidecar is a
	// native sidecar prepended to InitContainers (ahead of the setup init).
	if len(spec.Containers) != 1 {
		t.Fatalf("expected 1 regular container (runtime), got %d", len(spec.Containers))
	}
	if spec.Containers[0].Name != "agent" {
		t.Errorf("regular container should be 'agent' (runtime); got %q", spec.Containers[0].Name)
	}
	if len(spec.InitContainers) == 0 || spec.InitContainers[0].Name != StatusSidecarContainerName {
		t.Errorf("status-sidecar should be prepended to InitContainers; order=%v", initNames(&spec))
	}
}

// TestAppendStatusSidecar_OmitsOtelEnvWhenUnset locks the conservative
// behavior for dev installs / older deployments that haven't wired the
// collector: AppendStatusSidecar must NOT emit empty KYBER_OTEL_ENDPOINT or
// KYBER_RUNTIME_TYPE env vars (the sidecar reads them as "unset" and skips
// metrics init, which is what we want).
func TestAppendStatusSidecar_OmitsOtelEnvWhenUnset(t *testing.T) {
	spec := &corev1.PodSpec{}
	AppendStatusSidecar(spec, SidecarConfig{AgentName: "alice", Image: "img:v1"})
	side := mustStatusSidecar(t, spec)
	for _, e := range side.Env {
		if e.Name == "KYBER_OTEL_ENDPOINT" || e.Name == "KYBER_RUNTIME_TYPE" {
			t.Errorf("env %q must be omitted when SidecarConfig field is empty; got value %q", e.Name, e.Value)
		}
	}
}

// TestAppendStatusSidecar_EmitsOtelEnvWhenSet verifies that the new
// kyber#256 fields propagate into pod-spec env. The sidecar reads
// KYBER_OTEL_ENDPOINT to gate metrics init and KYBER_RUNTIME_TYPE to label
// per-agent metric points.
func TestAppendStatusSidecar_EmitsOtelEnvWhenSet(t *testing.T) {
	spec := &corev1.PodSpec{}
	AppendStatusSidecar(spec, SidecarConfig{
		AgentName:    "alice",
		Image:        "img:v1",
		OtelEndpoint: "http://kyber-otel-collector:4318",
		Runtime:      "claude-code",
	})
	side := mustStatusSidecar(t, spec)
	envByName := map[string]string{}
	for _, e := range side.Env {
		envByName[e.Name] = e.Value
	}
	if got := envByName["KYBER_OTEL_ENDPOINT"]; got != "http://kyber-otel-collector:4318" {
		t.Errorf("KYBER_OTEL_ENDPOINT: got %q, want collector URL", got)
	}
	if got := envByName["KYBER_RUNTIME_TYPE"]; got != "claude-code" {
		t.Errorf("KYBER_RUNTIME_TYPE: got %q, want claude-code", got)
	}
}

// TestAppendStatusSidecar_OmitsLogLevelEnvWhenUnset locks the default-off
// behavior for kyber#360 diagnostic logging — when the operator hasn't set
// the toggle on the CP deployment, sidecar pod specs must not include the
// env var. The pod spec stays byte-identical to its prior shape during
// normal operation, so this PR's wiring change doesn't churn existing pods.
func TestAppendStatusSidecar_OmitsLogLevelEnvWhenUnset(t *testing.T) {
	spec := &corev1.PodSpec{}
	AppendStatusSidecar(spec, SidecarConfig{AgentName: "alice", Image: "img:v1"})
	side := mustStatusSidecar(t, spec)
	for _, e := range side.Env {
		if e.Name == "KYBER_SIDECAR_LOG_LEVEL" {
			t.Errorf("env KYBER_SIDECAR_LOG_LEVEL must be omitted when LogLevel is empty; got %q", e.Value)
		}
	}
}

// TestAppendStatusSidecar_EmitsLogLevelEnvWhenSet verifies kyber#360's
// CP-controlled diagnostic toggle propagates into the sidecar pod env so
// every fleet pod picks up the new level on the next reconcile cycle.
func TestAppendStatusSidecar_EmitsLogLevelEnvWhenSet(t *testing.T) {
	spec := &corev1.PodSpec{}
	AppendStatusSidecar(spec, SidecarConfig{
		AgentName: "alice",
		Image:     "img:v1",
		LogLevel:  "debug",
	})
	side := mustStatusSidecar(t, spec)
	envByName := map[string]string{}
	for _, e := range side.Env {
		envByName[e.Name] = e.Value
	}
	if got := envByName["KYBER_SIDECAR_LOG_LEVEL"]; got != "debug" {
		t.Errorf("KYBER_SIDECAR_LOG_LEVEL: got %q, want debug", got)
	}
}

func TestAppendStatusSidecar_SecurityHardened(t *testing.T) {
	spec := &corev1.PodSpec{}
	AppendStatusSidecar(spec, SidecarConfig{AgentName: "alice", Image: "img:v1"})
	side := mustStatusSidecar(t, spec)
	if side.SecurityContext == nil {
		t.Fatal("security context unset")
	}
	if side.SecurityContext.ReadOnlyRootFilesystem == nil || !*side.SecurityContext.ReadOnlyRootFilesystem {
		t.Error("ReadOnlyRootFilesystem must be true")
	}
	if side.SecurityContext.AllowPrivilegeEscalation == nil || *side.SecurityContext.AllowPrivilegeEscalation {
		t.Error("AllowPrivilegeEscalation must be false")
	}
	if side.SecurityContext.RunAsUser == nil || *side.SecurityContext.RunAsUser != 0 {
		t.Error("RunAsUser must be root for the root-owned persist PVC")
	}
}

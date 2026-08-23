// Status-sidecar injection for agent pods (kyber#248). Adds the
// kyber-status-sidecar container to the pod spec built by
// BuildPodSpec. The sidecar pushes heartbeats (and, post-#249, activity
// events) to the control plane's /internal/agents/{name}/status-stream
// endpoint.
//
// Kept separate from pod_builder.go so:
//   1. Unit tests can exercise the injection without faking the runtime
//      adapter or pod spec assembly.
//   2. Future runtimes (Codex / OpenClaw / Hermes) get the sidecar for
//      free — the injection is platform-level, not adapter-level.

package agent

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/intstr"
)

const (
	// StatusSidecarContainerName is the container name in the pod spec.
	// Kept stable so kubectl/PWA tooling can target it predictably.
	StatusSidecarContainerName = "kyber-status-sidecar"
	// statusSidecarHealthPort is where the sidecar serves /healthz. Must
	// match the const in cmd/status-sidecar/main.go.
	statusSidecarHealthPort int32 = 8090
)

// SidecarConfig carries the optional fields the controller threads through
// to the sidecar's pod-spec env. AgentName + Image are required (the latter
// gates whether the sidecar is injected at all); OtelEndpoint and Runtime
// are added in kyber#256 to surface per-agent metrics. Both are tolerated
// empty — the sidecar disables metrics emission in that case. LogLevel
// (kyber#360) propagates the operator-chosen debug toggle from the CP
// deployment to every sidecar pod, so enabling the diagnostic-safety-net
// logs is a single CP env-var edit instead of a per-pod patch.
type SidecarConfig struct {
	AgentName    string
	Image        string
	OtelEndpoint string
	Runtime      string
	LogLevel     string
}

// AppendStatusSidecar appends the kyber-status-sidecar container to pod's
// Containers slice. No-op when cfg.Image is empty (dev installs / tests).
//
// The sidecar inherits AGENT_NAME and KYBER_CONTROL_PLANE_INTERNAL_URL
// from the same env-var convention as kyber-token-reporter
// (cmd/token-reporter/main.go:35) — same control plane URL, same agent
// identity, no new wire concepts. KYBER_OTEL_ENDPOINT and KYBER_RUNTIME_TYPE
// were added in kyber#256 so the sidecar can label per-agent OTel metrics
// (Phase C1 of the observability epic).
//
// Resource sizing is intentionally tight: the sidecar runs a short
// network loop with a 5s heartbeat. Spending more would just waste
// per-pod overhead with no benefit. Limits are non-zero so a runaway
// process can't starve the runtime container.
func AppendStatusSidecar(spec *corev1.PodSpec, cfg SidecarConfig) {
	if cfg.Image == "" {
		return
	}

	probe := &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{
				Path: "/healthz",
				Port: intstr.FromInt32(statusSidecarHealthPort),
			},
		},
		InitialDelaySeconds: 10,
		PeriodSeconds:       30,
		FailureThreshold:    3,
	}

	env := []corev1.EnvVar{
		{Name: "AGENT_NAME", Value: cfg.AgentName},
		{Name: "KYBER_CONTROL_PLANE_INTERNAL_URL", Value: controlPlaneInternalURL()},
	}
	env = append(env, loggingContextEnv(StatusSidecarContainerName)...)
	// Only emit the OTel + runtime envs when a value is set so old tests +
	// dev installs that didn't configure a collector keep their existing
	// pod spec shape (and the sidecar's empty-endpoint branch disables
	// metrics rather than crashing).
	if cfg.OtelEndpoint != "" {
		env = append(env, corev1.EnvVar{Name: "KYBER_OTEL_ENDPOINT", Value: cfg.OtelEndpoint})
	}
	if cfg.Runtime != "" {
		env = append(env, corev1.EnvVar{Name: "KYBER_RUNTIME_TYPE", Value: cfg.Runtime})
	}
	// kyber#360 diagnostic-safety-net toggle. Off by default — only emit
	// the env when the operator has set it on the CP deployment, so the
	// pod spec stays byte-identical to its prior shape under normal
	// operation. The sidecar's main() reads this and bumps slog to
	// LevelDebug when value == "debug" (case-insensitive).
	if cfg.LogLevel != "" {
		env = append(env, corev1.EnvVar{Name: "KYBER_SIDECAR_LOG_LEVEL", Value: cfg.LogLevel})
	}

	container := corev1.Container{
		Name:  StatusSidecarContainerName,
		Image: cfg.Image,
		Env:   env,
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("10m"),
				corev1.ResourceMemory: resource.MustParse("32Mi"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("100m"),
				corev1.ResourceMemory: resource.MustParse("64Mi"),
			},
		},
		LivenessProbe: probe,
		// Pod-token mount (kyber#566): the sidecar reads the token from
		// PodTokenMountDir and presents it as a Bearer to the internal API
		// (cmd/status-sidecar/main.go). The volume itself is declared by
		// BuildPodSpec; here we just mount it read-only into the sidecar.
		VolumeMounts: []corev1.VolumeMount{
			{
				Name:      PodTokenVolumeName,
				MountPath: PodTokenMountDir,
				ReadOnly:  true,
			},
		},
		// Read-only root filesystem: the sidecar reads the pod-token Secret
		// from a mounted path and writes nothing to disk.
		// Distroless/static-debian12:nonroot in the image already runs
		// as a non-root user; this just hardens the container further.
		SecurityContext: &corev1.SecurityContext{
			ReadOnlyRootFilesystem:   ptrTo(true),
			AllowPrivilegeEscalation: ptrTo(false),
		},
		// Native sidecar (kyber#575): RestartPolicy:Always on an init container
		// makes the kubelet restart it on ANY exit — OOM, panic, SIGTERM, a clean
		// exit 0 — INDEPENDENTLY of the pod-level RestartPolicy:Never. Before this,
		// a dead status-sidecar was never restarted (pod-level Never), leaving the
		// pod permanently NotReady with a silently-frozen heartbeat until a human
		// pod-deleted (the r2-d2 incident). The pod-level Never stays (pod_builder.go)
		// so the #563 agent-container contract is preserved: when the agent
		// container exits, the kubelet tears native sidecars down in reverse order
		// and the pod reaches a terminal phase → the controller recreates it.
		RestartPolicy: ptrTo(corev1.ContainerRestartPolicyAlways),
	}
	// PREPEND to InitContainers, ahead of the git-clone "setup" init container, so
	// the heartbeat is live DURING the potentially-slow clone/boot rather than only
	// after the agent container starts. No startupProbe is set: a native sidecar is
	// "started" as soon as it runs, so it never gates the agent container's start
	// (the sidecar is observability, not on the critical path). Read-sites that
	// look up this container by name must consult Spec.InitContainers /
	// Status.InitContainerStatuses (see extractSidecarSpecImage / isSidecarReady).
	spec.InitContainers = append([]corev1.Container{container}, spec.InitContainers...)
}

// ptrTo returns a pointer to v. Generic helper; Go 1.21+.
func ptrTo[T any](v T) *T { return &v }

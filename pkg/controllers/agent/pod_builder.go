package agent

import (
	"fmt"
	"os"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	pkgruntimes "github.com/matty-v/kyber/pkg/runtimes"
)

// controlPlaneInternalURL returns the in-cluster URL of the control-plane internal API.
// Sourced from KYBER_CONTROL_PLANE_INTERNAL_URL, which the Helm chart sets on the
// control-plane Deployment. Required — the controller fatals at startup if unset
// (see cmd/control-plane/main.go).
func controlPlaneInternalURL() string {
	return os.Getenv("KYBER_CONTROL_PLANE_INTERNAL_URL")
}

// controlPlanePublicURL returns the in-cluster URL of the control-plane PUBLIC
// API (:8080), where /webhooks/inbound is served — the endpoint the Discord
// sidecar (kyber#646) forwards messages to. Sourced from KYBER_CONTROL_PLANE_URL
// (chart-set on the control-plane Deployment); falls back to the internal URL
// with the port swapped :8082→:8080 so an in-cluster config still resolves when
// only the internal URL is set.
func controlPlanePublicURL() string {
	if u := os.Getenv("KYBER_CONTROL_PLANE_URL"); u != "" {
		return u
	}
	return strings.Replace(controlPlaneInternalURL(), ":8082", ":8080", 1)
}

// agentPrivileged reports whether agent pods should run in the legacy
// full-Privileged profile. Sourced from KYBER_AGENT_PRIVILEGED (Helm-set on the
// control-plane Deployment). Default (empty/false) is the de-privileged hardened
// profile — see buildAgentSecurityContext and
// docs/design/2026-08-15-agent-pod-isolation-design.md (kyber#76). "true"
// restores the pre-#76 behavior as a break-glass rollback.
func agentPrivileged() bool {
	return strings.EqualFold(os.Getenv("KYBER_AGENT_PRIVILEGED"), "true")
}

// agentUserNamespaces reports whether agent pods should run in a Linux user
// namespace (pod.spec.hostUsers=false), remapping in-pod root to an
// unprivileged host uid. Sourced from KYBER_AGENT_USER_NAMESPACES. Default off:
// enabling it safely requires per-cluster validation of runtime/kernel support
// (idmapped mounts for the pre-existing persist PVC) — see the design doc's
// rollout section. Ignored when agentPrivileged() is true (a Privileged
// container in a user namespace is contradictory).
func agentUserNamespaces() bool {
	return !agentPrivileged() && strings.EqualFold(os.Getenv("KYBER_AGENT_USER_NAMESPACES"), "true")
}

// agentSeccompProfileType returns the seccomp profile type applied to the
// de-privileged agent container. Sourced from KYBER_AGENT_SECCOMP_PROFILE;
// accepts "Unconfined" (case-insensitive) and defaults to RuntimeDefault for
// anything else (including empty). Unconfined is the documented fallback when a
// target's RuntimeDefault profile is strict enough to block the mount/FUSE
// syscalls fuse-overlayfs needs — dropping Privileged is what closes the host
// escape, not the seccomp profile.
func agentSeccompProfileType() corev1.SeccompProfileType {
	if strings.EqualFold(os.Getenv("KYBER_AGENT_SECCOMP_PROFILE"), "Unconfined") {
		return corev1.SeccompProfileTypeUnconfined
	}
	return corev1.SeccompProfileTypeRuntimeDefault
}

// buildAgentSecurityContext returns the main agent container's security context.
//
// Default (de-privileged, kyber#76): Privileged is false, so host block devices
// are absent from the pod and the device cgroup reverts to the runtime-default
// allow-list — the G3 host escape (mount /dev/sda) has no device to act on even
// though CAP_SYS_ADMIN is still held. SYS_ADMIN is retained because it is the
// one capability fuse-overlayfs's mount(2) requires; the rest of the capability
// set is the runtime default (sufficient for apt/su). Seccomp is applied.
// AllowPrivilegeEscalation is deliberately left unset (true) so in-pod
// `sudo`/setuid — which agents legitimately use to install software — keeps
// working; in-pod root (G2) is an accepted, required capability and is
// host-neutered when user namespaces are enabled.
//
// Legacy (agentPrivileged()==true): the pre-#76 full-Privileged profile, kept
// only as a break-glass rollback.
func buildAgentSecurityContext() *corev1.SecurityContext {
	sysAdmin := &corev1.Capabilities{
		Add: []corev1.Capability{corev1.Capability("SYS_ADMIN")},
	}
	if agentPrivileged() {
		privileged := true
		return &corev1.SecurityContext{
			Privileged:   &privileged,
			Capabilities: sysAdmin,
		}
	}
	privileged := false
	return &corev1.SecurityContext{
		Privileged:     &privileged,
		Capabilities:   sysAdmin,
		SeccompProfile: &corev1.SeccompProfile{Type: agentSeccompProfileType()},
	}
}

// PVCName returns the PersistentVolumeClaim name for the given agent.
// Format: agent-{name}-pv
func PVCName(agentName string) string {
	return fmt.Sprintf("agent-%s-pv", agentName)
}

// OffsetsPVCName returns the dedicated transcript-offsets PVC name for the given
// agent (kyber#467). Format: agent-{name}-offsets-pv — distinct from PVCName so
// the small writable offsets volume never collides with the (read-only, #446)
// persist PVC. This is the durable replacement for the pod-lifetime emptyDir
// that lost its checkpoints on pod recreation and triggered a full backlog
// re-ship (the #466 incident).
func OffsetsPVCName(agentName string) string {
	return fmt.Sprintf("agent-%s-offsets-pv", agentName)
}

// defaultTranscriptOffsetsSize is the offsets PVC size used when no explicit
// size is configured (kyber#467). The offsets store holds only per-file
// shipped-line-count integers (keyed by an md5 of the file path) — sub-1KB even
// at Lando's ~82-file backlog scale — so a tiny claim is deliberate: on gcp a
// GCE-PD has a 1Gi minimum, and binding that for <1KB would waste storage by
// four orders of magnitude (Ackbar's deploy review). The cluster-default
// StorageClass (local-path) is used on all targets, where 10Mi is near-free.
const defaultTranscriptOffsetsSize = "10Mi"

// UserSecretKVName returns the k8s Secret name holding operator-uploaded kv
// secrets for the given agent. Projected into the pod via envFrom with the
// USER_ prefix. See docs/design/2026-04-18-user-secrets-design.md (#75).
func UserSecretKVName(agentName string) string {
	return fmt.Sprintf("%s-user-secrets-kv", agentName)
}

// UserSecretFilesName returns the k8s Secret name holding operator-uploaded
// file secrets for the given agent. Projected into the pod as a volume mount
// at /user-secrets. See docs/design/2026-04-18-user-secrets-design.md (#75).
func UserSecretFilesName(agentName string) string {
	return fmt.Sprintf("%s-user-secrets-files", agentName)
}

// UserSecretsMountPath is the in-pod path for operator-uploaded file secrets.
const UserSecretsMountPath = "/user-secrets"

// PodTokenSecretName returns the k8s Secret name holding the agent's
// control-plane-signed pod-token (kyber#566). Labeled kyber.io/agent=<name> and
// owner-ref'd to the Agent so the deletion finalizer GCs it.
func PodTokenSecretName(agentName string) string {
	return fmt.Sprintf("%s-pod-token", agentName)
}

const (
	// PodTokenSecretKey is the data key under which the signed token is stored
	// in the pod-token Secret — and the filename it surfaces as under the mount.
	PodTokenSecretKey = "pod-token"
	// PodTokenVolumeName is the pod volume that projects the pod-token Secret.
	PodTokenVolumeName = "pod-token"
	// PodTokenMountDir is the directory the pod-token Secret mounts at; the
	// token surfaces as PodTokenMountDir/PodTokenSecretKey. This path is the
	// long-standing client convention (cmd/status-sidecar/main.go,
	// pkg/nodeagent/reporter.go, images/claude-code/start-claude.sh) — clients
	// already read the Bearer here; #566 makes the control plane actually mint
	// and mount it.
	PodTokenMountDir = "/var/run/secrets/kyber"
)

// JobsSourceMountPath is where the per-agent <name>-jobs ConfigMap is mounted
// on the pod. entrypoint.sh copies `<this>/crontab` to /etc/cron.d/kyber-jobs
// and the dispatcher reads prompt-<name> files here at fire time. Kept
// outside /etc/cron.d (and the /persist bind tree) so the runtime's overlay
// + bind-mount-home schemes can both work with a single shared source path.
const JobsSourceMountPath = "/kyber/jobs-src"

// UserSecretsEnvPrefix is prepended to every kv key when it's projected into
// the pod as an env var — enforces the mount-time isolation from KYBER_* and
// framework env vars.
const UserSecretsEnvPrefix = "USER_"

// AgentContainerName is the main agent container's name in the pod spec.
// Mirrors StatusSidecarContainerName (status_sidecar.go) so drift detection can
// key off the container by name rather than a positional index.
const AgentContainerName = "agent"

// AgentPodName returns the deterministic pod name for an agent.
// Format: agent-{name}
func AgentPodName(agentName string) string {
	return fmt.Sprintf("agent-%s", agentName)
}

// AgentPodLabels returns the labels to apply to an agent pod.
// Labels: kyber.io/agent={name}, kyber.io/runtime={type}, kyber.io/auth-type={authType}
//
// kyber.io/auth-type is used by node-agent (Task 5.2) to classify refresh failures:
// only pods with auth-type="oauth" emit NeedsAuth events on token expiry.
func AgentPodLabels(agent *kyberv1.Agent, adapter pkgruntimes.Adapter) map[string]string {
	return map[string]string{
		"kyber.io/agent":     agent.Name,
		"kyber.io/runtime":   adapter.Type(),
		"kyber.io/auth-type": string(agent.Spec.Secrets.AuthType),
	}
}

// BuildPodSpec constructs a corev1.PodSpec for the given agent and runtime adapter.
// nodeName is the k8s node name corresponding to the agent's spec.machine
// (resolved by the reconciler from the Machine CRD's status.nodeName before calling this).
//
// The returned PodSpec is ready to embed in a Pod object — the caller sets ObjectMeta
// (name, namespace, labels, ownerReferences).
func BuildPodSpec(agent *kyberv1.Agent, adapter pkgruntimes.Adapter, nodeName string) (corev1.PodSpec, error) {
	gracePeriod := int64(adapter.GracefulShutdownSeconds())

	// --- Volumes ---
	volumes := []corev1.Volume{
		{
			Name: "persist",
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: PVCName(agent.Name),
				},
			},
		},
	}

	secretMounts := adapter.SecretMounts(agent)
	for _, sm := range secretMounts {
		readOnly := true
		volumes = append(volumes, corev1.Volume{
			Name: sm.Name,
			VolumeSource: corev1.VolumeSource{
				CSI: &corev1.CSIVolumeSource{
					Driver:   "secrets-store.csi.k8s.io",
					ReadOnly: &readOnly,
					VolumeAttributes: map[string]string{
						"secretProviderClass": sm.ProviderClass,
					},
				},
			},
		})
	}

	// Identity-repo: when spec.identityRepo.repo is set, the pod gets the
	// KYBER_IDENTITY_REPO env (below) so start-claude.sh clones it. As of
	// kyber#509 git auth rides the generic PAT user-secret ($GH_TOKEN /
	// $USER_GITHUB_TOKEN), so there is no per-agent <name>-github Secret volume
	// to deliver or mount anymore.
	identityRepoConfigured := agent.Spec.IdentityRepo.Repo != ""

	// /dev/fuse for fuse-overlayfs (whole-disk persistence fallback when kernel
	// overlayfs is rejected by nested overlay scenarios — see entrypoint.sh).
	fuseDevType := corev1.HostPathCharDev
	volumes = append(volumes, corev1.Volume{
		Name: "fuse-dev",
		VolumeSource: corev1.VolumeSource{
			HostPath: &corev1.HostPathVolumeSource{
				Path: "/dev/fuse",
				Type: &fuseDevType,
			},
		},
	})

	// User file-secrets volume (#75). Mounted unconditionally — the controller
	// eagerly creates an empty Secret alongside the Agent CR, so the reference
	// always resolves even when the operator hasn't uploaded anything yet.
	volumes = append(volumes, corev1.Volume{
		Name: "user-secrets-files",
		VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{
				SecretName: UserSecretFilesName(agent.Name),
			},
		},
	})

	// Pod-token Secret (kyber#566). Carries the agent's control-plane-signed
	// pod-token, mounted at PodTokenMountDir so the agent's clients (start-
	// claude.sh, the job dispatcher, the session-brief init container) and the
	// status-sidecar can present it as a Bearer to the internal API. Optional so
	// the pod still schedules during the migration window before the reconciler
	// has minted the Secret (or when no signing key is configured) — combined
	// with the internal API's grace mode, an absent token is tolerated, not
	// fatal.
	podTokenOptional := true
	volumes = append(volumes, corev1.Volume{
		Name: PodTokenVolumeName,
		VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{
				SecretName: PodTokenSecretName(agent.Name),
				Optional:   &podTokenOptional,
			},
		},
	})

	// Agent jobs ConfigMap (#135). Mounted unconditionally — the controller
	// eagerly creates an empty <agent>-jobs ConfigMap (containing just a
	// no-op crontab header) on every reconcile, so the reference resolves
	// even when spec.jobs is empty. Kubelet refreshes the mount on ConfigMap
	// rotation within ~30-60s; entrypoint.sh copies the content into
	// /etc/cron.d/kyber-jobs at boot, and node-agent re-syncs on change.
	volumes = append(volumes, corev1.Volume{
		Name: "agent-jobs",
		VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{
					Name: JobsConfigMapName(agent.Name),
				},
			},
		},
	})

	// --- Volume mounts for the main container ---
	containerMounts := []corev1.VolumeMount{
		{
			Name:      "persist",
			MountPath: "/persist",
		},
		{
			Name:      "fuse-dev",
			MountPath: "/dev/fuse",
		},
	}
	for _, sm := range secretMounts {
		containerMounts = append(containerMounts, corev1.VolumeMount{
			Name:      sm.Name,
			MountPath: sm.MountPath,
			ReadOnly:  true,
		})
	}
	// User file-secrets mount (#75).
	containerMounts = append(containerMounts, corev1.VolumeMount{
		Name:      "user-secrets-files",
		MountPath: UserSecretsMountPath,
		ReadOnly:  true,
	})
	// Agent jobs ConfigMap mount (#135). The agent runs in a chroot at
	// /merged in overlay mode — entrypoint.sh bind-mounts this path into the
	// chroot so the dispatcher and copy-to-/etc/cron.d step can both read it.
	containerMounts = append(containerMounts, corev1.VolumeMount{
		Name:      "agent-jobs",
		MountPath: JobsSourceMountPath,
		ReadOnly:  true,
	})
	// Pod-token mount (kyber#566) — read-only; surfaces the token at
	// PodTokenMountDir/PodTokenSecretKey for start-claude.sh and kyber-job-dispatch.
	containerMounts = append(containerMounts, corev1.VolumeMount{
		Name:      PodTokenVolumeName,
		MountPath: PodTokenMountDir,
		ReadOnly:  true,
	})

	// --- Environment variables ---
	// Start with AGENT_NAME (always injected regardless of adapter).
	// KYBER_CONTROL_PLANE_INTERNAL_URL gives in-pod services the base URL;
	// KYBER_REFRESH_TOKEN_URL is a pre-built full path for start-claude.sh's
	// boot-time OAuth rotation push. The token-reporter / credential-syncer
	// pair switched to the sidecar in kyber#257, but start-claude.sh's
	// boot-time push runs before the sidecar is reachable and still POSTs
	// directly to the control plane — that migration is a follow-up.
	envVars := []corev1.EnvVar{
		{Name: "AGENT_NAME", Value: agent.Name},
		{Name: "KYBER_CONTROL_PLANE_INTERNAL_URL", Value: controlPlaneInternalURL()},
		{Name: "KYBER_REFRESH_TOKEN_URL", Value: fmt.Sprintf("%s/internal/agents/%s/refresh-token", controlPlaneInternalURL(), agent.Name)},
	}
	// TZ controls the pod-wide timezone — cron daemon, dispatcher logs, and
	// any process that calls localtime(3). Setting it here is strictly more
	// reliable than the CRON_TZ in /etc/cron.d/ rendered by jobs_configmap.go:
	// Debian's cron 3.0pl1-184ubuntu2 silently ignores CRON_TZ in system
	// crontabs (only honors it in user crontabs), so a job declared
	// "0 7 * * *" with CRON_TZ=America/Denver was still firing at 07:00 UTC.
	// Setting TZ on the daemon's environment makes vixie cron interpret
	// every schedule in that TZ regardless of where it lives. See kyber#177
	// follow-up: the CRON_TZ render stays as defense-in-depth for cron
	// implementations that DO honor it (cronie, etc.).
	if tz := agentJobTimezone(); tz != "" {
		envVars = append(envVars, corev1.EnvVar{Name: "TZ", Value: tz})
	}
	if identityRepoConfigured {
		// Only the repo slug — git auth is the generic PAT user-secret
		// installed by start-claude.sh (kyber#509). No token-path env anymore.
		envVars = append(envVars,
			corev1.EnvVar{Name: IdentityRepoEnvVar, Value: agent.Spec.IdentityRepo.Repo},
		)
	}
	// Append adapter-provided env vars.
	envVars = append(envVars, adapter.EnvVars(agent)...)

	// --- Resource requirements ---
	resources := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    agent.Spec.Resources.CPU,
			corev1.ResourceMemory: agent.Spec.Resources.Memory,
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    agent.Spec.Resources.CPU,
			corev1.ResourceMemory: agent.Spec.Resources.Memory,
		},
	}
	// --- Security context ---
	// De-privileged by default (kyber#76): fuse-overlayfs needs only /dev/fuse
	// (mounted above) + CAP_SYS_ADMIN for mount(2), NOT full Privileged.
	// Dropping Privileged removes the host block devices and restores the device
	// cgroup allow-list, closing the host escape while whole-/ overlay
	// persistence keeps working. See buildAgentSecurityContext and
	// docs/design/2026-08-15-agent-pod-isolation-design.md.
	securityContext := buildAgentSecurityContext()

	// User kv-secrets envFrom (#75). Projected from {name}-user-secrets-kv
	// with the USER_ prefix so operator-uploaded kv entries appear as
	// $USER_<KEY> in the pod env. Secret is eagerly created empty by the
	// reconciler so this reference always resolves.
	envFrom := []corev1.EnvFromSource{
		{
			Prefix: UserSecretsEnvPrefix,
			SecretRef: &corev1.SecretEnvSource{
				LocalObjectReference: corev1.LocalObjectReference{
					Name: UserSecretKVName(agent.Name),
				},
			},
		},
	}

	// --- Main container ---
	container := corev1.Container{
		Name:            AgentContainerName,
		Image:           adapter.Image(),
		Args:            adapter.EntrypointArgs(agent),
		Env:             envVars,
		EnvFrom:         envFrom,
		VolumeMounts:    containerMounts,
		Resources:       resources,
		SecurityContext: securityContext,
		LivenessProbe:   adapter.LivenessProbe(),
		ReadinessProbe:  adapter.ReadinessProbe(),
		// TTY + Stdin are required for Claude Code, which uses Ink (React-for-
		// terminal) that needs raw mode on stdin. Without a pseudo-terminal the
		// process errors with "Raw mode is not supported on stdin" and the
		// Telegram plugin can't initialize.
		TTY:   true,
		Stdin: true,
	}

	// preStop hook (runtime-specific, optional). The Claude Code runtime uses it
	// to release the Telegram plugin's getUpdates slot before the pod dies —
	// see runtimes.Adapter.PreStopCommand. Nil for runtimes that need no action.
	if preStop := adapter.PreStopCommand(); preStop != nil {
		container.Lifecycle = &corev1.Lifecycle{
			PreStop: &corev1.LifecycleHandler{
				Exec: &corev1.ExecAction{Command: preStop},
			},
		}
	}

	// --- Init container: session brief fetch ---
	// B2 will fill in the actual brief preparation logic on the control plane side.
	// For B1, the init container fetches from the internal API endpoint (or writes {} on failure).
	briefURL := fmt.Sprintf("%s/internal/agents/%s/session-brief",
		controlPlaneInternalURL(), agent.Name)
	briefPath := adapter.SessionBriefPath()

	// The init container must run as root so it can write to the freshly-
	// provisioned PVC (which starts with root ownership). The main container's
	// entrypoint later chowns /persist to the non-root agent user.
	var rootUID int64
	initContainer := corev1.Container{
		Name:    "session-brief",
		Image:   "curlimages/curl:latest",
		Command: []string{"/bin/sh", "-c"},
		Args: []string{
			// Present the pod-token as a Bearer so the internal API admits this
			// fetch as the agent itself (kyber#566). The token file may be
			// absent during the migration (Optional mount) — `cat … 2>/dev/null`
			// yields an empty Bearer, which the internal API's grace mode
			// tolerates; once enforcement is on the minted token is present.
			fmt.Sprintf(
				"curl -sf -H \"Authorization: Bearer $(cat %s/%s 2>/dev/null)\" %s -o %s || echo '{}' > %s",
				PodTokenMountDir, PodTokenSecretKey, briefURL, briefPath, briefPath,
			),
		},
		SecurityContext: &corev1.SecurityContext{
			RunAsUser: &rootUID,
		},
		VolumeMounts: []corev1.VolumeMount{
			{
				Name:      "persist",
				MountPath: "/persist",
			},
			{
				Name:      PodTokenVolumeName,
				MountPath: PodTokenMountDir,
				ReadOnly:  true,
			},
		},
		// Ensure resource limits for the init container are minimal.
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("10m"),
				corev1.ResourceMemory: resource.MustParse("16Mi"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("100m"),
				corev1.ResourceMemory: resource.MustParse("64Mi"),
			},
		},
	}

	// --- Node affinity: schedule on the node corresponding to spec.machine ---
	affinity := &corev1.Affinity{
		NodeAffinity: &corev1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
				NodeSelectorTerms: []corev1.NodeSelectorTerm{
					{
						MatchExpressions: []corev1.NodeSelectorRequirement{
							{
								Key:      "kubernetes.io/hostname",
								Operator: corev1.NodeSelectorOpIn,
								Values:   []string{nodeName},
							},
						},
					},
				},
			},
		},
	}

	podSpec := corev1.PodSpec{
		InitContainers:                []corev1.Container{initContainer},
		Containers:                    []corev1.Container{container},
		Volumes:                       volumes,
		Affinity:                      affinity,
		TerminationGracePeriodSeconds: &gracePeriod,
		RestartPolicy:                 corev1.RestartPolicyNever,
	}

	// User namespaces (kyber#76, Phase 2): remap in-pod root (and its retained
	// CAP_SYS_ADMIN) to an unprivileged host uid so it is powerless against
	// host-owned resources while staying fully root inside the pod. Off by
	// default — enabling requires per-cluster runtime/kernel validation (see the
	// design doc). hostUsers=false is meaningless under the legacy Privileged
	// profile, so agentUserNamespaces() already gates on !agentPrivileged().
	if agentUserNamespaces() {
		hostUsers := false
		podSpec.HostUsers = &hostUsers
	}

	return podSpec, nil
}

// BuildPVC returns a PersistentVolumeClaim for the agent's persistent storage.
// The PVC is created once (on first reconcile of a new agent) and preserved across
// all lifecycle transitions until the agent is deleted.
//
// An empty storageClassName omits the StorageClassName field on the PVC spec,
// which causes Kubernetes to bind against the cluster's default StorageClass
// (e.g. local-path on k3s, standard on GKE).
func BuildPVC(agent *kyberv1.Agent, storageClassName string) *corev1.PersistentVolumeClaim {
	storageRequest := corev1.ResourceList{
		corev1.ResourceStorage: agent.Spec.Resources.Disk,
	}

	pvc := &corev1.PersistentVolumeClaim{}
	pvc.Name = PVCName(agent.Name)
	pvc.Namespace = agent.Namespace
	pvc.Labels = map[string]string{
		"kyber.io/agent": agent.Name,
	}

	if storageClassName != "" {
		pvc.Spec.StorageClassName = &storageClassName
	}

	pvc.Spec.AccessModes = []corev1.PersistentVolumeAccessMode{
		corev1.ReadWriteOnce,
	}
	pvc.Spec.Resources = corev1.VolumeResourceRequirements{
		Requests: storageRequest,
	}

	return pvc
}

// BuildOffsetsPVC returns the small RWO PersistentVolumeClaim that durably holds
// the transcript-tailer's per-file offset checkpoints (kyber#467). It mirrors
// BuildPVC but is deliberately tiny and independent of the agent's persist disk:
// the data is line-count integers only, so size defaults to
// defaultTranscriptOffsetsSize when size is empty (and falls back to it on a
// malformed value rather than panicking). An empty storageClassName omits the
// field so the cluster default StorageClass binds it — which, per Ackbar's
// deploy review, must stay the local-path default on ALL targets (never kyber-pd,
// whose 1Gi PD minimum would waste space for sub-1KB checkpoints).
//
// A kyber.io/volume=transcript-offsets label distinguishes it from the persist
// PVC so ops/GC tooling can select the two apart. The reconciler owner-refs it
// to the Agent CRD so it is garbage-collected on agent deletion.
func BuildOffsetsPVC(agent *kyberv1.Agent, storageClassName, size string) *corev1.PersistentVolumeClaim {
	if size == "" {
		size = defaultTranscriptOffsetsSize
	}
	qty, err := resource.ParseQuantity(size)
	if err != nil {
		// Never panic on operator misconfiguration — a bad size string falls
		// back to the safe tiny default rather than failing the reconcile.
		qty = resource.MustParse(defaultTranscriptOffsetsSize)
	}

	pvc := &corev1.PersistentVolumeClaim{}
	pvc.Name = OffsetsPVCName(agent.Name)
	pvc.Namespace = agent.Namespace
	pvc.Labels = map[string]string{
		"kyber.io/agent":  agent.Name,
		"kyber.io/volume": "transcript-offsets",
	}

	if storageClassName != "" {
		pvc.Spec.StorageClassName = &storageClassName
	}

	pvc.Spec.AccessModes = []corev1.PersistentVolumeAccessMode{
		corev1.ReadWriteOnce,
	}
	pvc.Spec.Resources = corev1.VolumeResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceStorage: qty,
		},
	}

	return pvc
}

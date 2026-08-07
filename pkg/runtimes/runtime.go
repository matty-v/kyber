// Package runtimes is the per-runtime knowledge registry. Each supported
// runtime (claude-code, codex, openclaw, hermes, ...) lives in its own
// subpackage that self-registers via init() against the global registry.
// Both the control-plane binary and the future status-sidecar binary
// blank-import the runtime packages they want enabled.
//
// Adding a runtime is mechanically:
//
//	pkg/runtimes/<newtype>/
//	    runtime.go     // package init() calls runtimes.Register(...)
//	    adapter.go     // implements Adapter (pod-spec assembly)
//	    probe.go       // implements Probe (sidecar-side, kyber#248)
//	    paths.go       // shared constants
//
// ...plus a `_ "github.com/matty-v/kyber/pkg/runtimes/<newtype>"` blank
// import in any binary that should accept the new runtime.
//
// Spec: kyber#250.
package runtimes

import (
	"sync"

	corev1 "k8s.io/api/core/v1"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
)

// Runtime groups the per-runtime concerns the platform cares about behind
// a single interface. Implementations are registered at package init time.
type Runtime interface {
	// Type returns the runtime identifier (e.g. "claude-code"). Must match
	// what operators set on Agent.Spec.Runtime.
	Type() string

	// Adapter returns the pod-spec-side adapter — image reference, env vars,
	// secret mounts, probes, session paths, restart-session command. Used
	// by the agent reconciler when assembling the agent's Pod spec.
	Adapter() Adapter

	// Probe returns a sidecar-side hook for cross-runtime signals that
	// don't need filesystem access (process health from /proc, OTel
	// metrics, etc). Reserved for future signal types — kyber#249 ships
	// activity detection via in-pod runtime binaries instead, because
	// the agent's transcripts live inside its chroot/overlay and can't
	// be reached from the sidecar's filesystem. See
	// docs/architecture/status-pipeline.md for when to use Probe vs the
	// in-pod-binary path.
	Probe() Probe
}

// Adapter is the pod-spec-side interface the agent reconciler uses to
// assemble a pod for a runtime. Lifted from
// pkg/controllers/agent/adapter.go's RuntimeAdapter (kyber#250).
type Adapter interface {
	// Type returns the runtime identifier (e.g., "claude-code", "openclaw").
	Type() string

	// Image returns the container image reference for this runtime.
	Image() string

	// EntrypointArgs returns arguments passed to the overlay entrypoint.
	// The entrypoint script is baked into the image; these are the args passed to it.
	EntrypointArgs(agent *kyberv1.Agent) []string

	// EnvVars returns runtime-specific environment variables to inject into the agent container.
	EnvVars(agent *kyberv1.Agent) []corev1.EnvVar

	// SecretMounts returns the secret volume mounts this runtime needs.
	SecretMounts(agent *kyberv1.Agent) []SecretMount

	// LivenessProbe returns the probe spec for health checking.
	LivenessProbe() *corev1.Probe

	// ReadinessProbe returns the probe spec for readiness.
	ReadinessProbe() *corev1.Probe

	// GracefulShutdownSeconds returns how long to wait for graceful termination.
	GracefulShutdownSeconds() int32

	// SessionBriefPath returns the PV path where the init container writes the session brief.
	SessionBriefPath() string

	// SessionStatePath returns the PV path where the agent MAY write session state before shutdown.
	// Read by the controller after pod termination to construct the next brief.
	SessionStatePath() string

	// ModelEnvVar returns the env var name used to set the LLM model (e.g., "CLAUDE_MODEL").
	ModelEnvVar() string

	// CredentialSecretName returns the name of the Secret holding the
	// operator-supplied model credential for this agent, or "" when the
	// runtime has no such Secret (or the agent's auth type does not use one).
	//
	// This is what lets the controller tell "the operator re-authorized" from
	// "this agent is still configured to want to be Running" (kyber#684).
	// NeedsAuth used to leave on the standing desiredPhase=Running, which is
	// permanently true for every agent — so there was no edge to detect and a
	// dead credential rebuilt its pod every ~20s forever. Keyed on this
	// Secret's identity instead, re-entry happens exactly once per new
	// credential.
	CredentialSecretName(agent *kyberv1.Agent) string

	// RestartSessionCommand returns the argv to exec inside the agent pod to
	// reset the in-flight session without rolling the pod. Wraps whatever
	// the runtime needs to kill its session + re-launch in place.
	//
	// Return nil (not an empty slice) when the runtime does not support
	// session restart — the API handler translates nil to 501. New runtimes
	// (#133 Codex, #137 OpenClaw) can ship without implementing the hook;
	// the pod-level "Restart pod" still works as the heavier alternative.
	//
	// Callers exec the returned argv via SPDY in the agent container; the
	// script/command is responsible for its own session-lock coordination
	// (see #135 D9) and for not leaving orphan processes behind.
	RestartSessionCommand() []string

	// PreStopCommand returns the argv for the pod's container preStop lifecycle
	// hook, or nil when the runtime needs no pre-stop action. Kubelet runs it
	// synchronously (within the termination grace period) BEFORE sending SIGTERM
	// to the container's PID 1.
	//
	// The Claude Code runtime uses it to let the Telegram channel plugin release
	// its Telegram getUpdates slot before the pod dies. Telegram allows exactly
	// one getUpdates consumer per bot token; the plugin frees the slot on
	// SIGTERM/stdin-EOF via bot.stop(). But CC runs inside a *detached* tmux
	// server that never receives the pod's SIGTERM, so on reboot the plugin is
	// SIGKILLed without releasing the slot — the next pod's bot then gets 409
	// Conflict, gives up after ~28s, and the channel stays dead until a manual
	// /reload-plugins. The preStop hook signals the plugin directly so it shuts
	// down cleanly and the incoming pod starts with a free slot.
	//
	// Runtimes with no detached-child signal-isolation problem return nil.
	PreStopCommand() []string
}

// Probe is the sidecar-side interface for cross-runtime signals that
// don't need filesystem access — process health, OTel-style metrics,
// future cross-cutting checks. Each runtime can ship its own
// implementation; the sidecar dispatches via the registry.
//
// Stub today (kyber#250). Activity detection ships via in-pod runtime
// binaries instead (kyber#249 + docs/architecture/status-pipeline.md);
// the Probe path is reserved for signals that genuinely belong in the
// platform sidecar.
type Probe interface {
	// Type returns the runtime identifier. Mirrors Adapter.Type for
	// consistency.
	Type() string
}

// SecretMount describes a secret volume to be mounted into the agent pod.
// Returned by Adapter.SecretMounts.
type SecretMount struct {
	// Name is the volume and volumeMount name.
	Name string
	// MountPath is the path inside the container where the secret is mounted.
	MountPath string
	// ProviderClass is the SecretProviderClass resource name (CSI secrets-store driver).
	ProviderClass string
}

// registry is the package-private map of registered runtimes, keyed by
// Runtime.Type().
var (
	registry   = map[string]Runtime{}
	registryMu sync.RWMutex
)

// Register adds rt to the registry. Panics on duplicate registration —
// runtimes register at init time, so duplicates indicate a programmer
// error (two packages claiming the same Type), not a runtime condition.
//
// Call from each runtime package's init() function, e.g.:
//
//	func init() { runtimes.Register(&ClaudeCode{}) }
func Register(rt Runtime) {
	registryMu.Lock()
	defer registryMu.Unlock()
	t := rt.Type()
	if t == "" {
		panic("runtimes.Register: Runtime.Type() returned empty string")
	}
	if _, exists := registry[t]; exists {
		panic("runtimes.Register: duplicate runtime type " + t)
	}
	registry[t] = rt
}

// Get returns the runtime registered for the given type, or (nil, false)
// if no runtime is registered. Callers should treat false as "this build
// does not support that runtime" — typically a 4xx error to the
// operator, not a 5xx.
func Get(t string) (Runtime, bool) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	rt, ok := registry[t]
	return rt, ok
}

// All returns a snapshot of every registered runtime. Order is not
// guaranteed; callers should sort if order matters. Used by main.go to
// build derived lookups (validRuntimes, restartSessionCommands, etc).
func All() []Runtime {
	registryMu.RLock()
	defer registryMu.RUnlock()
	out := make([]Runtime, 0, len(registry))
	for _, rt := range registry {
		out = append(out, rt)
	}
	return out
}

// HelmImageKey maps a runtime identifier to the Helm values key that pins its
// container image — e.g. "codex" → image.codex.tag, "claude-code" →
// image.claudeCode.tag. Unknown runtimes return the identifier unchanged,
// which is still a better pointer than nothing.
//
// This exists so the API's create-time rejection and the controller's
// RuntimeImageMissing condition name the SAME value, and cannot drift into
// telling an operator two different things to set (kyber#674). Registration
// does not imply usability: a runtime can be registered with no image pinned,
// and until kyber#674 that combination produced an agent that could never
// start and never said why.
func HelmImageKey(runtime string) string {
	switch runtime {
	case "claude-code":
		return "claudeCode"
	default:
		// codex → codex, openclaw → openclaw: the chart keys match the
		// runtime identifier wherever it is already a single lowercase word.
		return runtime
	}
}

// reset is a test-only helper to clear the registry. Not exported. Tests
// in this package use it; tests in other packages should use the
// register-and-cleanup pattern with t.Cleanup.
func reset() {
	registryMu.Lock()
	defer registryMu.Unlock()
	registry = map[string]Runtime{}
}

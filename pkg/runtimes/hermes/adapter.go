package hermes

import (
	"os"

	corev1 "k8s.io/api/core/v1"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/runtimes"
)

const RuntimeImageEnv = "KYBER_HERMES_RUNTIME_IMAGE"

// CustomProviderID is the provider name Kyber writes into Hermes's config.yaml
// when the agent carries spec.inference. It is a fixed, Kyber-owned id rather
// than something operator-supplied so the value can never collide with one of
// Hermes's own provider names or smuggle YAML through the configurator.
const CustomProviderID = "kyber-endpoint"

// defaultProviderID is the built-in provider a Hermes agent uses when it has
// no spec.inference — the behaviour every agent had before that field existed.
const defaultProviderID = "openrouter"

// CustomEndpointStaleTimeoutSeconds is how long Hermes waits for the first
// streamed byte from a custom inference endpoint before killing the request.
//
// Hermes' own default is 180s for any endpoint it does not recognise as local,
// and 900s for localhost ones, because self-hosted servers send nothing until
// prefill finishes. A self-hosted model behind a public URL gets the cloud
// 180s, so a long prompt is cancelled mid-prefill and retried from scratch.
// 900s matches what Hermes already grants local servers.
//
// Set through HERMES_STREAM_STALE_TIMEOUT rather than a per-provider
// stale_timeout_seconds in config.yaml: Hermes looks that up by its runtime
// provider id, which resolves to "custom" for a providers: entry rather than
// CustomProviderID, so a per-provider value would be silently ignored.
const CustomEndpointStaleTimeoutSeconds = "900"

type Adapter struct{ image string }

func NewAdapter() *Adapter       { return &Adapter{image: os.Getenv(RuntimeImageEnv)} }
func (a *Adapter) Type() string  { return Type }
func (a *Adapter) Image() string { return a.image }

func (a *Adapter) EntrypointArgs(*kyberv1.Agent) []string {
	return []string{"/usr/local/bin/start-hermes.sh"}
}

// CredentialSecretName is keyed on by the NeedsAuth recovery gate. An agent
// pointed at its own inference endpoint is recovered by rotating that
// endpoint's Secret, not the OpenRouter one it does not use.
func (a *Adapter) CredentialSecretName(agent *kyberv1.Agent) string {
	if agent == nil {
		return ""
	}
	if inf := agent.Spec.Inference; inf != nil {
		return inf.Credential.ExistingSecret
	}
	if agent.Spec.Secrets.AuthType != kyberv1.AgentAuthTypeAPIKey {
		return ""
	}
	return agent.Name + "-openrouter"
}

func (a *Adapter) EnvVars(agent *kyberv1.Agent) []corev1.EnvVar {
	vars := []corev1.EnvVar{
		{Name: "HERMES_INFERENCE_MODEL", Value: inferenceModel(agent)},
		{Name: "HERMES_PROVIDER", Value: providerID(agent)},
		{Name: "HERMES_HOME", Value: "/home/kyber/.hermes"},
		{Name: "HERMES_YOLO_MODE", Value: "1"},
		{Name: "HERMES_ACCEPT_HOOKS", Value: "1"},
		{Name: "HERMES_DISABLE_LAZY_INSTALLS", Value: "1"},
	}
	// Exactly one credential is referenced. Naming the OpenRouter Secret on an
	// agent that does not use it would block the pod from starting, because a
	// SecretKeyRef to a missing Secret is fatal to pod creation.
	if inf := agent.Spec.Inference; inf != nil {
		vars = append(vars,
			corev1.EnvVar{Name: "KYBER_INFERENCE_BASE_URL", Value: inf.BaseURL},
			corev1.EnvVar{Name: "KYBER_INFERENCE_PROVIDER", Value: CustomProviderID},
			corev1.EnvVar{Name: "HERMES_STREAM_STALE_TIMEOUT", Value: CustomEndpointStaleTimeoutSeconds},
			corev1.EnvVar{Name: "OPENAI_API_KEY", ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: inf.Credential.ExistingSecret},
					Key:                  inf.Credential.Key,
				},
			}},
		)
	} else {
		vars = append(vars, corev1.EnvVar{Name: "OPENROUTER_API_KEY", ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: agent.Name + "-openrouter"},
				Key:                  "token",
			},
		}})
	}
	if agent.Spec.Secrets.TelegramEnabled {
		vars = append(vars, corev1.EnvVar{Name: "KYBER_TELEGRAM_MCP_URL", Value: runtimes.TelegramMCPURL()})
	}
	if agent.Spec.Channels != nil && agent.Spec.Channels.Discord != nil {
		vars = append(vars, corev1.EnvVar{Name: "KYBER_DISCORD_MCP_URL", Value: runtimes.DiscordMCPURL()})
	}
	if agent.Spec.Secrets.SlackEnabled {
		vars = append(vars, corev1.EnvVar{Name: "KYBER_SLACK_MCP_URL", Value: runtimes.SlackMCPURL()})
	}
	return vars
}

func (a *Adapter) SecretMounts(*kyberv1.Agent) []runtimes.SecretMount { return nil }

func (a *Adapter) LivenessProbe() *corev1.Probe {
	// A new runtime image first merges its immutable root into the durable
	// filesystem. Hermes is a large Python image and a cold pull plus bounded
	// migration can take more than six minutes; killing it mid-merge only
	// repeats the work.
	// Readiness remains strict throughout, so the extra liveness grace never
	// exposes a half-started agent.
	return processProbe(600, 30)
}

func (a *Adapter) ReadinessProbe() *corev1.Probe {
	// Either credential satisfies the probe: an agent on its own inference
	// endpoint carries OPENAI_API_KEY and never has OPENROUTER_API_KEY. The
	// adapter guarantees exactly one of them is set, so checking for either
	// still proves a credential reached the container.
	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{
			"/bin/bash", "-c", `[ -n "${OPENROUTER_API_KEY:-}${OPENAI_API_KEY:-}" ] && pgrep -f '[h]ermes.*chat' >/dev/null`,
		}}},
		InitialDelaySeconds: 5,
		PeriodSeconds:       5,
		FailureThreshold:    3,
	}
}

func processProbe(delay, period int32) *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler:        corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"pgrep", "-f", "[h]ermes.*chat"}}},
		InitialDelaySeconds: delay,
		PeriodSeconds:       period,
		FailureThreshold:    3,
	}
}

func (a *Adapter) GracefulShutdownSeconds() int32 { return 45 }
func (a *Adapter) SessionBriefPath() string       { return "/persist/session-brief.json" }
func (a *Adapter) SessionStatePath() string       { return "/persist/session-state.json" }
func (a *Adapter) ModelEnvVar() string            { return "HERMES_INFERENCE_MODEL" }

func (a *Adapter) RestartSessionCommand() []string {
	return []string{"nsenter", "--target", "1", "--mount", "--uts", "--ipc", "--net", "--pid", "--root", "--wd", "--", "/bin/bash", "/persist/last-hermes-launch.sh", "--fresh"}
}

func (a *Adapter) CompactSessionCommand() []string {
	return []string{"nsenter", "--target", "1", "--mount", "--uts", "--ipc", "--net", "--pid", "--root", "--wd", "--", "/usr/sbin/runuser", "-u", "kyber", "--", "/usr/local/bin/kyber-compact-session", "/compress"}
}

// providerID is the Hermes provider name this agent runs against.
func providerID(agent *kyberv1.Agent) string {
	if agent != nil && agent.Spec.Inference != nil {
		return CustomProviderID
	}
	return defaultProviderID
}

// inferenceModel prefers the endpoint-specific model id and falls back to
// spec.model, so an operator who already set spec.model need not repeat it.
func inferenceModel(agent *kyberv1.Agent) string {
	if agent == nil {
		return ""
	}
	if inf := agent.Spec.Inference; inf != nil && inf.Model != "" {
		return inf.Model
	}
	return agent.Spec.Model
}

func (a *Adapter) PreStopCommand() []string                             { return nil }
func (a *Adapter) RuntimeRepair(*kyberv1.Agent) *runtimes.RuntimeRepair { return nil }

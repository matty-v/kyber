package hermes

import (
	"os"

	corev1 "k8s.io/api/core/v1"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/runtimes"
)

const RuntimeImageEnv = "KYBER_HERMES_RUNTIME_IMAGE"

type Adapter struct{ image string }

func NewAdapter() *Adapter       { return &Adapter{image: os.Getenv(RuntimeImageEnv)} }
func (a *Adapter) Type() string  { return Type }
func (a *Adapter) Image() string { return a.image }

func (a *Adapter) EntrypointArgs(*kyberv1.Agent) []string {
	return []string{"/usr/local/bin/start-hermes.sh"}
}

func (a *Adapter) CredentialSecretName(agent *kyberv1.Agent) string {
	if agent == nil || agent.Spec.Secrets.AuthType != kyberv1.AgentAuthTypeAPIKey {
		return ""
	}
	return agent.Name + "-openrouter"
}

func (a *Adapter) EnvVars(agent *kyberv1.Agent) []corev1.EnvVar {
	vars := []corev1.EnvVar{
		{Name: "HERMES_INFERENCE_MODEL", Value: agent.Spec.Model},
		{Name: "HERMES_PROVIDER", Value: "openrouter"},
		{Name: "HERMES_HOME", Value: "/home/kyber/.hermes"},
		{Name: "HERMES_YOLO_MODE", Value: "1"},
		{Name: "HERMES_ACCEPT_HOOKS", Value: "1"},
		{Name: "HERMES_DISABLE_LAZY_INSTALLS", Value: "1"},
		{Name: "OPENROUTER_API_KEY", ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: agent.Name + "-openrouter"},
				Key:                  "token",
			},
		}},
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
	return processProbe(30, 30)
}

func (a *Adapter) ReadinessProbe() *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{
			"/bin/bash", "-c", `[ -n "${OPENROUTER_API_KEY:-}" ] && pgrep -f "hermes.*chat" >/dev/null`,
		}}},
		InitialDelaySeconds: 5,
		PeriodSeconds:       5,
		FailureThreshold:    3,
	}
}

func processProbe(delay, period int32) *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler:        corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"pgrep", "-f", "hermes.*chat"}}},
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

func (a *Adapter) PreStopCommand() []string                             { return nil }
func (a *Adapter) RuntimeRepair(*kyberv1.Agent) *runtimes.RuntimeRepair { return nil }

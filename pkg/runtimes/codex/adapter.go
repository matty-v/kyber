package codex

import (
	"os"

	corev1 "k8s.io/api/core/v1"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/runtimes"
)

const RuntimeImageEnv = "KYBER_CODEX_RUNTIME_IMAGE"

type Adapter struct{ image string }

func NewAdapter() *Adapter      { return &Adapter{image: os.Getenv(RuntimeImageEnv)} }
func (a *Adapter) Type() string { return "codex" }

// CredentialSecretName is keyed on by the NeedsAuth recovery gate. Subscription
// agents use the persisted auth.json Secret; API-key agents use their OpenAI
// key Secret and never enter the interactive login flow.
func (a *Adapter) CredentialSecretName(agent *kyberv1.Agent) string {
	if agent == nil {
		return ""
	}
	if agent.Spec.Secrets.AuthType == kyberv1.AgentAuthTypeAPIKey {
		return agent.Name + "-openai"
	}
	return agent.Name + "-codex-auth"
}

func (a *Adapter) Image() string { return a.image }
func (a *Adapter) EntrypointArgs(*kyberv1.Agent) []string {
	return []string{"/usr/local/bin/start-codex.sh"}
}
func (a *Adapter) EnvVars(agent *kyberv1.Agent) []corev1.EnvVar {
	optional := true
	vars := []corev1.EnvVar{
		{Name: "CODEX_MODEL", Value: agent.Spec.Model},
		{Name: "KYBER_REQUESTED_CODEX_VERSION", Value: agent.Spec.RuntimeVersion},
		{Name: "CODEX_AUTH_JSON", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: agent.Name + "-codex-auth"},
			Key:                  "auth.json", Optional: &optional,
		}}},
	}
	if agent.Spec.Secrets.AuthType == kyberv1.AgentAuthTypeAPIKey {
		vars = append(vars, corev1.EnvVar{Name: "OPENAI_API_KEY", ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: agent.Name + "-openai"},
				Key:                  "token",
			},
		}})
	}
	// Telegram MCP sidecar (kyber#684): same endpoint the Claude Code runtime
	// registers, so both runtimes get one tool surface instead of Codex being
	// told to curl a bare HTTP endpoint.
	if agent.Spec.Secrets.TelegramEnabled {
		vars = append(vars, corev1.EnvVar{
			Name:  "KYBER_TELEGRAM_MCP_URL",
			Value: runtimes.TelegramMCPURL(),
		})
	}
	if agent.Spec.Channels != nil && agent.Spec.Channels.Discord != nil {
		vars = append(vars, corev1.EnvVar{
			Name:  "KYBER_DISCORD_MCP_URL",
			Value: runtimes.DiscordMCPURL(),
		})
	}
	return vars
}
func (a *Adapter) SecretMounts(*kyberv1.Agent) []runtimes.SecretMount { return nil }
func (a *Adapter) LivenessProbe() *corev1.Probe                       { return processProbe(30, 30) }
func (a *Adapter) ReadinessProbe() *corev1.Probe {
	return &corev1.Probe{ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{
		// Kubernetes exec probes start outside PID 1's chroot with HOME=/root.
		// Enter the persisted agent filesystem before asking Codex for its login
		// state; otherwise every healthy subscription agent remains unready. Also
		// reject Kyber's exact {} device marker because Codex 0.146 incorrectly
		// reports that placeholder as logged in.
		Command: []string{"/bin/bash", "-c", `[ -n "${OPENAI_API_KEY:-}" ] || nsenter --target 1 --mount --root --wd -- runuser -u kyber -- bash -c 'marker=$(tr -d "[:space:]" < "/home/kyber/.codex/auth.json" 2>/dev/null || true); [ "$marker" != "{}" ] && codex login status >/dev/null 2>&1'`},
	}}, InitialDelaySeconds: 5, PeriodSeconds: 5, FailureThreshold: 3}
}
func processProbe(delay, period int32) *corev1.Probe {
	return &corev1.Probe{ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{
		Command: []string{"pgrep", "-f", "codex"},
	}}, InitialDelaySeconds: delay, PeriodSeconds: period, FailureThreshold: 3}
}
func (a *Adapter) GracefulShutdownSeconds() int32 { return 30 }
func (a *Adapter) SessionBriefPath() string       { return "/persist/session-brief.json" }
func (a *Adapter) SessionStatePath() string       { return "/persist/session-state.json" }
func (a *Adapter) ModelEnvVar() string            { return "CODEX_MODEL" }
func (a *Adapter) RestartSessionCommand() []string {
	// --fresh mirrors the claude-code adapter: an intentional
	// restart-session always starts a fresh session even when
	// spec.sessionResume is enabled (kyber#118).
	return []string{"nsenter", "--target", "1", "--mount", "--uts", "--ipc", "--net", "--pid", "--root", "--wd", "--", "/bin/bash", "/persist/last-codex-launch.sh", "--fresh"}
}

// CompactSessionCommand pastes "/compact" into the live tmux session. Codex
// uses the same slash command as Claude Code, so both runtimes share the
// in-pod script and differ only in this argv.
//
// The bracketed-paste delivery in kyber-tmux-paste.sh matters more here than
// anywhere else: Codex's TUI is the reason that helper exists, since it can
// absorb a following Enter into a heuristic paste burst.
//
// runuser -u kyber for the same per-uid tmux socket reason as claude-code.
func (a *Adapter) CompactSessionCommand() []string {
	return []string{"nsenter", "--target", "1", "--mount", "--uts", "--ipc", "--net", "--pid", "--root", "--wd", "--", "/usr/sbin/runuser", "-u", "kyber", "--", "/usr/local/bin/kyber-compact-session", "/compact"}
}
func (a *Adapter) PreStopCommand() []string { return nil }

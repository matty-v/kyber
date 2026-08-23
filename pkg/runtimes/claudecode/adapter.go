package claudecode

import (
	"context"
	"os"
	"strconv"

	corev1 "k8s.io/api/core/v1"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/runtimes"
	"github.com/matty-v/kyber/pkg/tokenreport"
)

// ContextWindowResolver returns the operator-supplied context-window for
// a model, plus a "known" flag indicating whether the value came from the
// override map (true) or is the floor default (false). PR-D wires this to
// pkg/contextwindowmap.Resolver. Defined as an interface here so the
// adapter does not depend on that package directly.
type ContextWindowResolver interface {
	LookupOr(ctx context.Context, modelID string) (int64, bool)
}

// SnapshotWindowResolver returns a model's auto-detected context window from
// the runtimedetect detection snapshot (the same data served at
// GET /api/v1/available), plus a "known" flag that is true only when the
// snapshot carries a confident, positive window for the model. kyber#492
// wires this to pkg/runtimedetect.SnapshotResolver. Defined as an interface
// here so the adapter does not depend on that package directly. Best-effort:
// a nil resolver, empty snapshot, cache error, or unknown model resolves to
// (0, false) and the caller falls through to tokenreport.LimitFor.
type SnapshotWindowResolver interface {
	LookupWindow(ctx context.Context, modelID string) (int64, bool)
}

// RequestedCCVersionEnvVar carries the resolved Claude Code version the
// agent's start-claude.sh boot path will install at startup when it
// differs from the baked-in baseline. Validated against the CRD pattern
// + re-validated charset in the shell before any interpolation (kyber#377
// / PR-C). Empty value means "use the baked-in version" — the install
// branch in start-claude.sh skips and behavior is byte-equivalent to the
// pre-PR-C boot path.
const RequestedCCVersionEnvVar = "KYBER_REQUESTED_CC_VERSION"

// ModelContextWindowEnvVar carries the model's context-window size in
// tokens. start-claude.sh consumes this as the data-driven gate for the
// `[1m]` opt-in (replacing the previous hardcoded shell `case` at
// images/claude-code/start-claude.sh:386-395). A value >= 1_000_000
// applies `[1m]`; anything below leaves the model in its 200K variant.
// Unknown model IDs receive the 200K floor from tokenreport.LimitFor
// (kyber#377 / PR-C). PR-D will demote the in-Go fallback list in
// favor of the operator-editable context-window map.
const ModelContextWindowEnvVar = "KYBER_MODEL_CONTEXT_WINDOW"

// AgentRuntimeImageEnv is the env var the control-plane reads to resolve
// the Claude Code runtime image reference. Rendered by the Helm chart from
// image.claudeCode.{repository,tag} in
// deploy/helm/kyber/templates/control-plane/deployment.yaml. Mirrors
// KYBER_STATUS_SIDECAR_IMAGE in pkg/controllers/agent/status_sidecar.go —
// same operator-controlled passthrough, same trust boundary.
const AgentRuntimeImageEnv = "KYBER_AGENT_RUNTIME_IMAGE"

// ClaudeCodeAdapter provides runtime configuration for Claude Code agents.
//
// Secrets are injected via valueFrom.SecretKeyRef env vars (not file mounts).
// The controller expects operators to pre-create the following k8s secrets
// before the Agent CRD is applied:
//
//   - "<agent-name>-oauth" with keys "access_token","refresh_token","expires_at" — required when AuthType="oauth"
//   - "<agent-name>-anthropic" with key "token" — required when AuthType="api-key"
//   - "<agent-name>-telegram" with key "token" — required when TelegramEnabled=true.
//     Consumed by the kyber-mcp-telegram SIDECAR, not by this container (kyber#684).
//   - "<agent-name>-discord" with key "webhook-url" — required when DiscordEnabled=true (kyber#132)
//
// The image reference is resolved at construction time from
// KYBER_AGENT_RUNTIME_IMAGE (see NewClaudeCodeAdapter). Unset → Image()
// returns "" so pod creation fails visibly; we deliberately do NOT fall
// back to a hardcoded tag — that silent fallback (kyber#360 Cause D) is
// what let the cluster drift onto a stale :latest image for two release
// cycles.
type ClaudeCodeAdapter struct {
	// image is the resolved container image reference, captured at
	// construction time. Direct struct literals (test fixtures) leave
	// this empty; production wiring goes through NewClaudeCodeAdapter.
	image string

	// ContextWindows enriches the pod's KYBER_MODEL_CONTEXT_WINDOW env var
	// with values from the operator-editable override map (kyber#378
	// PR-D). When nil, EnvVars falls back to tokenreport.LimitFor, which
	// returns the in-Go knownModels table or the 200K floor for unknown
	// IDs. Assigned in cmd/control-plane/main.go after constructing the
	// adapter; left nil in test fixtures that don't exercise the
	// override path.
	ContextWindows ContextWindowResolver

	// Snapshots resolves the pod's KYBER_MODEL_CONTEXT_WINDOW from the
	// runtimedetect detection snapshot (auto-detected max_input_tokens,
	// kyber#488/#492) — the layer between the operator override map and the
	// in-Go tokenreport.LimitFor floor. When nil (detection disabled or not
	// wired), EnvVars resolves exactly as before. Assigned in
	// cmd/control-plane/main.go after the runtimedetect cache is constructed;
	// left nil in test fixtures that don't exercise the snapshot path.
	Snapshots SnapshotWindowResolver
}

// NewClaudeCodeAdapter constructs a ClaudeCodeAdapter, resolving the
// runtime image from KYBER_AGENT_RUNTIME_IMAGE. Mirrors the sibling
// KYBER_STATUS_SIDECAR_IMAGE pattern in
// pkg/controllers/agent/status_sidecar.go: the chart renders the env from
// image.claudeCode.{repository,tag}, the control-plane reads it once, and
// downstream code uses the captured value. An unset env yields an empty
// Image() — pod creation then fails loudly at admission time rather than
// drifting onto a hardcoded :latest (the kyber#360 Cause D regression).
func NewClaudeCodeAdapter() *ClaudeCodeAdapter {
	return &ClaudeCodeAdapter{image: os.Getenv(AgentRuntimeImageEnv)}
}

// Type returns the runtime identifier for Claude Code agents.
func (a *ClaudeCodeAdapter) Type() string { return "claude-code" }

// CredentialSecretName returns the Secret carrying this agent's model
// credential, per the AuthType convention documented on the struct above.
// Keyed on by the NeedsAuth recovery gate (kyber#684) so a re-auth is
// detectable; an unrecognised auth type returns "" and simply disables the
// gate rather than guessing at a Secret name.
func (a *ClaudeCodeAdapter) CredentialSecretName(agent *kyberv1.Agent) string {
	if agent == nil {
		return ""
	}
	switch agent.Spec.Secrets.AuthType {
	case kyberv1.AgentAuthTypeOAuth:
		return agent.Name + "-oauth"
	case kyberv1.AgentAuthTypeAPIKey:
		return agent.Name + "-anthropic"
	default:
		return ""
	}
}

// Image returns the container image reference for this runtime, resolved
// at construction from KYBER_AGENT_RUNTIME_IMAGE. See NewClaudeCodeAdapter.
func (a *ClaudeCodeAdapter) Image() string {
	return a.image
}

// EntrypointArgs returns the arguments passed to the agent container's entrypoint.
// The agent-base image entrypoint sets up the overlay; start-claude.sh is exec'd inside the chroot.
func (a *ClaudeCodeAdapter) EntrypointArgs(agent *kyberv1.Agent) []string {
	return []string{"/usr/local/bin/start-claude.sh"}
}

// EnvVars returns the environment variables to inject into the Claude Code agent container.
//
// Always injected:
//   - CLAUDE_MODEL — the LLM model identifier from agent.Spec.Model
//
// Injected based on AuthType:
//   - When AuthType is "oauth", three env vars are injected from the <agent-name>-oauth secret:
//   - CLAUDE_ACCESS_TOKEN — PKCE-OAuth exchange; key "access_token"
//     (start-claude.sh uses this to write .credentials.json)
//   - CLAUDE_REFRESH_TOKEN — PKCE-OAuth exchange; key "refresh_token"
//     (start-claude.sh refreshes using this token before writing .credentials.json)
//   - CLAUDE_ACCESS_TOKEN_EXPIRES_AT — PKCE-OAuth exchange; key "expires_at"
//     (start-claude.sh uses this to skip the Anthropic refresh when the token is still valid)
//   - ANTHROPIC_API_KEY (valueFrom SecretKeyRef) — when AuthType is "api-key"
//     Secret name: <agent-name>-anthropic, key: "token"
//
// Injected when TelegramEnabled is true (kyber#684):
//   - KYBER_TELEGRAM_MCP_URL — the sidecar's loopback MCP endpoint. The bot
//     token is NOT injected: the sidecar owns the channel, and withholding the
//     credential is what makes it impossible for the native plugin to poll the
//     same bot and 409 against it.
//
// Injected when DiscordEnabled is true (kyber#132 Phase 1):
//   - KYBER_DISCORD_WEBHOOK (valueFrom SecretKeyRef) — when agent.Spec.Secrets.DiscordEnabled is true
//     Secret name: <agent-name>-discord, key: "webhook-url". Outbound-only:
//     the agent shells `curl -X POST -H 'Content-Type: application/json'
//     -d '{"content":"..."}' "$KYBER_DISCORD_WEBHOOK"` to ping a Discord
//     channel. The Content-Type header is required — without it Discord
//     returns 400. Bidirectional comms wait on kyber#138.
//
// Note: AGENT_NAME is injected by pod_builder unconditionally and is intentionally
// omitted here to avoid duplicate env var entries in the pod spec.
func (a *ClaudeCodeAdapter) EnvVars(agent *kyberv1.Agent) []corev1.EnvVar {
	// Resolve the model's context-window size in four layers, lowest
	// precedence first (each known layer overrides the one below):
	//
	//   4. tokenreport.LimitFor  — in-Go knownModels table / 200K floor for
	//      unknown IDs (cold-start safety; a brand-new model floors rather
	//      than crashing, and simply doesn't get `[1m]` until detected).
	//   3. detection snapshot    — auto-detected max_input_tokens (kyber#492),
	//      so a new 1M model gets `[1m]` with no ConfigMap edit. Best-effort:
	//      empty/error/unknown snapshot falls through, never blocks.
	//   1. operator override map — `kubectl edit cm kyber-model-context-windows`
	//      (kyber#378). Stays on top: explicit human intent, and its 30s TTL
	//      reflects an operator edit faster than the hourly detection poll —
	//      matching the poller's own override-on-top-of-detection order.
	//
	// Applied bottom-up so the highest known layer wins.
	contextWindow := tokenreport.LimitFor(agent.Spec.Model)
	if agent.Spec.Model != "" {
		if a.Snapshots != nil {
			if cw, known := a.Snapshots.LookupWindow(context.Background(), agent.Spec.Model); known {
				contextWindow = cw
			}
		}
		if a.ContextWindows != nil {
			if cw, known := a.ContextWindows.LookupOr(context.Background(), agent.Spec.Model); known {
				contextWindow = cw
			}
		}
	}

	vars := []corev1.EnvVar{
		{Name: a.ModelEnvVar(), Value: agent.Spec.Model},
		// PR-C wiring: empty RuntimeVersion means "no override" — the
		// shell install branch skips and boots on the baked-in pin.
		{Name: RequestedCCVersionEnvVar, Value: agent.Spec.RuntimeVersion},
		{Name: ModelContextWindowEnvVar, Value: strconv.FormatInt(contextWindow, 10)},
	}

	switch agent.Spec.Secrets.AuthType {
	case kyberv1.AgentAuthTypeOAuth:
		optional := true
		// PKCE-OAuth multi-key path — access_token + refresh_token from the code exchange.
		// start-claude.sh uses CLAUDE_REFRESH_TOKEN to refresh and write .credentials.json.
		vars = append(vars, corev1.EnvVar{
			Name: "CLAUDE_ACCESS_TOKEN",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: agent.Name + "-oauth"},
					Key:                  "access_token",
					Optional:             &optional,
				},
			},
		})
		vars = append(vars, corev1.EnvVar{
			Name: "CLAUDE_REFRESH_TOKEN",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: agent.Name + "-oauth"},
					Key:                  "refresh_token",
					Optional:             &optional,
				},
			},
		})
		vars = append(vars, corev1.EnvVar{
			Name: "CLAUDE_ACCESS_TOKEN_EXPIRES_AT",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: agent.Name + "-oauth"},
					Key:                  "expires_at",
					Optional:             &optional,
				},
			},
		})
	case kyberv1.AgentAuthTypeAPIKey:
		vars = append(vars, corev1.EnvVar{
			Name: "ANTHROPIC_API_KEY",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: agent.Name + "-anthropic",
					},
					Key: "token",
				},
			},
		})
	}

	// Telegram (kyber#684): the SIDECAR owns this channel now, for every runtime.
	//
	// The bot token is deliberately NOT injected here any more. That is the
	// mutual exclusion, and it is structural rather than advisory: the native
	// Claude Code plugin cannot poll getUpdates without a token, so it is
	// impossible for the plugin and the sidecar to both hold the slot. Two
	// pollers on one token is a permanent 409 storm — the exact failure
	// kyber#678/#679 chased, where whichever side won a boot race decided
	// whether the agent could hear Telegram at all. A flag would have left that
	// possible; removing the credential does not.
	//
	// What the agent gets instead is the sidecar's MCP endpoint, so it keeps a
	// real tool surface (reply/react/edit_message/download_attachment). The
	// start script registers it with `claude mcp add --transport http`.
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

	// kyber#132 Phase 1 — Discord webhook for outbound notifications.
	// Same SecretKeyRef shape as Telegram; the key is "webhook-url" (not
	// "token") because the value is a URL, not a bearer token. The agent's
	// shell calls `curl "$KYBER_DISCORD_WEBHOOK"` to post.
	if agent.Spec.Secrets.DiscordEnabled {
		vars = append(vars, corev1.EnvVar{
			Name: "KYBER_DISCORD_WEBHOOK",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: agent.Name + "-discord",
					},
					Key: "webhook-url",
				},
			},
		})
	}

	return vars
}

// SecretMounts returns the secret volume mounts for the Claude Code agent.
// Claude Code uses env var injection (valueFrom SecretKeyRef) for all secrets,
// so no CSI file mounts are needed.
func (a *ClaudeCodeAdapter) SecretMounts(agent *kyberv1.Agent) []runtimes.SecretMount {
	return []runtimes.SecretMount{}
}

// LivenessProbe returns the probe used to health-check the Claude Code agent container.
// It checks that the claude process is running via pgrep.
func (a *ClaudeCodeAdapter) LivenessProbe() *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			Exec: &corev1.ExecAction{
				Command: []string{"pgrep", "-f", "claude"},
			},
		},
		InitialDelaySeconds: 30,
		PeriodSeconds:       30,
		FailureThreshold:    3,
	}
}

// ReadinessProbe returns the probe used to check if the Claude Code agent is ready.
// It uses the same pgrep check as the liveness probe but with a shorter initial delay,
// so the pod is marked ready once claude is confirmed running. The spec does not define
// a separate readiness probe for Claude Code; pgrep is the most reliable check available
// without adding a custom healthcheck endpoint to the Claude Code image.
//
// TODO: InitialDelaySeconds=15 is aggressive vs liveness=30. If Claude Code
// startup (node.js init + OAuth handshake) consistently takes >15s, pods may
// flap ready/not-ready before settling. Tune after observing real boot times.
func (a *ClaudeCodeAdapter) ReadinessProbe() *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			Exec: &corev1.ExecAction{
				Command: []string{"pgrep", "-f", "claude"},
			},
		},
		InitialDelaySeconds: 15,
		PeriodSeconds:       10,
		FailureThreshold:    3,
	}
}

// GracefulShutdownSeconds returns the time to wait for graceful termination.
func (a *ClaudeCodeAdapter) GracefulShutdownSeconds() int32 { return 30 }

// SessionBriefPath returns the path where the init container writes the session brief.
func (a *ClaudeCodeAdapter) SessionBriefPath() string { return "/persist/session-brief.json" }

// SessionStatePath returns the path where the agent may write session state before shutdown.
func (a *ClaudeCodeAdapter) SessionStatePath() string { return "/persist/session-state.json" }

// ModelEnvVar returns the environment variable name used to set the LLM model.
func (a *ClaudeCodeAdapter) ModelEnvVar() string { return "CLAUDE_MODEL" }

// RestartSessionCommand returns the argv for kyber-restart-session — the
// re-runnable script start-claude.sh dumps to /persist on every boot (#128).
// The dumped script acquires /persist/var/lock/session.lock (coordinating
// with #135's job dispatcher so cron fires during the kill+relaunch window
// skip cleanly), kills the tmux "agent" session, and re-runs the identical
// `tmux new-session` invocation with the resolved CLAUDE_ARGS baked in at
// boot time.
//
// Wrapped in `nsenter --target 1 …` so the exec enters PID 1's chroot —
// the /persist bind on the chroot's view is what exposes the dumped script
// to this command (the agent's tmux socket also lives inside the chroot).
// Same namespace set as routes_exec.go's shell mode.
func (a *ClaudeCodeAdapter) RestartSessionCommand() []string {
	return []string{
		"nsenter", "--target", "1",
		"--mount", "--uts", "--ipc", "--net", "--pid",
		"--root", "--wd",
		"--",
		// --fresh: an intentional restart-session always starts a fresh
		// session, even when spec.sessionResume is enabled — restart is
		// the operator's context-clearing tool (kyber#118). The crash
		// watchdog calls the same script WITHOUT --fresh, which is where
		// resume applies.
		"/bin/bash", "/persist/last-claude-launch.sh", "--fresh",
	}
}

// CompactSessionCommand returns the argv for kyber-compact-session, which
// pastes "/compact" into the live tmux session. Claude Code treats that as
// the in-TUI compaction command; there is no headless equivalent for an
// already-running session, so delivering the keystrokes IS the API.
//
// Same `nsenter --target 1 …` wrapper as RestartSessionCommand — the tmux
// socket lives inside PID 1's chroot. The extra `runuser -u kyber` is
// required on top of it: the exec lands as root, and tmux resolves its
// default socket path per-uid (/tmp/tmux-0 for root, /tmp/tmux-1001 for the
// agent), so a root invocation would report "no server running" against a
// perfectly healthy session.
func (a *ClaudeCodeAdapter) CompactSessionCommand() []string {
	return []string{
		"nsenter", "--target", "1",
		"--mount", "--uts", "--ipc", "--net", "--pid",
		"--root", "--wd",
		"--",
		"/usr/sbin/runuser", "-u", "kyber", "--",
		"/usr/local/bin/kyber-compact-session", "/compact",
	}
}

// PreStopCommand SIGTERMs the Telegram channel plugin (`bun server.ts`) so it
// releases Telegram's single getUpdates slot via bot.stop() before the pod is
// torn down. Without this, the plugin — a grandchild of the *detached* tmux
// "agent" session — never sees the pod's SIGTERM (it lands on PID 1, which
// doesn't forward to the tmux daemon) and is SIGKILLed with the slot still held.
// The next pod's bot then hits 409 Conflict, retries only ~28s before exiting,
// and the channel stays dead until a manual /reload-plugins. See the interface
// doc on Adapter.PreStopCommand.
//
// Wrapped in the same `nsenter --target 1 …` namespace set as
// RestartSessionCommand (and routes_exec.go's shell mode) so pkill sees PID 1's
// process view — proven-good for in-pod session manipulation in this runtime.
// The pattern matches the poller (`… bun server.ts`) but not its `bun run …
// start` parent; `|| true` keeps the hook non-fatal when the plugin is already
// gone; the short sleep lets bot.stop() complete its close round-trip to
// Telegram before the grace period ends (GracefulShutdownSeconds is 30s).
//
// This fires only on graceful termination (reboot, rollout, eviction), which is
// exactly the reported failure case; a hard node crash skips preStop, but there
// the plugin's own 409 retry eventually recovers once the dead poll times out.
func (a *ClaudeCodeAdapter) PreStopCommand() []string {
	return []string{
		"nsenter", "--target", "1",
		"--mount", "--uts", "--ipc", "--net", "--pid",
		"--root", "--wd",
		"--",
		"/bin/sh", "-c",
		"pkill -TERM -f 'bun .*server\\.ts' || true; sleep 3",
	}
}

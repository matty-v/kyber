// Discord channel-sidecar injection for agent pods (kyber#646). Adds the
// kyber-mcp-discord container to the pod spec built by BuildPodSpec when the
// agent enables the Discord channel (spec.channels.discord).
//
// The sidecar holds the Discord Gateway and HMAC-forwards allowlisted messages
// to the agent's `discord` inbound binding — reusing the kyber#208 inbound
// rail (same wake/dedup/envelope path as Telegram). Kept separate from
// pod_builder.go for the same reasons as status_sidecar.go: testable in
// isolation, and platform-level so any runtime gets it for free.
package agent

import (
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/matty-v/kyber/pkg/runtimes"
)

const (
	// DiscordSidecarContainerName is the container name in the pod spec.
	DiscordSidecarContainerName = "kyber-mcp-discord"
	// discordSidecarHealthPort matches KYBER_DISCORD_HEALTH_ADDR's default
	// (:14002) in cmd/kyber-mcp-discord/main.go.
	discordSidecarHealthPort int32 = 14002
	// DiscordInboundBindingName is the binding the sidecar forwards to and the
	// controller auto-renders when the Discord channel is enabled.
	DiscordInboundBindingName       = "discord"
	DiscordConfigRevisionAnnotation = "kyber.io/discord-config-revision"
)

// DiscordSidecarConfig carries what the controller threads into the sidecar's
// pod-spec env. Image gates injection (empty → no-op, like the status sidecar);
// ExistingSecret names the per-agent Secret holding bot-token + webhook-secret
// (+ optional allowlist keys).
type DiscordSidecarConfig struct {
	AgentName      string
	Image          string
	ExistingSecret string
	LogLevel       string
	// MentionOnly mirrors spec.channels.discord.mentionOnly: forward only
	// messages that address the bot (@-mention or a reply to the bot).
	MentionOnly bool
}

// AppendDiscordSidecar appends the kyber-mcp-discord container to pod's
// Containers slice. No-op when cfg.Image or cfg.ExistingSecret is empty.
//
// Credentials never touch the runtime container: the bot token and HMAC secret
// are injected here, into the sidecar only, via SecretKeyRef. The allowlist +
// guild/channel come from optional keys in the same Secret. The inbound URL is
// the in-cluster control plane; the sidecar posts to
// /webhooks/inbound/<agent>/discord.
func AppendDiscordSidecar(spec *corev1.PodSpec, cfg DiscordSidecarConfig) {
	if cfg.Image == "" || cfg.ExistingSecret == "" {
		return
	}

	// Optional Secret keys resolve to empty env when absent (optional:true), so
	// a Secret carrying only bot-token + webhook-secret is valid — the sidecar
	// then falls back to its own defaults (any guild/channel, deny-all users).
	secretEnv := func(name, key string, optional bool) corev1.EnvVar {
		o := optional
		return corev1.EnvVar{
			Name: name,
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: cfg.ExistingSecret},
					Key:                  key,
					Optional:             &o,
				},
			},
		}
	}

	env := []corev1.EnvVar{
		{Name: "KYBER_AGENT_NAME", Value: cfg.AgentName},
		{Name: "KYBER_LOG_LEVEL", Value: cfg.LogLevel},
		{Name: "KYBER_INBOUND_BINDING", Value: DiscordInboundBindingName},
		{Name: "KYBER_INBOUND_URL", Value: controlPlanePublicURL()},
		{Name: "KYBER_INBOUND_SIGNATURE_HEADER", Value: "X-Kyber-Signature-256"},
		{Name: "KYBER_INBOUND_SIGNATURE_PREFIX", Value: "sha256="},
		{Name: "KYBER_DISCORD_MCP_ADDR", Value: runtimes.DiscordMCPAddr()},
		{Name: "KYBER_DISCORD_DOWNLOAD_DIR", Value: runtimes.DiscordAttachmentDir},
		secretEnv("DISCORD_BOT_TOKEN", "bot-token", false),
		secretEnv("KYBER_INBOUND_HMAC_SECRET", "webhook-secret", false),
		secretEnv("DISCORD_ALLOWED_GUILD_IDS", "guild-id", true),
		secretEnv("DISCORD_ALLOWED_CHANNEL_IDS", "channel-id", true),
		secretEnv("DISCORD_ALLOWED_USER_IDS", "allowed-user-ids", true),
	}
	env = append(env, loggingContextEnv(DiscordSidecarContainerName)...)
	if cfg.MentionOnly {
		// Only set when enabled — an absent var is the sidecar's default (off),
		// so an older sidecar image ignores it rather than misreading "false".
		env = append(env, corev1.EnvVar{Name: "DISCORD_MENTION_ONLY", Value: "true"})
	}

	probe := &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{
				Path: "/healthz",
				Port: intstr.FromInt32(discordSidecarHealthPort),
			},
		},
		InitialDelaySeconds: 10,
		PeriodSeconds:       30,
		FailureThreshold:    3,
	}

	container := corev1.Container{
		Name:  DiscordSidecarContainerName,
		Image: cfg.Image,
		Env:   env,
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("10m"),
				corev1.ResourceMemory: resource.MustParse("48Mi"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("100m"),
				corev1.ResourceMemory: resource.MustParse("96Mi"),
			},
		},
		VolumeMounts: []corev1.VolumeMount{{Name: "persist", MountPath: "/persist"}},
		SecurityContext: &corev1.SecurityContext{
			RunAsUser:                ptrTo(int64(0)),
			ReadOnlyRootFilesystem:   ptrTo(true),
			AllowPrivilegeEscalation: ptrTo(false),
		},
		LivenessProbe:  probe,
		ReadinessProbe: probe.DeepCopy(),
		// SIGTERM in main() closes the Gateway cleanly; give it room before the
		// pod's grace period expires so Discord sees a clean disconnect.
		Lifecycle: nil,
	}

	spec.Containers = append(spec.Containers, container)
}

// DefaultDiscordAction is the instruction text rendered into every Discord
// envelope. It has to be self-sufficient: this text is where the agent learns
// how to answer through the sidecar without receiving Discord credentials.
//
// It lives here rather than in pkg/api because two callers need it — the comms
// API when wiring a channel, and the reconciler when migrating an agent that
// was wired before the bot token moved out of the runtime container.
func DefaultDiscordAction(mentionOnly bool) string {
	var b strings.Builder
	b.WriteString("Someone messaged you on Discord (details below). Reply conversationally.\n\n")
	b.WriteString("Reply with the kyber-discord MCP server's reply tool, using channel_id and message_id from the message below. ")
	b.WriteString("If attachments are listed, use download_attachment with attachment_id; attach files in your reply with absolute paths under /persist. ")
	b.WriteString("If that tool is unavailable, run this compatibility fallback in your shell — replace REPLY_TEXT with your message, ")
	b.WriteString("CHANNEL_ID with channel_id, and MESSAGE_ID with message_id shown below:\n")
	b.WriteString(`curl -sS -X POST -H "Content-Type: application/json" ` +
		`-d '{"channel_id":"CHANNEL_ID","content":"REPLY_TEXT","message_id":"MESSAGE_ID"}' ` +
		`http://127.0.0.1:14005/send` + "\n\n")
	b.WriteString("Keep replies short and conversational. Discord credentials stay in the sidecar; do not look for a bot token.")
	if mentionOnly {
		b.WriteString("\nYou only receive messages that @-mention you or reply to one of your own messages, ")
		b.WriteString("so every message you get is meant for you — always reply.")
	}
	return b.String()
}

// IsLegacyDiscordDefaultAction reports whether an action still instructs the
// agent to call Discord's REST API directly with DISCORD_BOT_TOKEN. That env
// var is no longer injected into the runtime, so such an action can only fail.
//
// The match is deliberately loose on the URL: any mention of the bot token
// paired with a direct discord.com API call is stale, whatever API version the
// text names. An operator's hand-tuned action that does NOT reference the token
// is left alone — it may well drive the loopback endpoint correctly.
func IsLegacyDiscordDefaultAction(action string) bool {
	directREST := strings.Contains(action, "DISCORD_BOT_TOKEN") &&
		strings.Contains(action, "discord.com/api")
	generatedHTTPFallback := strings.Contains(action, "Someone messaged you on Discord (details below). Reply conversationally.") &&
		strings.Contains(action, "http://127.0.0.1:14005/send") &&
		strings.Contains(action, "Keep replies short and conversational.") &&
		!strings.Contains(action, "kyber-discord")
	generatedMCPWithoutAttachments := strings.Contains(action, "Someone messaged you on Discord (details below). Reply conversationally.") &&
		strings.Contains(action, "kyber-discord") && strings.Contains(action, "Keep replies short and conversational.") &&
		!strings.Contains(action, "download_attachment")
	return directREST || generatedHTTPFallback || generatedMCPWithoutAttachments
}

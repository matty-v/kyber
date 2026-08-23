package agent

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/intstr"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	pkgruntimes "github.com/matty-v/kyber/pkg/runtimes"
)

const (
	TelegramSidecarContainerName = "kyber-mcp-telegram"
	TelegramInboundBindingName   = "telegram"
	telegramSidecarHealthPort    = 14003

	// Keys on the per-agent "<name>-telegram" Secret. The sidecar reads all
	// three; before kyber#684 a Claude Code agent's Secret carried only the
	// token, because the in-process plugin needed nothing else.
	TelegramTokenKey          = "token"
	TelegramAllowedUserIDsKey = "allowed-user-ids"
	TelegramWebhookSecretKey  = "webhook-secret"
)

// DefaultTelegramAction is the instruction an agent receives with each inbound
// Telegram message. It lives here rather than in pkg/api for the same reason
// DefaultDiscordAction does: both the comms API (writing a new binding) and the
// controller (healing an old one) have to produce the same text.
//
// Prefers the MCP tool over curl since kyber#684 — every runtime now has the
// sidecar's tool surface registered, and the tools handle formatting,
// attachments, reactions and edits that a bare /send POST cannot. The curl form
// stays documented because /send is still there and still works.
func DefaultTelegramAction() string {
	return "Telegram sent you a message, callback, reaction, or album (details below). Respond appropriately. " +
		"If attachment_file_id is present, use the kyber-telegram MCP `download_attachment` tool " +
		"with that file ID before responding. For an album, attachments is a JSON array; download each file_id you need. " +
		"For choices, the `reply` tool accepts buttons with text and an internal value; callback_value returns the selected value. " +
		"After handling a callback, edit the original message with buttons: [] to remove its keyboard. " +
		"Use the bundled telegram-messaging skill for richer interaction patterns when relevant. " +
		"Reply with the kyber-telegram MCP `reply` tool, passing the chat_id below. " +
		"If that tool is unavailable, fall back to: curl -sS -X POST http://127.0.0.1:14004/send " +
		"-d chat_id=CHAT_ID --data-urlencode text=REPLY_TEXT. Keep replies concise."
}

// interactiveTelegramAction is the exact default introduced with callbacks,
// reactions, and albums, before the bundled cookbook taught keyboard cleanup.
func interactiveTelegramAction() string {
	return "Telegram sent you a message, callback, reaction, or album (details below). Respond appropriately. " +
		"If attachment_file_id is present, use the kyber-telegram MCP `download_attachment` tool " +
		"with that file ID before responding. For an album, attachments is a JSON array; download each file_id you need. " +
		"For choices, the `reply` tool accepts buttons with text and an internal value; callback_value returns the selected value. " +
		"Reply with the kyber-telegram MCP `reply` tool, passing the chat_id below. " +
		"If that tool is unavailable, fall back to: curl -sS -X POST http://127.0.0.1:14004/send " +
		"-d chat_id=CHAT_ID --data-urlencode text=REPLY_TEXT. Keep replies concise."
}

// attachmentTelegramAction is the exact default introduced with attachment
// support and superseded by callbacks, reactions, and albums. It is safe to
// upgrade only this exact value; operator-authored instructions are preserved.
func attachmentTelegramAction() string {
	return "Someone messaged you on Telegram (details below). Reply conversationally. " +
		"If attachment_file_id is present, use the kyber-telegram MCP `download_attachment` tool " +
		"with that file ID before responding. " +
		"Reply with the kyber-telegram MCP `reply` tool, passing the chat_id below. " +
		"If that tool is unavailable, fall back to: curl -sS -X POST http://127.0.0.1:14004/send " +
		"-d chat_id=CHAT_ID --data-urlencode text=REPLY_TEXT. Keep replies concise."
}

// legacyTelegramAction is the exact pre-attachment default. Reconciliation may
// safely upgrade this value while leaving operator-authored actions untouched.
func legacyTelegramAction() string {
	return "Someone messaged you on Telegram (details below). Reply conversationally. " +
		"Reply with the kyber-telegram MCP `reply` tool, passing the chat_id below. " +
		"If that tool is unavailable, fall back to: curl -sS -X POST http://127.0.0.1:14004/send " +
		"-d chat_id=CHAT_ID --data-urlencode text=REPLY_TEXT. Keep replies concise."
}

// TelegramInboundBinding builds the signed inbound binding the sidecar POSTs
// into. Shared by the comms API and the kyber#684 migration so a healed agent
// and a freshly-configured one are byte-identical.
func TelegramInboundBinding(secretName, action string) kyberv1.AgentInboundBinding {
	return kyberv1.AgentInboundBinding{
		Name: TelegramInboundBindingName, ExistingSecret: secretName,
		SignatureHeader: "X-Kyber-Signature-256", SignaturePrefix: "sha256=", Action: action,
		Fields: []kyberv1.AgentInboundField{
			{Label: "event_type", JsonPath: "$.event_type"},
			{Label: "from", JsonPath: "$.user"}, {Label: "chat_id", JsonPath: "$.chat_id"},
			{Label: "message_id", JsonPath: "$.message_id"},
			{Label: "message", JsonPath: "$.content"},
			{Label: "attachment_type", JsonPath: "$.attachment_type"},
			{Label: "attachment_file_id", JsonPath: "$.attachment_file_id"},
			{Label: "attachment_name", JsonPath: "$.attachment_name"},
			{Label: "media_group_id", JsonPath: "$.media_group_id"},
			{Label: "attachments", JsonPath: "$.attachments"},
			{Label: "callback_label", JsonPath: "$.callback_label"},
			{Label: "callback_value", JsonPath: "$.callback_value"},
			{Label: "reaction_old", JsonPath: "$.reaction_old"},
			{Label: "reaction_new", JsonPath: "$.reaction_new"},
		},
	}
}

type TelegramSidecarConfig struct {
	AgentName      string
	Image          string
	ExistingSecret string
	LogLevel       string
}

// AppendTelegramSidecar adds the runtime-neutral Telegram polling bridge.
// Credentials are scoped to this sidecar and never mounted into the runtime.
//
// It adds NO new volume: it reuses the existing "persist" PVC volume added by
// pod_builder, mounted at the SAME path the agent container uses. That shared
// path is load-bearing, not tidiness — the download_attachment tool returns a
// path to the model and the model then reads it, so a sidecar-local download
// directory would mean every inbound photo lands somewhere the agent cannot
// see. Same precedent as the session-saver (kyber#639).
func AppendTelegramSidecar(spec *corev1.PodSpec, cfg TelegramSidecarConfig) {
	if cfg.Image == "" || cfg.ExistingSecret == "" {
		return
	}
	// optional mirrors the Discord sidecar's helper (kyber#646). A required
	// SecretKeyRef whose key is absent does not degrade the channel — the
	// kubelet refuses to start the CONTAINER, so the pod never becomes ready and
	// the whole agent is down.
	//
	// That distinction became load-bearing in kyber#684. Every pre-existing
	// Claude Code agent's "<name>-telegram" Secret holds only the bot token,
	// because the in-process plugin needed nothing else. Un-gating the sidecar
	// with all three keys required would have taken those agents down at their
	// next pod recreation — a broken channel turned into a broken agent.
	// migrateLegacyTelegramSecret backfills the missing keys, and these flags
	// are the belt to its braces: only the bot token is genuinely un-substitutable.
	secretEnv := func(name, key string, optional bool) corev1.EnvVar {
		return corev1.EnvVar{Name: name, ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: cfg.ExistingSecret}, Key: key,
			Optional: &optional,
		}}}
	}
	probe := &corev1.Probe{
		ProbeHandler:        corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/healthz", Port: intstr.FromInt(telegramSidecarHealthPort)}},
		InitialDelaySeconds: 10, PeriodSeconds: 30, FailureThreshold: 3,
	}
	spec.Containers = append(spec.Containers, corev1.Container{
		Name: TelegramSidecarContainerName, Image: cfg.Image,
		Env: append([]corev1.EnvVar{
			{Name: "KYBER_AGENT_NAME", Value: cfg.AgentName},
			{Name: "KYBER_LOG_LEVEL", Value: cfg.LogLevel},
			{Name: "KYBER_INBOUND_BINDING", Value: TelegramInboundBindingName},
			{Name: "KYBER_INBOUND_URL", Value: controlPlanePublicURL()},
			{Name: "KYBER_TELEGRAM_MCP_ADDR", Value: pkgruntimes.TelegramMCPAddr()},
			// Set explicitly rather than left to the binary's default: this path
			// must match what the agent container sees, so it is a contract
			// between two containers and belongs in the pod spec where that is
			// visible.
			{Name: "KYBER_TELEGRAM_DOWNLOAD_DIR", Value: pkgruntimes.TelegramAttachmentDir},
			// The token is the one key with no fallback: without it the sidecar
			// cannot poll at all, so a missing token SHOULD stop the container.
			secretEnv("TELEGRAM_BOT_TOKEN", TelegramTokenKey, false),
			secretEnv("TELEGRAM_ALLOWED_USER_IDS", TelegramAllowedUserIDsKey, true),
			secretEnv("KYBER_INBOUND_HMAC_SECRET", TelegramWebhookSecretKey, true),
		}, loggingContextEnv(TelegramSidecarContainerName)...),
		VolumeMounts: []corev1.VolumeMount{
			{Name: "persist", MountPath: "/persist"},
		},
		// RunAsUser:0, same reason as the transcript-tailer and session-saver:
		// the pod sets no fsGroup, so the persist PVC root stays root-owned and
		// the sidecar's own distroless "nonroot" uid (65532) could not even
		// create the attachment directory, let alone hand the agent (uid 1001)
		// something readable. Writing as root with world-readable modes is what
		// makes the file legible across the container boundary — the failure
		// mode kyber#684's workstream 1 spent a whole agent bring-up on.
		// Posture otherwise stays tight, and RunAsNonRoot without RunAsUser
		// bricks admission (kyber#451), so pin it explicitly.
		SecurityContext: &corev1.SecurityContext{
			RunAsUser:                ptrTo(int64(0)),
			ReadOnlyRootFilesystem:   ptrTo(true),
			AllowPrivilegeEscalation: ptrTo(false),
		},
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m"), corev1.ResourceMemory: resource.MustParse("32Mi")},
			Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("64Mi")},
		},
		LivenessProbe: probe, ReadinessProbe: probe,
	})
}

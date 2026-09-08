package agent

import (
	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/runtimes"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/intstr"
)

const SlackSidecarContainerName = "kyber-mcp-slack"
const SlackInboundBindingName = "slack"
const SlackConfigRevisionAnnotation = "kyber.io/slack-config-revision"

type SlackSidecarConfig struct {
	AgentName, Image, ExistingSecret, LogLevel string
}

// DefaultSlackAction is deliberately self-contained: the binding is also
// synthesized by the reconciler for agents enabled directly through the CRD.
func DefaultSlackAction() string {
	return "Someone messaged you on Slack (details below). Reply conversationally using the kyber-slack MCP reply tool with channel_id and thread_ts. If attachments are listed, use download_attachment with file_id; attach outbound files using absolute paths under /persist. For callbacks, use callback_label and callback_value and respond appropriately. The reply tool can send Block Kit buttons. Keep replies concise."
}

func IsLegacySlackDefaultAction(action string) bool {
	return action == "Someone messaged you on Slack (details below). Reply conversationally using the kyber-slack MCP reply tool with channel_id and thread_ts. Keep replies concise."
}

func SlackInboundBinding(secretName, action string) kyberv1.AgentInboundBinding {
	return kyberv1.AgentInboundBinding{Name: SlackInboundBindingName, ExistingSecret: secretName,
		SignatureHeader: "X-Kyber-Signature-256", SignaturePrefix: "sha256=", Action: action,
		Fields: []kyberv1.AgentInboundField{
			{Label: "event_type", JsonPath: "$.event_type"}, {Label: "from", JsonPath: "$.user"},
			{Label: "user_id", JsonPath: "$.user_id"}, {Label: "channel_id", JsonPath: "$.channel_id"},
			{Label: "message_id", JsonPath: "$.message_id"}, {Label: "message", JsonPath: "$.content"},
			{Label: "thread_ts", JsonPath: "$.thread_id"},
			{Label: "attachments", JsonPath: "$.attachments"},
			{Label: "callback_label", JsonPath: "$.callback_label"},
			{Label: "callback_value", JsonPath: "$.callback_value"},
		},
	}
}

func AppendSlackSidecar(spec *corev1.PodSpec, cfg SlackSidecarConfig) {
	if cfg.Image == "" || cfg.ExistingSecret == "" {
		return
	}
	secret := func(name, key string, optional bool) corev1.EnvVar {
		return corev1.EnvVar{Name: name, ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: cfg.ExistingSecret}, Key: key, Optional: &optional}}}
	}
	probe := &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/healthz", Port: intstr.FromInt32(14009)}}, InitialDelaySeconds: 10, PeriodSeconds: 30, FailureThreshold: 3}
	env := append([]corev1.EnvVar{{Name: "KYBER_AGENT_NAME", Value: cfg.AgentName}, {Name: "KYBER_LOG_LEVEL", Value: cfg.LogLevel}, {Name: "KYBER_INBOUND_BINDING", Value: SlackInboundBindingName}, {Name: "KYBER_INBOUND_URL", Value: controlPlanePublicURL()}, {Name: "KYBER_SLACK_MCP_ADDR", Value: runtimes.SlackMCPAddr()}, {Name: "KYBER_SLACK_DOWNLOAD_DIR", Value: runtimes.SlackAttachmentDir}, secret("SLACK_BOT_TOKEN", "bot-token", false), secret("SLACK_APP_TOKEN", "app-token", false), secret("KYBER_INBOUND_HMAC_SECRET", "webhook-secret", true), secret("SLACK_ALLOWED_USER_IDS", "allowed-user-ids", true), secret("SLACK_ALLOWED_CHANNEL_IDS", "allowed-channel-ids", true)}, loggingContextEnv(SlackSidecarContainerName)...)
	env = append(env, secret("SLACK_MENTION_ONLY", "mention-only", true))
	spec.Containers = append(spec.Containers, corev1.Container{Name: SlackSidecarContainerName, Image: cfg.Image, Env: env, VolumeMounts: []corev1.VolumeMount{{Name: "persist", MountPath: "/persist"}}, Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m"), corev1.ResourceMemory: resource.MustParse("32Mi")}, Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("64Mi")}}, SecurityContext: &corev1.SecurityContext{RunAsUser: ptrTo(int64(0)), ReadOnlyRootFilesystem: ptrTo(true), AllowPrivilegeEscalation: ptrTo(false)}, LivenessProbe: probe, ReadinessProbe: probe.DeepCopy()})
}

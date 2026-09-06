package agent

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/intstr"
	"github.com/matty-v/kyber/pkg/runtimes"
)

const SlackSidecarContainerName = "kyber-mcp-slack"
const SlackInboundBindingName = "slack"

type SlackSidecarConfig struct { AgentName, Image, ExistingSecret, LogLevel string }

func AppendSlackSidecar(spec *corev1.PodSpec, cfg SlackSidecarConfig) {
	if cfg.Image == "" || cfg.ExistingSecret == "" { return }
	secret := func(name,key string, optional bool) corev1.EnvVar { return corev1.EnvVar{Name:name,ValueFrom:&corev1.EnvVarSource{SecretKeyRef:&corev1.SecretKeySelector{LocalObjectReference:corev1.LocalObjectReference{Name:cfg.ExistingSecret},Key:key,Optional:&optional}}} }
	probe:=&corev1.Probe{ProbeHandler:corev1.ProbeHandler{HTTPGet:&corev1.HTTPGetAction{Path:"/healthz",Port:intstr.FromInt32(14009)}},InitialDelaySeconds:10,PeriodSeconds:30,FailureThreshold:3}
	spec.Containers=append(spec.Containers,corev1.Container{Name:SlackSidecarContainerName,Image:cfg.Image,Env:append([]corev1.EnvVar{{Name:"KYBER_AGENT_NAME",Value:cfg.AgentName},{Name:"KYBER_LOG_LEVEL",Value:cfg.LogLevel},{Name:"KYBER_INBOUND_BINDING",Value:SlackInboundBindingName},{Name:"KYBER_INBOUND_URL",Value:controlPlanePublicURL()},{Name:"KYBER_SLACK_MCP_ADDR",Value:runtimes.SlackMCPAddr()},{Name:"KYBER_SLACK_DOWNLOAD_DIR",Value:runtimes.SlackAttachmentDir},secret("SLACK_BOT_TOKEN","bot-token",false),secret("SLACK_APP_TOKEN","app-token",false),secret("KYBER_INBOUND_HMAC_SECRET","webhook-secret",true),secret("SLACK_ALLOWED_USER_IDS","allowed-user-ids",true),secret("SLACK_ALLOWED_CHANNEL_IDS","allowed-channel-ids",true)},loggingContextEnv(SlackSidecarContainerName)...),VolumeMounts:[]corev1.VolumeMount{{Name:"persist",MountPath:"/persist"}},Resources:corev1.ResourceRequirements{Requests:corev1.ResourceList{corev1.ResourceCPU:resource.MustParse("10m"),corev1.ResourceMemory:resource.MustParse("32Mi")},Limits:corev1.ResourceList{corev1.ResourceCPU:resource.MustParse("100m"),corev1.ResourceMemory:resource.MustParse("64Mi")}},SecurityContext:&corev1.SecurityContext{RunAsUser:ptrTo(int64(0)),ReadOnlyRootFilesystem:ptrTo(true),AllowPrivilegeEscalation:ptrTo(false)},LivenessProbe:probe,ReadinessProbe:probe.DeepCopy()})
}

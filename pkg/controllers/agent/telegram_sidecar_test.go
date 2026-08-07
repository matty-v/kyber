package agent

import (
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	pkgruntimes "github.com/matty-v/kyber/pkg/runtimes"
)

func TestTelegramInboundBindingIncludesAttachmentMetadata(t *testing.T) {
	binding := TelegramInboundBinding("dave-telegram", DefaultTelegramAction())
	want := []kyberv1.AgentInboundField{
		{Label: "attachment_type", JsonPath: "$.attachment_type"},
		{Label: "attachment_file_id", JsonPath: "$.attachment_file_id"},
		{Label: "attachment_name", JsonPath: "$.attachment_name"},
	}
	for _, field := range want {
		if !telegramBindingHasField(binding.Fields, field) {
			t.Errorf("Telegram binding missing field %+v", field)
		}
	}
	if !strings.Contains(binding.Action, "download_attachment") {
		t.Errorf("Telegram action does not tell the agent how to download an attachment: %q", binding.Action)
	}
}

// The sidecar emits message_id on every inbound path — message, callback and
// reaction — but for a while the binding did not forward it, so the agent never
// saw it. That silently disabled both `react` and `edit_message`: each requires
// a message_id the model had no way to obtain, including the keyboard cleanup
// the action text itself instructs. Forwarding the field is what makes those
// tools usable at all.
func TestTelegramInboundBindingForwardsMessageID(t *testing.T) {
	binding := TelegramInboundBinding("dave-telegram", DefaultTelegramAction())
	field := kyberv1.AgentInboundField{Label: "message_id", JsonPath: "$.message_id"}
	if !telegramBindingHasField(binding.Fields, field) {
		t.Errorf("Telegram binding missing %+v — react and edit_message cannot address a message without it", field)
	}
}

func TestAppendTelegramSidecar(t *testing.T) {
	spec := &corev1.PodSpec{Containers: []corev1.Container{{Name: AgentContainerName}}}
	AppendTelegramSidecar(spec, TelegramSidecarConfig{
		AgentName: "dave", Image: "kyber/mcp-telegram:local", ExistingSecret: "dave-telegram",
	})
	if len(spec.Containers) != 2 || spec.Containers[1].Name != TelegramSidecarContainerName {
		t.Fatalf("containers=%v, want Telegram sidecar appended", spec.Containers)
	}
	want := map[string]string{
		"TELEGRAM_BOT_TOKEN": "token", "TELEGRAM_ALLOWED_USER_IDS": "allowed-user-ids",
		"KYBER_INBOUND_HMAC_SECRET": "webhook-secret",
	}
	for _, env := range spec.Containers[1].Env {
		key, ok := want[env.Name]
		if !ok {
			continue
		}
		if env.ValueFrom == nil || env.ValueFrom.SecretKeyRef == nil ||
			env.ValueFrom.SecretKeyRef.Name != "dave-telegram" || env.ValueFrom.SecretKeyRef.Key != key {
			t.Errorf("env %s has wrong SecretKeyRef: %#v", env.Name, env.ValueFrom)
		}
		delete(want, env.Name)
	}
	if len(want) != 0 {
		t.Errorf("missing secret env vars: %v", want)
	}
}

// kyber#684: the sidecar must advertise its MCP address, or the agent's start
// script has nothing to register and the runtime silently loses every Telegram
// tool while still appearing healthy.
func TestAppendTelegramSidecar_AdvertisesMCPAddress(t *testing.T) {
	spec := &corev1.PodSpec{Containers: []corev1.Container{{Name: AgentContainerName}}}
	AppendTelegramSidecar(spec, TelegramSidecarConfig{
		AgentName: "dave", Image: "kyber/mcp-telegram:local", ExistingSecret: "dave-telegram",
	})
	var got string
	for _, env := range spec.Containers[1].Env {
		if env.Name == "KYBER_TELEGRAM_MCP_ADDR" {
			got = env.Value
		}
	}
	want := fmt.Sprintf("127.0.0.1:%d", pkgruntimes.TelegramMCPPort)
	if got != want {
		t.Errorf("KYBER_TELEGRAM_MCP_ADDR = %q, want %q", got, want)
	}
	// Loopback is the auth boundary: the agent container shares this pod's
	// network namespace, and binding anything wider would expose the tools —
	// which can message the operator — to the cluster network.
	if !strings.HasPrefix(got, "127.0.0.1:") {
		t.Errorf("MCP listener %q is not loopback-only — the Telegram tools would be reachable off-pod", got)
	}
}

func TestAppendTelegramSidecarGating(t *testing.T) {
	for _, cfg := range []TelegramSidecarConfig{
		{Image: "image"}, {ExistingSecret: "secret"}, {},
	} {
		spec := &corev1.PodSpec{Containers: []corev1.Container{{Name: AgentContainerName}}}
		AppendTelegramSidecar(spec, cfg)
		if len(spec.Containers) != 1 {
			t.Errorf("cfg=%+v appended sidecar without image and secret", cfg)
		}
	}
}

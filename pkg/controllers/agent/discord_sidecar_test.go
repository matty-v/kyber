package agent

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/matty-v/kyber/pkg/runtimes"
)

func TestAppendDiscordSidecar_Gating(t *testing.T) {
	cases := []struct {
		name       string
		cfg        DiscordSidecarConfig
		wantAppend bool
	}{
		{"no image → no-op", DiscordSidecarConfig{AgentName: "barf", ExistingSecret: "barf-discord"}, false},
		{"no secret → no-op", DiscordSidecarConfig{AgentName: "barf", Image: "ghcr.io/x/kyber-mcp-discord:latest"}, false},
		{"image + secret → inject", DiscordSidecarConfig{AgentName: "barf", Image: "ghcr.io/x/kyber-mcp-discord:latest", ExistingSecret: "barf-discord"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := &corev1.PodSpec{Containers: []corev1.Container{{Name: "runtime"}}}
			AppendDiscordSidecar(spec, tc.cfg)
			has := false
			for _, c := range spec.Containers {
				if c.Name == DiscordSidecarContainerName {
					has = true
				}
			}
			if has != tc.wantAppend {
				t.Errorf("append=%v, want %v", has, tc.wantAppend)
			}
		})
	}
}

func TestDiscordMCPWiringAndPortAllocation(t *testing.T) {
	if runtimes.DiscordMCPPort == runtimes.TelegramMCPPort ||
		runtimes.DiscordMCPPort == int(discordSidecarHealthPort) ||
		runtimes.DiscordMCPPort == 14003 || runtimes.DiscordMCPPort == 14004 || runtimes.DiscordMCPPort == 14005 {
		t.Fatalf("DiscordMCPPort %d collides with another pod-local channel endpoint", runtimes.DiscordMCPPort)
	}
	spec := &corev1.PodSpec{Containers: []corev1.Container{{Name: "runtime"}}}
	AppendDiscordSidecar(spec, DiscordSidecarConfig{AgentName: "barf", Image: "discord:local", ExistingSecret: "barf-discord"})
	for _, env := range spec.Containers[1].Env {
		if env.Name == "KYBER_DISCORD_MCP_ADDR" {
			if env.Value != runtimes.DiscordMCPAddr() {
				t.Fatalf("KYBER_DISCORD_MCP_ADDR = %q, want %q", env.Value, runtimes.DiscordMCPAddr())
			}
			return
		}
	}
	t.Fatal("Discord sidecar has no KYBER_DISCORD_MCP_ADDR")
}

func TestDiscordSidecarSharesPersistForAttachments(t *testing.T) {
	spec := &corev1.PodSpec{Containers: []corev1.Container{{Name: "runtime"}}}
	AppendDiscordSidecar(spec, DiscordSidecarConfig{AgentName: "barf", Image: "discord:local", ExistingSecret: "barf-discord"})
	sidecar := spec.Containers[1]
	if len(sidecar.VolumeMounts) != 1 || sidecar.VolumeMounts[0].Name != "persist" || sidecar.VolumeMounts[0].MountPath != "/persist" {
		t.Fatalf("volume mounts = %+v", sidecar.VolumeMounts)
	}
	if sidecar.SecurityContext == nil || sidecar.SecurityContext.RunAsUser == nil || *sidecar.SecurityContext.RunAsUser != 0 {
		t.Fatalf("security context = %+v; root is required to write the root-owned persist PVC", sidecar.SecurityContext)
	}
	found := false
	for _, env := range sidecar.Env {
		if env.Name == "KYBER_DISCORD_DOWNLOAD_DIR" && env.Value == runtimes.DiscordAttachmentDir {
			found = true
		}
	}
	if !found {
		t.Fatal("sidecar download directory does not match runtime attachment path")
	}
}

func TestAppendDiscordSidecar_SecretEnvWiring(t *testing.T) {
	spec := &corev1.PodSpec{Containers: []corev1.Container{{Name: "runtime"}}}
	AppendDiscordSidecar(spec, DiscordSidecarConfig{
		AgentName:      "barf",
		Image:          "ghcr.io/x/kyber-mcp-discord:latest",
		ExistingSecret: "barf-discord",
	})
	var sidecar *corev1.Container
	for i := range spec.Containers {
		if spec.Containers[i].Name == DiscordSidecarContainerName {
			sidecar = &spec.Containers[i]
		}
	}
	if sidecar == nil {
		t.Fatal("discord sidecar not injected")
	}
	if sidecar.LivenessProbe == nil || sidecar.ReadinessProbe == nil {
		t.Fatal("discord sidecar must expose both liveness and readiness probes")
	}

	// The bot token + HMAC secret must come from the per-agent Secret (never a
	// literal value that could leak into the pod spec / etcd in plaintext).
	wantSecretRefs := map[string]struct {
		key      string
		optional bool
	}{
		"DISCORD_BOT_TOKEN":           {"bot-token", false},
		"KYBER_INBOUND_HMAC_SECRET":   {"webhook-secret", false},
		"DISCORD_ALLOWED_USER_IDS":    {"allowed-user-ids", true},
		"DISCORD_ALLOWED_CHANNEL_IDS": {"channel-id", true},
		"DISCORD_ALLOWED_GUILD_IDS":   {"guild-id", true},
	}
	seen := map[string]bool{}
	for _, e := range sidecar.Env {
		want, ok := wantSecretRefs[e.Name]
		if !ok {
			continue
		}
		seen[e.Name] = true
		if e.Value != "" {
			t.Errorf("%s should use a SecretKeyRef, not a literal value %q", e.Name, e.Value)
		}
		if e.ValueFrom == nil || e.ValueFrom.SecretKeyRef == nil {
			t.Errorf("%s: expected a SecretKeyRef source", e.Name)
			continue
		}
		ref := e.ValueFrom.SecretKeyRef
		if ref.Name != "barf-discord" || ref.Key != want.key {
			t.Errorf("%s: bad secret ref %+v (want secret=barf-discord key=%s)", e.Name, ref, want.key)
		}
		if ref.Optional == nil || *ref.Optional != want.optional {
			t.Errorf("%s: optional=%v, want %v", e.Name, ref.Optional, want.optional)
		}
	}
	for name := range wantSecretRefs {
		if !seen[name] {
			t.Errorf("missing env %s", name)
		}
	}
	// Routing envs the sidecar needs.
	routing := map[string]bool{"KYBER_AGENT_NAME": false, "KYBER_INBOUND_BINDING": false, "KYBER_INBOUND_URL": false}
	for _, e := range sidecar.Env {
		if _, ok := routing[e.Name]; ok {
			routing[e.Name] = true
		}
	}
	for name, ok := range routing {
		if !ok {
			t.Errorf("missing routing env %s", name)
		}
	}
	for _, e := range sidecar.Env {
		if e.Name == "KYBER_DISCORD_SEND_ADDR" {
			t.Error("send address should use the sidecar's loopback-only default")
		}
	}
}

// mentionOnly=false must leave the env var OFF entirely, not render "false":
// an older sidecar image that doesn't know the knob then behaves as before.
func TestAppendDiscordSidecar_MentionOnly(t *testing.T) {
	for _, tc := range []struct {
		name        string
		mentionOnly bool
		wantValue   string // "" = env must be absent
	}{
		{"default → env absent", false, ""},
		{"enabled → DISCORD_MENTION_ONLY=true", true, "true"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := &corev1.PodSpec{Containers: []corev1.Container{{Name: "runtime"}}}
			AppendDiscordSidecar(spec, DiscordSidecarConfig{
				AgentName:      "barf",
				Image:          "ghcr.io/x/kyber-mcp-discord:latest",
				ExistingSecret: "barf-discord",
				MentionOnly:    tc.mentionOnly,
			})
			got := ""
			for _, c := range spec.Containers {
				if c.Name != DiscordSidecarContainerName {
					continue
				}
				for _, e := range c.Env {
					if e.Name == "DISCORD_MENTION_ONLY" {
						got = e.Value
					}
				}
			}
			if got != tc.wantValue {
				t.Errorf("DISCORD_MENTION_ONLY = %q, want %q", got, tc.wantValue)
			}
		})
	}
}

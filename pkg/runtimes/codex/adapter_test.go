package codex

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/runtimes"
)

func TestAdapter(t *testing.T) {
	t.Setenv(RuntimeImageEnv, "kyber/codex:test")
	a := NewAdapter()
	if got := a.Image(); got != "kyber/codex:test" {
		t.Errorf("Image() = %q", got)
	}
	agent := &kyberv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "echo"}, Spec: kyberv1.AgentSpec{Model: "gpt-5.2-codex"}}
	env := a.EnvVars(agent)
	if len(env) != 3 || env[0].Value != "gpt-5.2-codex" {
		t.Fatalf("EnvVars() = %+v", env)
	}
	if got := env[2].ValueFrom.SecretKeyRef.Name; got != "echo-codex-auth" {
		t.Errorf("auth secret = %q", got)
	}
	if len(a.RestartSessionCommand()) == 0 {
		t.Error("RestartSessionCommand() is empty")
	}
	probeCommand := strings.Join(a.ReadinessProbe().Exec.Command, " ")
	if !strings.Contains(probeCommand, "nsenter --target 1 --mount --root --wd") ||
		!strings.Contains(probeCommand, "/home/kyber/.codex/auth.json") ||
		!strings.Contains(probeCommand, `marker" != "{}"`) ||
		!strings.Contains(probeCommand, "codex login status") {
		t.Errorf("ReadinessProbe() does not check Codex inside the agent chroot: %q", probeCommand)
	}
}

func TestAdapterDiscordMCP(t *testing.T) {
	a := NewAdapter()
	agent := &kyberv1.Agent{Spec: kyberv1.AgentSpec{
		Channels: &kyberv1.AgentChannels{Discord: &kyberv1.AgentDiscordChannel{}},
	}}
	for _, v := range a.EnvVars(agent) {
		if v.Name == "KYBER_DISCORD_MCP_URL" {
			if v.Value != runtimes.DiscordMCPURL() {
				t.Fatalf("KYBER_DISCORD_MCP_URL = %q, want %q", v.Value, runtimes.DiscordMCPURL())
			}
			return
		}
	}
	t.Fatal("KYBER_DISCORD_MCP_URL not found")
}

func TestAdapterAPIKey(t *testing.T) {
	a := NewAdapter()
	agent := &kyberv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "echo"}, Spec: kyberv1.AgentSpec{
		Secrets: kyberv1.AgentSecrets{AuthType: kyberv1.AgentAuthTypeAPIKey},
	}}
	if got := a.CredentialSecretName(agent); got != "echo-openai" {
		t.Fatalf("CredentialSecretName() = %q, want echo-openai", got)
	}
	env := a.EnvVars(agent)
	if len(env) != 4 {
		t.Fatalf("EnvVars() len = %d, want 4: %+v", len(env), env)
	}
	if got := env[3].ValueFrom.SecretKeyRef.Name; got != "echo-openai" {
		t.Errorf("OPENAI_API_KEY secret = %q, want echo-openai", got)
	}
}

package agent

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	pkgruntimes "github.com/matty-v/kyber/pkg/runtimes"
	"github.com/matty-v/kyber/pkg/runtimes/claudecode"
	"github.com/matty-v/kyber/pkg/runtimes/codex"
)

// kyber#684: one Telegram bridge for both runtimes.
//
// Telegram used to have two implementations split by runtime — the native
// Claude Code plugin, and this sidecar for Codex. That meant two things to
// maintain for one channel, and the in-process half carried the reboot-409 race
// (#678/#679). These tests pin the convergence and, above all, the invariant
// that makes it safe: the plugin and the sidecar can never both hold the bot.

func telegramAgent(runtime string) *kyberv1.Agent {
	a := &kyberv1.Agent{}
	a.Name = "dave"
	a.Namespace = "kyber-system"
	a.Spec.Runtime = runtime
	a.Spec.Secrets.TelegramEnabled = true
	a.Spec.Secrets.AuthType = kyberv1.AgentAuthTypeOAuth
	return a
}

func adapterFor(t *testing.T, runtime string) pkgruntimes.Adapter {
	t.Helper()
	switch runtime {
	case "claude-code":
		return &claudecode.ClaudeCodeAdapter{}
	case "codex":
		return codex.NewAdapter()
	}
	t.Fatalf("no adapter for runtime %q", runtime)
	return nil
}

// THE invariant. Two pollers on one bot token is a permanent 409 storm, and it
// is not hypothetical — #678/#679 is exactly that, where whichever poller won a
// boot race decided whether the agent could hear Telegram at all.
//
// The guarantee here is structural rather than a flag: the runtime container is
// never given TELEGRAM_BOT_TOKEN, so the plugin CANNOT poll even if something
// re-enables it. Only the sidecar holds the credential.
func TestTelegramConvergence_RuntimeNeverGetsTheBotToken(t *testing.T) {
	for _, runtime := range []string{"claude-code", "codex"} {
		t.Run(runtime, func(t *testing.T) {
			for _, env := range adapterFor(t, runtime).EnvVars(telegramAgent(runtime)) {
				if env.Name == "TELEGRAM_BOT_TOKEN" {
					t.Fatalf("%s runtime container receives TELEGRAM_BOT_TOKEN — the plugin could poll the "+
						"same bot as the sidecar and 409 against it (kyber#684, #678/#679)", runtime)
				}
			}
		})
	}
}

// Both runtimes must be pointed at the sidecar's tool surface. Without this an
// agent can receive Telegram messages and have no way to answer them.
func TestTelegramConvergence_BothRuntimesGetTheMCPURL(t *testing.T) {
	for _, runtime := range []string{"claude-code", "codex"} {
		t.Run(runtime, func(t *testing.T) {
			var got string
			for _, env := range adapterFor(t, runtime).EnvVars(telegramAgent(runtime)) {
				if env.Name == "KYBER_TELEGRAM_MCP_URL" {
					got = env.Value
				}
			}
			if got == "" {
				t.Fatalf("%s has no KYBER_TELEGRAM_MCP_URL — it could receive messages but never reply", runtime)
			}
			if !strings.HasPrefix(got, "http://127.0.0.1:") {
				t.Errorf("%s MCP URL %q is not loopback — the Telegram tools must not be reachable off-pod", runtime, got)
			}
		})
	}
}

// The sidecar is no longer Codex-only. A Claude Code agent with Telegram
// enabled must get it too, or the convergence is only half done and Claude Code
// agents lose the channel entirely (they no longer have the plugin).
func TestTelegramConvergence_SidecarInjectedForEveryRuntime(t *testing.T) {
	for _, runtime := range []string{"claude-code", "codex", "some-future-runtime"} {
		t.Run(runtime, func(t *testing.T) {
			spec := &corev1.PodSpec{Containers: []corev1.Container{{Name: AgentContainerName}}}
			AppendTelegramSidecar(spec, TelegramSidecarConfig{
				AgentName: "dave", Image: "kyber/mcp-telegram:local", ExistingSecret: "dave-telegram",
			})
			var found bool
			for _, c := range spec.Containers {
				if c.Name == TelegramSidecarContainerName {
					found = true
				}
			}
			if !found {
				t.Errorf("no Telegram sidecar for runtime %q", runtime)
			}
		})
	}
}

// Only the sidecar holds the credential — assert the positive half too, so a
// refactor that drops the secret wiring fails loudly instead of producing an
// agent that silently cannot reach Telegram at all.
func TestTelegramConvergence_SidecarHoldsTheCredential(t *testing.T) {
	spec := &corev1.PodSpec{Containers: []corev1.Container{{Name: AgentContainerName}}}
	AppendTelegramSidecar(spec, TelegramSidecarConfig{
		AgentName: "dave", Image: "kyber/mcp-telegram:local", ExistingSecret: "dave-telegram",
	})
	var sidecar *corev1.Container
	for i := range spec.Containers {
		if spec.Containers[i].Name == TelegramSidecarContainerName {
			sidecar = &spec.Containers[i]
		}
	}
	if sidecar == nil {
		t.Fatal("sidecar not appended")
	}
	for _, env := range sidecar.Env {
		if env.Name == "TELEGRAM_BOT_TOKEN" {
			if env.ValueFrom == nil || env.ValueFrom.SecretKeyRef == nil {
				t.Fatal("sidecar's TELEGRAM_BOT_TOKEN must come from a SecretKeyRef, never a literal")
			}
			return
		}
	}
	t.Error("the sidecar has no TELEGRAM_BOT_TOKEN — nothing can poll Telegram for this agent")
}

// appendTelegramSidecarTo builds a pod spec shaped like the real one — an agent
// container that mounts the persist PVC — and returns the appended sidecar.
func appendTelegramSidecarTo(t *testing.T) (*corev1.PodSpec, *corev1.Container) {
	t.Helper()
	spec := &corev1.PodSpec{Containers: []corev1.Container{{
		Name:         AgentContainerName,
		VolumeMounts: []corev1.VolumeMount{{Name: "persist", MountPath: "/persist"}},
	}}}
	AppendTelegramSidecar(spec, TelegramSidecarConfig{
		AgentName: "dave", Image: "kyber/mcp-telegram:local", ExistingSecret: "dave-telegram",
	})
	for i := range spec.Containers {
		if spec.Containers[i].Name == TelegramSidecarContainerName {
			return spec, &spec.Containers[i]
		}
	}
	t.Fatal("sidecar not appended")
	return nil, nil
}

// The download_attachment tool hands the MODEL a filesystem path and the model
// then reads it — from a different container. So the sidecar's download
// directory has to resolve to the same bytes in both containers, which means
// the same volume mounted at the same path.
//
// The first cut of kyber#684 gave the sidecar no volume mounts at all. Every
// download "succeeded" into the sidecar's own ephemeral filesystem and the agent
// found nothing, which is a silent failure of the headline parity item — an
// inbound photo reaching the agent. The unit test for downloads passed anyway,
// because it downloaded into a t.TempDir() and never crossed the boundary this
// feature actually has to cross.
func TestTelegramSidecar_AttachmentDirIsReadableByTheAgent(t *testing.T) {
	spec, sidecar := appendTelegramSidecarTo(t)

	var downloadDir string
	for _, env := range sidecar.Env {
		if env.Name == "KYBER_TELEGRAM_DOWNLOAD_DIR" {
			downloadDir = env.Value
		}
	}
	if downloadDir == "" {
		t.Fatal("sidecar has no KYBER_TELEGRAM_DOWNLOAD_DIR — downloads land wherever the binary defaults, " +
			"which the agent has no reason to be able to read")
	}

	// Resolve the directory to a (volume, path-within-volume) pair in each
	// container. If they disagree, the agent cannot read what was downloaded.
	resolve := func(c *corev1.Container) (volume, rel string, ok bool) {
		for _, vm := range c.VolumeMounts {
			if downloadDir == vm.MountPath || strings.HasPrefix(downloadDir, strings.TrimSuffix(vm.MountPath, "/")+"/") {
				return vm.Name, strings.TrimPrefix(downloadDir, vm.MountPath), true
			}
		}
		return "", "", false
	}

	sidecarVol, sidecarRel, ok := resolve(sidecar)
	if !ok {
		t.Fatalf("the sidecar mounts no volume covering %s — it would write attachments into its own "+
			"container filesystem and the agent would never see them (kyber#684)", downloadDir)
	}
	agentVol, agentRel, ok := resolve(&spec.Containers[0])
	if !ok {
		t.Fatalf("the agent container mounts no volume covering %s — it cannot read what the sidecar downloads", downloadDir)
	}
	if sidecarVol != agentVol || sidecarRel != agentRel {
		t.Errorf("attachment dir %s resolves to %s:%s in the sidecar but %s:%s in the agent — "+
			"download_attachment would return a path the agent cannot read",
			downloadDir, sidecarVol, sidecarRel, agentVol, agentRel)
	}
}

// The pod sets no fsGroup, so the persist PVC root stays root-owned. A sidecar
// running as its image's default "nonroot" uid could not create the attachment
// directory at all — and anything it did write 0600 would be unreadable by the
// agent's uid. Same permission mismatch that killed every new agent in
// kyber#684's workstream 1, one container over.
func TestTelegramSidecar_RunsAsRootSoAttachmentsAreWritable(t *testing.T) {
	_, sidecar := appendTelegramSidecarTo(t)
	if sidecar.SecurityContext == nil || sidecar.SecurityContext.RunAsUser == nil {
		t.Fatal("sidecar has no RunAsUser — it defaults to the image's nonroot uid and cannot write to the persist PVC")
	}
	if *sidecar.SecurityContext.RunAsUser != 0 {
		t.Errorf("sidecar RunAsUser = %d, want 0", *sidecar.SecurityContext.RunAsUser)
	}
}

// Every sidecar in an agent pod binds into ONE loopback namespace, so a port is
// a pod-wide allocation. The Telegram MCP server first took 14005, which is
// kyber-mcp-discord's /send — an agent with both channels enabled would have had
// two containers racing for the same port and one crash-looping.
func TestTelegramMCPPort_DoesNotCollideWithOtherSidecars(t *testing.T) {
	taken := map[int]string{
		int(discordSidecarHealthPort): "kyber-mcp-discord /healthz",
		telegramSidecarHealthPort:     "kyber-mcp-telegram /healthz",
		14004:                         "kyber-mcp-telegram /send",
		14005:                         "kyber-mcp-discord /send",
		pkgruntimes.DiscordMCPPort:    "kyber-mcp-discord /mcp",
	}
	if owner, clash := taken[pkgruntimes.TelegramMCPPort]; clash {
		t.Errorf("TelegramMCPPort %d is already bound by %s in the same pod netns — "+
			"one of the two containers will fail to listen and crash-loop",
			pkgruntimes.TelegramMCPPort, owner)
	}
}

// The sidecar binds one end of this connection and the runtime adapters name the
// other. They used to be a constant in the controller and a string literal in
// each adapter, so changing the port would have left the adapters pointing at a
// closed socket with nothing failing.
func TestTelegramMCPWiring_BothEndsAgree(t *testing.T) {
	_, sidecar := appendTelegramSidecarTo(t)
	var bound string
	for _, env := range sidecar.Env {
		if env.Name == "KYBER_TELEGRAM_MCP_ADDR" {
			bound = env.Value
		}
	}
	if bound == "" {
		t.Fatal("sidecar has no KYBER_TELEGRAM_MCP_ADDR")
	}
	for _, runtime := range []string{"claude-code", "codex"} {
		for _, env := range adapterFor(t, runtime).EnvVars(telegramAgent(runtime)) {
			if env.Name != "KYBER_TELEGRAM_MCP_URL" {
				continue
			}
			if !strings.Contains(env.Value, bound) {
				t.Errorf("%s dials %q but the sidecar listens on %q", runtime, env.Value, bound)
			}
		}
	}
}

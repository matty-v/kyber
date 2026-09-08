package hermes

import (
	"context"
	"errors"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	api "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/runtimes"
	"github.com/matty-v/kyber/pkg/runtimes/contracttest"
)

func TestAdapterContract(t *testing.T) {
	t.Setenv(RuntimeImageEnv, "kyber/hermes:test")
	agent := &api.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "echo"},
		Spec: api.AgentSpec{
			Runtime: Type,
			Model:   "anthropic/claude-sonnet-4.6",
			Secrets: api.AgentSecrets{
				AuthType: api.AgentAuthTypeAPIKey,
			},
		},
	}
	failures := contracttest.CheckAdapter(NewAdapter(), agent, contracttest.AuthCase{
		Mode:             api.AgentAuthTypeAPIKey,
		CredentialSuffix: "-openrouter",
		RequiredEnvKeys:  map[string]string{"OPENROUTER_API_KEY": "token"},
	})
	if len(failures) > 0 {
		t.Fatalf("adapter contract failures: %v", failures)
	}
}

func TestDescriptorDeclaresOnlyPreviewFeatures(t *testing.T) {
	d, ok := runtimes.Describe(Type)
	if !ok {
		t.Fatal("Hermes descriptor is not registered")
	}
	for _, feature := range []runtimes.Feature{runtimes.SessionRestart, runtimes.SessionResume, runtimes.Compaction, runtimes.ModelCatalog, runtimes.UsageReporting} {
		if !d.Supports(feature) {
			t.Errorf("descriptor does not support %q", feature)
		}
	}
	for _, feature := range []runtimes.Feature{runtimes.JobTurnHooks, runtimes.TaskReceipts, runtimes.TaskTools, runtimes.RuntimeRepairFeature} {
		if d.Supports(feature) {
			t.Errorf("descriptor advertises unimplemented feature %q", feature)
		}
	}
	if d.LegacyVersionsKey != "hermesVersions" || !d.RequireCatalogContext {
		t.Fatalf("version/catalog contract = %+v", d)
	}
	mode, ok := d.Auth(api.AgentAuthTypeAPIKey)
	if !ok || mode.InputField != openRouterInputField || mode.SecretSuffix != "openrouter" {
		t.Fatalf("OpenRouter auth mode = %+v, present=%v", mode, ok)
	}
}

func TestAuthentication(t *testing.T) {
	auth := authentication{}
	if err := auth.Validate(api.AgentAuthTypeOAuth, nil); err == nil {
		t.Fatal("OAuth unexpectedly accepted")
	}
	err := auth.Validate(api.AgentAuthTypeAPIKey, nil)
	var validation *runtimes.AuthValidationError
	if !errors.As(err, &validation) || validation.Field != "secrets.runtimeAuth.openrouterApiKey" {
		t.Fatalf("missing-key error = %#v", err)
	}
	credentials, err := auth.Prepare(context.Background(), api.AgentAuthTypeAPIKey, runtimes.AuthInput{openRouterInputField: "secret-value"}, runtimes.AuthOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(credentials) != 1 || credentials[0].Suffix != "openrouter" || string(credentials[0].Data["token"]) != "secret-value" {
		t.Fatalf("credentials = %+v", credentials)
	}
}

func TestSessionCommands(t *testing.T) {
	a := NewAdapter()
	if got := a.RestartSessionCommand(); len(got) == 0 || got[len(got)-1] != "--fresh" {
		t.Fatalf("RestartSessionCommand() = %v", got)
	}
	if got := strings.Join(a.CompactSessionCommand(), " "); !strings.Contains(got, "/compress") {
		t.Fatalf("CompactSessionCommand() = %q", got)
	}
	if got := strings.Join(a.ReadinessProbe().Exec.Command, " "); !strings.Contains(got, "OPENROUTER_API_KEY") || !strings.Contains(got, "[h]ermes.*chat") {
		t.Fatalf("ReadinessProbe() = %q", got)
	}
	if got := strings.Join(a.LivenessProbe().Exec.Command, " "); !strings.Contains(got, "[h]ermes.*chat") {
		t.Fatalf("LivenessProbe() can match its own probe process: %q", got)
	}
	if got := a.LivenessProbe().InitialDelaySeconds; got < 600 {
		t.Fatalf("LivenessProbe().InitialDelaySeconds = %d, want enough time for durable rootfs migration", got)
	}
}

func TestChannelMCPEnvironment(t *testing.T) {
	a := NewAdapter()
	agent := &api.Agent{Spec: api.AgentSpec{
		Secrets: api.AgentSecrets{
			TelegramEnabled: true,
			SlackEnabled:    true,
		},
		Channels: &api.AgentChannels{Discord: &api.AgentDiscordChannel{}},
	}}
	env := map[string]string{}
	for _, item := range a.EnvVars(agent) {
		env[item.Name] = item.Value
	}
	for key, want := range map[string]string{
		"KYBER_TELEGRAM_MCP_URL": runtimes.TelegramMCPURL(),
		"KYBER_DISCORD_MCP_URL":  runtimes.DiscordMCPURL(),
		"KYBER_SLACK_MCP_URL":    runtimes.SlackMCPURL(),
	} {
		if env[key] != want {
			t.Errorf("%s = %q, want %q", key, env[key], want)
		}
	}
	for _, forbidden := range []string{"TELEGRAM_BOT_TOKEN", "DISCORD_BOT_TOKEN", "SLACK_BOT_TOKEN"} {
		if _, ok := env[forbidden]; ok {
			t.Errorf("channel credential %s leaked to Hermes", forbidden)
		}
	}
}

package claudecode

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/runtimes"
)

// agentRuntimeImageEnv mirrors the const in adapter.go; kept local to the
// test file so a rename of the env var trips both at once.
const agentRuntimeImageEnv = "KYBER_AGENT_RUNTIME_IMAGE"

// Compile-time assertion: ClaudeCodeAdapter must satisfy runtimes.Adapter.
var _ runtimes.Adapter = (*ClaudeCodeAdapter)(nil)

// claudeCodeAgent builds a minimal Agent for ClaudeCodeAdapter tests.
func claudeCodeAgent(authType kyberv1.AgentAuthType) *kyberv1.Agent {
	return &kyberv1.Agent{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "dave",
			Namespace: "kyber-system",
		},
		Spec: kyberv1.AgentSpec{
			Machine: "node-01",
			Runtime: "claude-code",
			Model:   "claude-sonnet-4",
			Resources: kyberv1.AgentResources{
				CPU:    resource.MustParse("1"),
				Memory: resource.MustParse("2Gi"),
				Disk:   resource.MustParse("50Gi"),
			},
			Secrets: kyberv1.AgentSecrets{
				AuthType: authType,
			},
		},
	}
}

func TestClaudeCodeAdapter_Type(t *testing.T) {
	a := &ClaudeCodeAdapter{}
	if got := a.Type(); got != "claude-code" {
		t.Errorf("Type(): got %q, want %q", got, "claude-code")
	}
}

// TestNewClaudeCodeAdapter_ReadsEnvVar covers Cause D from kyber#360:
// the agent runtime image must be sourced from KYBER_AGENT_RUNTIME_IMAGE
// (Helm chart's image.claudeCode.{repository,tag} composed in
// deploy/helm/kyber/templates/control-plane/deployment.yaml). Previously
// adapter.Image() hardcoded ":latest", which silently ignored the chart's
// tag and let the cluster drift onto a pre-#281 image that bypassed the
// sidecar's BumpActivityCounter — leaving 3 metrics panels dark.
//
// Pattern mirror: KYBER_STATUS_SIDECAR_IMAGE in
// pkg/controllers/agent/status_sidecar.go.
func TestNewClaudeCodeAdapter_ReadsEnvVar(t *testing.T) {
	t.Setenv(agentRuntimeImageEnv, "ghcr.io/matty-v/kyber-claude-code:v1.3.2")
	a := NewClaudeCodeAdapter()
	got := a.Image()
	want := "ghcr.io/matty-v/kyber-claude-code:v1.3.2"
	if got != want {
		t.Errorf("Image(): got %q, want %q", got, want)
	}
}

// TestNewClaudeCodeAdapter_EmptyEnvFailsClosed asserts the deliberate
// fail-closed behavior — when KYBER_AGENT_RUNTIME_IMAGE is unset, Image()
// returns "" so pod creation visibly fails (k8s rejects an empty image
// reference) rather than silently falling back to a hardcoded :latest.
// Per Obi-wan's design pass on kyber#360: "operator visibility beats
// silent fallback." The pre-#360 hardcoded fallback is what allowed
// Cause C (sidecar forwarder bypass) to hide for two iterations.
func TestNewClaudeCodeAdapter_EmptyEnvFailsClosed(t *testing.T) {
	t.Setenv(agentRuntimeImageEnv, "")
	a := NewClaudeCodeAdapter()
	if got := a.Image(); got != "" {
		t.Errorf("Image() with unset env: got %q, want empty string (fail-closed)", got)
	}
}

// TestClaudeCodeAdapter_Image_ZeroValueIsEmpty covers direct struct
// construction (test fixtures that don't care about Image). Zero-value
// is empty, consistent with NewClaudeCodeAdapter on an unset env.
func TestClaudeCodeAdapter_Image_ZeroValueIsEmpty(t *testing.T) {
	a := &ClaudeCodeAdapter{}
	if got := a.Image(); got != "" {
		t.Errorf("Image() on zero-value adapter: got %q, want empty string", got)
	}
}

func TestClaudeCodeAdapter_EntrypointArgs(t *testing.T) {
	a := &ClaudeCodeAdapter{}
	agent := claudeCodeAgent(kyberv1.AgentAuthTypeOAuth)
	got := a.EntrypointArgs(agent)
	want := []string{"/usr/local/bin/start-claude.sh"}
	if len(got) != len(want) || got[0] != want[0] {
		t.Errorf("EntrypointArgs(): got %v, want %v", got, want)
	}
}

func TestClaudeCodeAdapter_EnvVars_OAuthAuth(t *testing.T) {
	a := &ClaudeCodeAdapter{}
	agent := claudeCodeAgent(kyberv1.AgentAuthTypeOAuth)

	vars := a.EnvVars(agent)
	envMap := make(map[string]corev1.EnvVar)
	for _, v := range vars {
		envMap[v.Name] = v
	}

	wantSecretName := "dave-oauth"

	// CLAUDE_CODE_OAUTH_TOKEN must be absent — the legacy setup-token path was removed.
	if _, found := envMap["CLAUDE_CODE_OAUTH_TOKEN"]; found {
		t.Error("CLAUDE_CODE_OAUTH_TOKEN must not be present (legacy setup-token path removed)")
	}

	// CLAUDE_ACCESS_TOKEN must be present with the correct SecretKeyRef (PKCE path).
	accessVar, ok := envMap["CLAUDE_ACCESS_TOKEN"]
	if !ok {
		t.Fatal("CLAUDE_ACCESS_TOKEN env var not found for oauth AuthType")
	}
	if accessVar.ValueFrom == nil || accessVar.ValueFrom.SecretKeyRef == nil {
		t.Fatal("CLAUDE_ACCESS_TOKEN must use valueFrom.secretKeyRef")
	}
	if accessVar.ValueFrom.SecretKeyRef.Name != wantSecretName {
		t.Errorf("CLAUDE_ACCESS_TOKEN secretKeyRef.name: got %q, want %q",
			accessVar.ValueFrom.SecretKeyRef.Name, wantSecretName)
	}
	if accessVar.ValueFrom.SecretKeyRef.Key != "access_token" {
		t.Errorf("CLAUDE_ACCESS_TOKEN secretKeyRef.key: got %q, want %q",
			accessVar.ValueFrom.SecretKeyRef.Key, "access_token")
	}

	// CLAUDE_REFRESH_TOKEN must be present with the correct SecretKeyRef (multi-key path).
	refreshVar, ok := envMap["CLAUDE_REFRESH_TOKEN"]
	if !ok {
		t.Fatal("CLAUDE_REFRESH_TOKEN env var not found for oauth AuthType")
	}
	if refreshVar.ValueFrom == nil || refreshVar.ValueFrom.SecretKeyRef == nil {
		t.Fatal("CLAUDE_REFRESH_TOKEN must use valueFrom.secretKeyRef")
	}
	if refreshVar.ValueFrom.SecretKeyRef.Name != wantSecretName {
		t.Errorf("CLAUDE_REFRESH_TOKEN secretKeyRef.name: got %q, want %q",
			refreshVar.ValueFrom.SecretKeyRef.Name, wantSecretName)
	}
	if refreshVar.ValueFrom.SecretKeyRef.Key != "refresh_token" {
		t.Errorf("CLAUDE_REFRESH_TOKEN secretKeyRef.key: got %q, want %q",
			refreshVar.ValueFrom.SecretKeyRef.Key, "refresh_token")
	}

	// CLAUDE_ACCESS_TOKEN_EXPIRES_AT must be present with the correct SecretKeyRef (multi-key path).
	expiresVar, ok := envMap["CLAUDE_ACCESS_TOKEN_EXPIRES_AT"]
	if !ok {
		t.Fatal("CLAUDE_ACCESS_TOKEN_EXPIRES_AT env var not found for oauth AuthType")
	}
	if expiresVar.ValueFrom == nil || expiresVar.ValueFrom.SecretKeyRef == nil {
		t.Fatal("CLAUDE_ACCESS_TOKEN_EXPIRES_AT must use valueFrom.secretKeyRef")
	}
	if expiresVar.ValueFrom.SecretKeyRef.Name != wantSecretName {
		t.Errorf("CLAUDE_ACCESS_TOKEN_EXPIRES_AT secretKeyRef.name: got %q, want %q",
			expiresVar.ValueFrom.SecretKeyRef.Name, wantSecretName)
	}
	if expiresVar.ValueFrom.SecretKeyRef.Key != "expires_at" {
		t.Errorf("CLAUDE_ACCESS_TOKEN_EXPIRES_AT secretKeyRef.key: got %q, want %q",
			expiresVar.ValueFrom.SecretKeyRef.Key, "expires_at")
	}

	// ANTHROPIC_API_KEY must be absent for oauth.
	if _, found := envMap["ANTHROPIC_API_KEY"]; found {
		t.Error("ANTHROPIC_API_KEY must not be present when AuthType is oauth")
	}
}

func TestClaudeCodeAdapter_EnvVars_OAuth_AllKeysAreOptional(t *testing.T) {
	a := &ClaudeCodeAdapter{}
	agent := claudeCodeAgent(kyberv1.AgentAuthTypeOAuth)

	vars := a.EnvVars(agent)
	envMap := make(map[string]corev1.EnvVar)
	for _, v := range vars {
		envMap[v.Name] = v
	}

	oauthVars := []struct {
		name string
		key  string
	}{
		{"CLAUDE_ACCESS_TOKEN", "access_token"},
		{"CLAUDE_REFRESH_TOKEN", "refresh_token"},
		{"CLAUDE_ACCESS_TOKEN_EXPIRES_AT", "expires_at"},
	}

	for _, tc := range oauthVars {
		v, ok := envMap[tc.name]
		if !ok {
			t.Errorf("%s env var not found", tc.name)
			continue
		}
		if v.ValueFrom == nil || v.ValueFrom.SecretKeyRef == nil {
			t.Errorf("%s must use valueFrom.secretKeyRef", tc.name)
			continue
		}
		ref := v.ValueFrom.SecretKeyRef
		if ref.Optional == nil {
			t.Errorf("%s secretKeyRef.optional: got nil, want *true", tc.name)
		} else if !*ref.Optional {
			t.Errorf("%s secretKeyRef.optional: got false, want true", tc.name)
		}
	}
}

func TestClaudeCodeAdapter_EnvVars_APIKeyAuth(t *testing.T) {
	a := &ClaudeCodeAdapter{}
	agent := claudeCodeAgent(kyberv1.AgentAuthTypeAPIKey)

	vars := a.EnvVars(agent)
	envMap := make(map[string]corev1.EnvVar)
	for _, v := range vars {
		envMap[v.Name] = v
	}

	// ANTHROPIC_API_KEY must be present with the correct SecretKeyRef.
	apiKeyVar, ok := envMap["ANTHROPIC_API_KEY"]
	if !ok {
		t.Fatal("ANTHROPIC_API_KEY env var not found for api-key AuthType")
	}
	if apiKeyVar.ValueFrom == nil || apiKeyVar.ValueFrom.SecretKeyRef == nil {
		t.Fatal("ANTHROPIC_API_KEY must use valueFrom.secretKeyRef")
	}
	wantSecretName := "dave-anthropic"
	if apiKeyVar.ValueFrom.SecretKeyRef.Name != wantSecretName {
		t.Errorf("ANTHROPIC_API_KEY secretKeyRef.name: got %q, want %q",
			apiKeyVar.ValueFrom.SecretKeyRef.Name, wantSecretName)
	}
	if apiKeyVar.ValueFrom.SecretKeyRef.Key != "token" {
		t.Errorf("ANTHROPIC_API_KEY secretKeyRef.key: got %q, want %q",
			apiKeyVar.ValueFrom.SecretKeyRef.Key, "token")
	}

	// OAuth env vars must be absent for api-key.
	for _, name := range []string{"CLAUDE_ACCESS_TOKEN", "CLAUDE_REFRESH_TOKEN", "CLAUDE_ACCESS_TOKEN_EXPIRES_AT"} {
		if _, found := envMap[name]; found {
			t.Errorf("%s must not be present when AuthType is api-key", name)
		}
	}
}

// kyber#684 inverted this test's contract, deliberately. The agent container no
// longer receives the bot token at all: the sidecar owns the Telegram channel
// for every runtime, and withholding the credential here is what makes it
// STRUCTURALLY impossible for the native plugin to poll the same bot and 409
// against the sidecar (the #678/#679 failure). A flag would have left that
// possible; no token means the plugin cannot poll even if something re-enables
// it. What the agent gets instead is the sidecar's MCP endpoint, so it keeps a
// real tool surface rather than dropping to curl.
func TestClaudeCodeAdapter_EnvVars_TelegramGivesMCPURLNotBotToken(t *testing.T) {
	a := &ClaudeCodeAdapter{}
	agent := claudeCodeAgent(kyberv1.AgentAuthTypeOAuth)
	agent.Spec.Secrets.TelegramEnabled = true

	envMap := make(map[string]corev1.EnvVar)
	for _, v := range a.EnvVars(agent) {
		envMap[v.Name] = v
	}

	if _, present := envMap["TELEGRAM_BOT_TOKEN"]; present {
		t.Error("TELEGRAM_BOT_TOKEN must NOT reach the agent container — the plugin could then poll " +
			"the same bot as the sidecar and 409 against it (kyber#684, #678/#679)")
	}
	mcp, ok := envMap["KYBER_TELEGRAM_MCP_URL"]
	if !ok {
		t.Fatal("KYBER_TELEGRAM_MCP_URL not found — the agent has no way to reach the Telegram tools")
	}
	if want := runtimes.TelegramMCPURL(); mcp.Value != want {
		t.Errorf("KYBER_TELEGRAM_MCP_URL = %q, want %q", mcp.Value, want)
	}
	if mcp.ValueFrom != nil {
		t.Error("KYBER_TELEGRAM_MCP_URL is a loopback address, not a secret — it must be a literal value")
	}
}

func TestClaudeCodeAdapter_EnvVars_NoTelegramWiringIfDisabled(t *testing.T) {
	a := &ClaudeCodeAdapter{}
	agent := claudeCodeAgent(kyberv1.AgentAuthTypeOAuth)
	// TelegramEnabled is false — neither the (now retired) token nor the MCP
	// URL may appear, or the start script would try to register a server that
	// no sidecar is listening on.

	for _, v := range a.EnvVars(agent) {
		if v.Name == "TELEGRAM_BOT_TOKEN" || v.Name == "KYBER_TELEGRAM_MCP_URL" {
			t.Errorf("%s must not be present when TelegramEnabled is false", v.Name)
		}
	}
}

func TestClaudeCodeAdapter_EnvVars_DiscordGivesMCPURL(t *testing.T) {
	a := &ClaudeCodeAdapter{}
	agent := claudeCodeAgent(kyberv1.AgentAuthTypeOAuth)
	agent.Spec.Channels = &kyberv1.AgentChannels{Discord: &kyberv1.AgentDiscordChannel{}}
	for _, v := range a.EnvVars(agent) {
		if v.Name == "KYBER_DISCORD_MCP_URL" {
			if v.Value != runtimes.DiscordMCPURL() || v.ValueFrom != nil {
				t.Fatalf("KYBER_DISCORD_MCP_URL = %+v, want literal %q", v, runtimes.DiscordMCPURL())
			}
			return
		}
	}
	t.Fatal("KYBER_DISCORD_MCP_URL not found")
}

// TestClaudeCodeAdapter_EnvVars_IncludesDiscordWebhookIfEnabled verifies
// kyber#132 Phase 1: KYBER_DISCORD_WEBHOOK is injected as a SecretKeyRef
// off the per-agent <agent-name>-discord secret's webhook-url key when
// the operator has enabled Discord on the agent's spec.
func TestClaudeCodeAdapter_EnvVars_IncludesDiscordWebhookIfEnabled(t *testing.T) {
	a := &ClaudeCodeAdapter{}
	agent := claudeCodeAgent(kyberv1.AgentAuthTypeOAuth)
	agent.Spec.Secrets.DiscordEnabled = true

	vars := a.EnvVars(agent)
	envMap := make(map[string]corev1.EnvVar)
	for _, v := range vars {
		envMap[v.Name] = v
	}

	dv, ok := envMap["KYBER_DISCORD_WEBHOOK"]
	if !ok {
		t.Fatal("KYBER_DISCORD_WEBHOOK env var not found when DiscordEnabled is true")
	}
	if dv.ValueFrom == nil || dv.ValueFrom.SecretKeyRef == nil {
		t.Fatal("KYBER_DISCORD_WEBHOOK must use valueFrom.secretKeyRef")
	}
	wantSecretName := "dave-discord"
	if dv.ValueFrom.SecretKeyRef.Name != wantSecretName {
		t.Errorf("KYBER_DISCORD_WEBHOOK secretKeyRef.name: got %q, want %q",
			dv.ValueFrom.SecretKeyRef.Name, wantSecretName)
	}
	if dv.ValueFrom.SecretKeyRef.Key != "webhook-url" {
		t.Errorf("KYBER_DISCORD_WEBHOOK secretKeyRef.key: got %q, want %q",
			dv.ValueFrom.SecretKeyRef.Key, "webhook-url")
	}
}

// TestClaudeCodeAdapter_EnvVars_NoDiscordWebhookIfDisabled mirrors the
// Telegram pattern: when the operator hasn't enabled Discord, the env var
// MUST NOT be in the pod spec at all (an empty-valued env var would still
// surface inside the container, which is a confusing operator UX).
func TestClaudeCodeAdapter_EnvVars_NoDiscordWebhookIfDisabled(t *testing.T) {
	a := &ClaudeCodeAdapter{}
	agent := claudeCodeAgent(kyberv1.AgentAuthTypeOAuth)
	// DiscordEnabled is false (default).
	vars := a.EnvVars(agent)
	for _, v := range vars {
		if v.Name == "KYBER_DISCORD_WEBHOOK" {
			t.Error("KYBER_DISCORD_WEBHOOK must not be present when DiscordEnabled is false")
		}
	}
}

func TestClaudeCodeAdapter_EnvVars_IncludesAgentNameAndModel(t *testing.T) {
	a := &ClaudeCodeAdapter{}
	agent := claudeCodeAgent(kyberv1.AgentAuthTypeOAuth)

	vars := a.EnvVars(agent)
	envMap := make(map[string]corev1.EnvVar)
	for _, v := range vars {
		envMap[v.Name] = v
	}

	// CLAUDE_MODEL must always be present with the agent's model.
	modelVar, ok := envMap["CLAUDE_MODEL"]
	if !ok {
		t.Fatal("CLAUDE_MODEL env var not found")
	}
	if modelVar.Value != "claude-sonnet-4" {
		t.Errorf("CLAUDE_MODEL: got %q, want %q", modelVar.Value, "claude-sonnet-4")
	}

	// AGENT_NAME is injected by pod_builder, not the adapter — verify it is absent here
	// so pod_builder's unconditional injection is not duplicated.
	if _, found := envMap["AGENT_NAME"]; found {
		t.Error("AGENT_NAME must not be returned by adapter.EnvVars (pod_builder injects it)")
	}
}

// TestClaudeCodeAdapter_EnvVars_RequestedCCVersion pins the kyber#377
// contract: spec.RuntimeVersion is plumbed through to the pod env as
// KYBER_REQUESTED_CC_VERSION. start-claude.sh's install branch keys off
// this value; empty (the existing-installs default) means "use the
// baked-in pin." Validation against the charset pattern happens
// server-side via the kubebuilder marker — by the time the adapter sees
// the field, it's safe to interpolate, but the shell re-validates as
// defense in depth.
func TestClaudeCodeAdapter_EnvVars_RequestedCCVersion(t *testing.T) {
	a := &ClaudeCodeAdapter{}
	cases := []struct {
		name    string
		version string
		want    string
	}{
		{"empty means use baked-in", "", ""},
		{"semver-ish version plumbed verbatim", "2.1.119", "2.1.119"},
		{"latest tag plumbed verbatim", "latest", "latest"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			agent := claudeCodeAgent(kyberv1.AgentAuthTypeOAuth)
			agent.Spec.RuntimeVersion = tc.version
			vars := a.EnvVars(agent)
			var got *corev1.EnvVar
			for i := range vars {
				if vars[i].Name == RequestedCCVersionEnvVar {
					got = &vars[i]
					break
				}
			}
			if got == nil {
				t.Fatalf("%s env var missing", RequestedCCVersionEnvVar)
			}
			if got.Value != tc.want {
				t.Errorf("%s: got %q, want %q", RequestedCCVersionEnvVar, got.Value, tc.want)
			}
		})
	}
}

// TestClaudeCodeAdapter_EnvVars_ModelContextWindow pins the data-driven
// [1m] decision contract: the adapter looks up the agent's model in
// tokenreport.LimitFor and emits the result as
// KYBER_MODEL_CONTEXT_WINDOW. start-claude.sh's arithmetic gate
// (>= 1_000_000 → append "[1m]") replaces the previous hardcoded shell
// `case`. Unknown models fall back to the 200K floor per LimitFor —
// safe degradation, not a crash.
func TestClaudeCodeAdapter_EnvVars_ModelContextWindow(t *testing.T) {
	a := &ClaudeCodeAdapter{}
	cases := []struct {
		name  string
		model string
		want  string
	}{
		{"1M model emits 1_000_000", "claude-opus-4-7", "1000000"},
		{"200K model emits 200_000", "claude-sonnet-4-5", "200000"},
		{"unknown model falls back to 200K floor", "claude-fictional-9", "200000"},
		{"empty model also falls back to 200K floor", "", "200000"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			agent := claudeCodeAgent(kyberv1.AgentAuthTypeOAuth)
			agent.Spec.Model = tc.model
			vars := a.EnvVars(agent)
			var got *corev1.EnvVar
			for i := range vars {
				if vars[i].Name == ModelContextWindowEnvVar {
					got = &vars[i]
					break
				}
			}
			if got == nil {
				t.Fatalf("%s env var missing", ModelContextWindowEnvVar)
			}
			if got.Value != tc.want {
				t.Errorf("%s for model %q: got %q, want %q", ModelContextWindowEnvVar, tc.model, got.Value, tc.want)
			}
		})
	}
}

// fakeOverrideResolver / fakeSnapshotResolver: in-memory stand-ins for the
// operator-override ConfigMap reader (contextwindowmap.Resolver) and the
// detection-snapshot reader (runtimedetect.SnapshotResolver). No Redis / API
// server — they just return whatever the test maps.
type fakeOverrideResolver map[string]int64

func (f fakeOverrideResolver) LookupOr(_ context.Context, id string) (int64, bool) {
	if v, ok := f[id]; ok {
		return v, true
	}
	return 200_000, false
}

type fakeSnapshotResolver map[string]int64

func (f fakeSnapshotResolver) LookupWindow(_ context.Context, id string) (int64, bool) {
	if v, ok := f[id]; ok {
		return v, true
	}
	return 0, false
}

// modelContextWindow extracts KYBER_MODEL_CONTEXT_WINDOW from EnvVars output.
func modelContextWindow(t *testing.T, a *ClaudeCodeAdapter, model string) string {
	t.Helper()
	agent := claudeCodeAgent(kyberv1.AgentAuthTypeOAuth)
	agent.Spec.Model = model
	for _, v := range a.EnvVars(agent) {
		if v.Name == ModelContextWindowEnvVar {
			return v.Value
		}
	}
	t.Fatalf("%s env var missing for model %q", ModelContextWindowEnvVar, model)
	return ""
}

// TestClaudeCodeAdapter_EnvVars_SnapshotWindow pins #492 (AC-B4 follow-on):
// the detection snapshot sizes KYBER_MODEL_CONTEXT_WINDOW for a model that is
// absent from both knownModels and the override ConfigMap, with the four-layer
// precedence override → snapshot → tokenreport.LimitFor → floor.
//
// "claude-opus-4-8" is intentionally NOT in tokenreport.knownModels, so
// LimitFor floors it at 200K — the exact new-model case the epic exists to fix.
func TestClaudeCodeAdapter_EnvVars_SnapshotWindow(t *testing.T) {
	const newModel = "claude-opus-4-8" // unknown to knownModels → LimitFor floor

	t.Run("snapshot supplies 1M for a model unknown to knownModels and unmapped in override (AC-B4)", func(t *testing.T) {
		a := &ClaudeCodeAdapter{Snapshots: fakeSnapshotResolver{newModel: 1_000_000}}
		if got := modelContextWindow(t, a, newModel); got != "1000000" {
			t.Errorf("got %q, want 1000000 (snapshot beats LimitFor floor)", got)
		}
	})

	t.Run("override ConfigMap beats the snapshot", func(t *testing.T) {
		a := &ClaudeCodeAdapter{
			ContextWindows: fakeOverrideResolver{newModel: 500_000},
			Snapshots:      fakeSnapshotResolver{newModel: 1_000_000},
		}
		if got := modelContextWindow(t, a, newModel); got != "500000" {
			t.Errorf("got %q, want 500000 (operator override wins over snapshot)", got)
		}
	})

	t.Run("empty/unknown snapshot falls through to the LimitFor floor", func(t *testing.T) {
		a := &ClaudeCodeAdapter{Snapshots: fakeSnapshotResolver{}} // model absent → (0,false)
		if got := modelContextWindow(t, a, newModel); got != "200000" {
			t.Errorf("got %q, want 200000 (cold-start safety: floor preserved)", got)
		}
	})

	t.Run("nil Snapshots leaves behavior unchanged", func(t *testing.T) {
		a := &ClaudeCodeAdapter{} // no resolvers at all
		if got := modelContextWindow(t, a, newModel); got != "200000" {
			t.Errorf("got %q, want 200000 (nil snapshot resolver → LimitFor)", got)
		}
	})

	t.Run("no regression: knownModels entry resolves correctly even with a snapshot present", func(t *testing.T) {
		// claude-opus-4-7 IS in knownModels at 1M; a snapshot need not even
		// carry it. The override/snapshot layers must not disturb this.
		a := &ClaudeCodeAdapter{Snapshots: fakeSnapshotResolver{}}
		if got := modelContextWindow(t, a, "claude-opus-4-7"); got != "1000000" {
			t.Errorf("got %q, want 1000000 (knownModels path unchanged)", got)
		}
	})
}

func TestClaudeCodeAdapter_LivenessProbe(t *testing.T) {
	a := &ClaudeCodeAdapter{}
	probe := a.LivenessProbe()

	if probe == nil {
		t.Fatal("LivenessProbe() returned nil")
	}
	if probe.Exec == nil {
		t.Fatal("LivenessProbe must use an exec probe")
	}
	wantCmd := []string{"pgrep", "-f", "claude"}
	if len(probe.Exec.Command) != len(wantCmd) {
		t.Fatalf("LivenessProbe command length: got %d, want %d", len(probe.Exec.Command), len(wantCmd))
	}
	for i, arg := range wantCmd {
		if probe.Exec.Command[i] != arg {
			t.Errorf("LivenessProbe command[%d]: got %q, want %q", i, probe.Exec.Command[i], arg)
		}
	}
	if probe.InitialDelaySeconds != 30 {
		t.Errorf("LivenessProbe.InitialDelaySeconds: got %d, want 30", probe.InitialDelaySeconds)
	}
	if probe.PeriodSeconds != 30 {
		t.Errorf("LivenessProbe.PeriodSeconds: got %d, want 30", probe.PeriodSeconds)
	}
	if probe.FailureThreshold != 3 {
		t.Errorf("LivenessProbe.FailureThreshold: got %d, want 3", probe.FailureThreshold)
	}
}

func TestClaudeCodeAdapter_ReadinessProbe(t *testing.T) {
	a := &ClaudeCodeAdapter{}
	probe := a.ReadinessProbe()

	if probe == nil {
		t.Fatal("ReadinessProbe() returned nil")
	}
	if probe.Exec == nil {
		t.Fatal("ReadinessProbe must use an exec probe")
	}
	// Readiness uses pgrep like liveness, with shorter initial delay.
	wantCmd := []string{"pgrep", "-f", "claude"}
	if len(probe.Exec.Command) != len(wantCmd) {
		t.Fatalf("ReadinessProbe command length: got %d, want %d", len(probe.Exec.Command), len(wantCmd))
	}
	for i, arg := range wantCmd {
		if probe.Exec.Command[i] != arg {
			t.Errorf("ReadinessProbe command[%d]: got %q, want %q", i, probe.Exec.Command[i], arg)
		}
	}
	// Readiness has shorter initial delay than liveness (15s vs 30s).
	if probe.InitialDelaySeconds >= 30 {
		t.Errorf("ReadinessProbe.InitialDelaySeconds should be < 30 (liveness), got %d", probe.InitialDelaySeconds)
	}
}

func TestClaudeCodeAdapter_SessionPaths(t *testing.T) {
	a := &ClaudeCodeAdapter{}

	wantBrief := "/persist/session-brief.json"
	if got := a.SessionBriefPath(); got != wantBrief {
		t.Errorf("SessionBriefPath(): got %q, want %q", got, wantBrief)
	}

	wantState := "/persist/session-state.json"
	if got := a.SessionStatePath(); got != wantState {
		t.Errorf("SessionStatePath(): got %q, want %q", got, wantState)
	}
}

func TestClaudeCodeAdapter_PreStopCommand(t *testing.T) {
	a := &ClaudeCodeAdapter{}
	cmd := a.PreStopCommand()
	if len(cmd) == 0 {
		t.Fatal("PreStopCommand() returned nil/empty; the Telegram slot-release hook must be set")
	}
	// Must enter PID 1's namespaces (mirrors RestartSessionCommand) so pkill
	// sees the agent's process view, then SIGTERM the Telegram plugin poller.
	if cmd[0] != "nsenter" || cmd[1] != "--target" || cmd[2] != "1" {
		t.Errorf("PreStopCommand() must start with `nsenter --target 1`; got %v", cmd[:3])
	}
	if !containsArg(cmd, "--pid") {
		t.Errorf("PreStopCommand() must enter the PID namespace (--pid) for pkill; got %v", cmd)
	}
	script := cmd[len(cmd)-1]
	for _, want := range []string{"pkill -TERM", "server\\.ts", "|| true", "sleep 3"} {
		if !strings.Contains(script, want) {
			t.Errorf("PreStopCommand() script missing %q; got %q", want, script)
		}
	}
	// Must target the poller, not permanently disable the bot.
	for _, bad := range []string{"logOut", "deleteWebhook", "rm "} {
		if strings.Contains(script, bad) {
			t.Errorf("PreStopCommand() script must not contain %q (would over-reach); got %q", bad, script)
		}
	}
}

func containsArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

func TestClaudeCodeAdapter_ModelEnvVar(t *testing.T) {
	a := &ClaudeCodeAdapter{}
	if got := a.ModelEnvVar(); got != "CLAUDE_MODEL" {
		t.Errorf("ModelEnvVar(): got %q, want %q", got, "CLAUDE_MODEL")
	}
}

func TestClaudeCodeAdapter_GracefulShutdownSeconds(t *testing.T) {
	a := &ClaudeCodeAdapter{}
	if got := a.GracefulShutdownSeconds(); got != 30 {
		t.Errorf("GracefulShutdownSeconds(): got %d, want 30", got)
	}
}

func TestClaudeCodeAdapter_SecretMounts_Empty(t *testing.T) {
	a := &ClaudeCodeAdapter{}
	agent := claudeCodeAgent(kyberv1.AgentAuthTypeOAuth)
	agent.Spec.Secrets.TelegramEnabled = true

	mounts := a.SecretMounts(agent)
	if len(mounts) != 0 {
		t.Errorf("SecretMounts(): expected empty slice, got %d mounts", len(mounts))
	}
}

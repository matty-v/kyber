package contracttest_test

import (
	"strings"
	"testing"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/runtimes"
	"github.com/matty-v/kyber/pkg/runtimes/claudecode"
	"github.com/matty-v/kyber/pkg/runtimes/codex"
	"github.com/matty-v/kyber/pkg/runtimes/contracttest"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestInteractiveAdapters(t *testing.T) {
	t.Setenv(claudecode.AgentRuntimeImageEnv, "test.invalid/claude:pinned")
	t.Setenv(codex.RuntimeImageEnv, "test.invalid/codex:pinned")
	cases := []struct {
		adapter runtimes.Adapter
		auth    []contracttest.AuthCase
	}{
		{claudecode.NewClaudeCodeAdapter(), []contracttest.AuthCase{
			{Mode: kyberv1.AgentAuthTypeOAuth, CredentialSuffix: "-oauth", RequiredEnvKeys: map[string]string{"CLAUDE_ACCESS_TOKEN": "access_token", "CLAUDE_REFRESH_TOKEN": "refresh_token"}, ForbiddenEnv: []string{"ANTHROPIC_API_KEY"}},
			{Mode: kyberv1.AgentAuthTypeAPIKey, CredentialSuffix: "-anthropic", RequiredEnvKeys: map[string]string{"ANTHROPIC_API_KEY": "token"}, ForbiddenEnv: []string{"CLAUDE_ACCESS_TOKEN", "CLAUDE_REFRESH_TOKEN"}},
		}},
		{codex.NewAdapter(), []contracttest.AuthCase{
			{Mode: kyberv1.AgentAuthTypeOAuth, CredentialSuffix: "-codex-auth", RequiredEnvKeys: map[string]string{"CODEX_AUTH_JSON": "auth.json"}, ForbiddenEnv: []string{"OPENAI_API_KEY"}},
			{Mode: kyberv1.AgentAuthTypeAPIKey, CredentialSuffix: "-openai", RequiredEnvKeys: map[string]string{"OPENAI_API_KEY": "token"}},
		}},
		{newFixture(), []contracttest.AuthCase{{Mode: kyberv1.AgentAuthTypeAPIKey, CredentialSuffix: "-fixture-key", RequiredEnvKeys: map[string]string{"FIXTURE_KEY": "token"}}}},
	}
	for _, tc := range cases {
		for _, auth := range tc.auth {
			for _, name := range []string{"contract-a", "contract-b"} {
				t.Run(tc.adapter.Type()+"/"+string(auth.Mode)+"/"+name, func(t *testing.T) {
					agent := &kyberv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: name}}
					agent.Spec.Runtime = tc.adapter.Type()
					agent.Spec.Model = "contract-model"
					agent.Spec.Secrets.AuthType = auth.Mode
					for _, err := range contracttest.CheckAdapter(tc.adapter, agent, auth) {
						t.Error(err)
					}
				})
			}
		}
	}
}

// fixture is a minimal new adapter, not a third production harness. Optional
// commands remain nil, proving the checker does not require Claude/Codex features.
type fixture struct {
	runtimes.Adapter
	wrongSecret, emptyCommand bool
}

func newFixture() *fixture {
	probe := &corev1.Probe{ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"true"}}}}
	return &fixture{Adapter: runtimes.NewStubAdapter("test.invalid/fixture:pinned", []string{"/fixture/start"}, nil, nil, probe, probe, 30, "/persist/brief.json", "/persist/state.json", "FIXTURE_MODEL")}
}
func (f *fixture) Type() string { return "contract-fixture" }
func (f *fixture) CredentialSecretName(a *kyberv1.Agent) string {
	if f.wrongSecret {
		return "another-agent-fixture-key"
	}
	return a.Name + "-fixture-key"
}
func (f *fixture) EnvVars(a *kyberv1.Agent) []corev1.EnvVar {
	return []corev1.EnvVar{
		{Name: "FIXTURE_MODEL", Value: a.Spec.Model},
		{Name: "FIXTURE_KEY", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: f.CredentialSecretName(a)}, Key: "token"}}},
	}
}
func (f *fixture) CompactSessionCommand() []string {
	if f.emptyCommand {
		return []string{}
	}
	return nil
}

func TestCheckerRejectsContractViolations(t *testing.T) {
	for _, tc := range []struct {
		name, id string
		breakIt  func(*fixture)
	}{
		{"cross-agent credentials", "HC-05", func(f *fixture) { f.wrongSecret = true }},
		{"ambiguous unsupported command", "HC-04", func(f *fixture) { f.emptyCommand = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture()
			tc.breakIt(f)
			a := &kyberv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "contract-a"}}
			a.Spec.Runtime = f.Type()
			a.Spec.Secrets.AuthType = kyberv1.AgentAuthTypeAPIKey
			errs := contracttest.CheckAdapter(f, a, contracttest.AuthCase{Mode: kyberv1.AgentAuthTypeAPIKey, CredentialSuffix: "-fixture-key", RequiredEnvKeys: map[string]string{"FIXTURE_KEY": "token"}})
			for _, err := range errs {
				if strings.HasPrefix(err.Error(), tc.id+":") {
					return
				}
			}
			t.Fatalf("expected %s violation, got %v", tc.id, errs)
		})
	}
}

// Register the minimal fixture through the same registry as real runtimes.
// This proves the Go extension point, not the out-of-interface boot/receipt path.
type fixtureRuntime struct{ adapter *fixture }

func (f fixtureRuntime) Type() string              { return f.adapter.Type() }
func (f fixtureRuntime) Adapter() runtimes.Adapter { return f.adapter }
func (f fixtureRuntime) Probe() runtimes.Probe     { return fixtureProbe{} }

type fixtureProbe struct{}

func (fixtureProbe) Type() string { return "contract-fixture" }
func TestFixtureUsesRuntimeRegistry(t *testing.T) {
	f := fixtureRuntime{newFixture()}
	if _, exists := runtimes.Get(f.Type()); !exists {
		runtimes.Register(f)
	}
	got, ok := runtimes.Get(f.Type())
	if !ok || got.Adapter().Type() != f.Type() || got.Probe().Type() != f.Type() {
		t.Fatal("fixture did not resolve through registry")
	}
	if got.Adapter().CompactSessionCommand() != nil {
		t.Fatal("fixture unexpectedly offers compaction")
	}
}

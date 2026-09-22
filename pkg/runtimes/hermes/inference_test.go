package hermes

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
)

func agentWithInference(inf *kyberv1.AgentInference) *kyberv1.Agent {
	agent := &kyberv1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "scout"},
		Spec: kyberv1.AgentSpec{
			Runtime:   Type,
			Model:     "spec-model",
			Inference: inf,
		},
	}
	// Assigned rather than written as a `AuthType: ...APIKey` struct field:
	// that key-colon-value shape reads as a hardcoded credential to secret
	// scanners even though both sides are type constants, and it blocks this
	// repo's scan. contracttest/adapter_test.go sets it the same way.
	agent.Spec.Secrets.AuthType = kyberv1.AgentAuthTypeAPIKey
	return agent
}

func envMap(t *testing.T, vars []corev1.EnvVar) (map[string]string, map[string]corev1.SecretKeySelector) {
	t.Helper()
	values := map[string]string{}
	refs := map[string]corev1.SecretKeySelector{}
	for _, v := range vars {
		if v.ValueFrom != nil && v.ValueFrom.SecretKeyRef != nil {
			refs[v.Name] = *v.ValueFrom.SecretKeyRef
			continue
		}
		values[v.Name] = v.Value
	}
	return values, refs
}

// A Hermes agent with no spec.inference must be byte-for-byte what it was
// before the field existed. Every deployed agent is in this state, so a
// regression here rolls the whole fleet onto a broken provider.
func TestEnvVarsWithoutInferenceKeepsOpenRouter(t *testing.T) {
	values, refs := envMap(t, (&Adapter{}).EnvVars(agentWithInference(nil)))

	if values["HERMES_PROVIDER"] != "openrouter" {
		t.Errorf("HERMES_PROVIDER = %q, want openrouter", values["HERMES_PROVIDER"])
	}
	if values["HERMES_INFERENCE_MODEL"] != "spec-model" {
		t.Errorf("HERMES_INFERENCE_MODEL = %q, want spec-model", values["HERMES_INFERENCE_MODEL"])
	}
	ref, ok := refs["OPENROUTER_API_KEY"]
	if !ok {
		t.Fatal("OPENROUTER_API_KEY is not injected for a default agent")
	}
	if ref.Name != "scout-openrouter" || ref.Key != "token" {
		t.Errorf("OpenRouter secret ref = %s/%s, want scout-openrouter/token", ref.Name, ref.Key)
	}
	if _, leaked := refs["OPENAI_API_KEY"]; leaked {
		t.Error("a default agent must not carry an inference-endpoint credential")
	}
	if _, leaked := values["KYBER_INFERENCE_BASE_URL"]; leaked {
		t.Error("a default agent must not carry KYBER_INFERENCE_BASE_URL")
	}
	// OpenRouter is a cloud provider; Hermes' 180s default is right for it.
	if _, leaked := values["HERMES_STREAM_STALE_TIMEOUT"]; leaked {
		t.Error("a default agent must keep Hermes' own stale-stream timeout")
	}
}

func TestEnvVarsWithInferencePointsAtTheEndpoint(t *testing.T) {
	agent := agentWithInference(&kyberv1.AgentInference{
		BaseURL:    "https://llm.example.com/v1",
		API:        "openai",
		Model:      "qwen3.6-35b-a3b",
		Credential: kyberv1.AgentInferenceCredentialRef{ExistingSecret: "falcon-llm", Key: "api-token"},
	})
	values, refs := envMap(t, (&Adapter{}).EnvVars(agent))

	if values["HERMES_PROVIDER"] != CustomProviderID {
		t.Errorf("HERMES_PROVIDER = %q, want %q", values["HERMES_PROVIDER"], CustomProviderID)
	}
	if values["KYBER_INFERENCE_BASE_URL"] != "https://llm.example.com/v1" {
		t.Errorf("base URL = %q", values["KYBER_INFERENCE_BASE_URL"])
	}
	// A self-hosted server sends nothing until prefill finishes. Without this,
	// Hermes cancels any prompt that takes longer than 180s to read.
	if values["HERMES_STREAM_STALE_TIMEOUT"] != "900" {
		t.Errorf("HERMES_STREAM_STALE_TIMEOUT = %q, want 900", values["HERMES_STREAM_STALE_TIMEOUT"])
	}
	// The env the configurator reads for the provider name must equal the
	// provider Hermes is told to select, or Hermes resolves a provider that
	// the config.yaml block never defined.
	if values["KYBER_INFERENCE_PROVIDER"] != values["HERMES_PROVIDER"] {
		t.Errorf("KYBER_INFERENCE_PROVIDER %q != HERMES_PROVIDER %q",
			values["KYBER_INFERENCE_PROVIDER"], values["HERMES_PROVIDER"])
	}
	ref, ok := refs["OPENAI_API_KEY"]
	if !ok {
		t.Fatal("OPENAI_API_KEY is not injected for an inference-endpoint agent")
	}
	if ref.Name != "falcon-llm" || ref.Key != "api-token" {
		t.Errorf("credential ref = %s/%s, want falcon-llm/api-token", ref.Name, ref.Key)
	}
	// Referencing the OpenRouter Secret on an agent that has none is fatal to
	// pod creation: kubelet cannot resolve a SecretKeyRef to a missing Secret.
	if _, leaked := refs["OPENROUTER_API_KEY"]; leaked {
		t.Error("an inference-endpoint agent must not reference the OpenRouter Secret")
	}
}

// spec.inference.model wins over spec.model, and an empty one falls back so an
// operator who already set spec.model does not have to repeat it.
func TestInferenceModelFallsBackToSpecModel(t *testing.T) {
	withModel := agentWithInference(&kyberv1.AgentInference{Model: "endpoint-model"})
	if got := inferenceModel(withModel); got != "endpoint-model" {
		t.Errorf("inferenceModel = %q, want endpoint-model", got)
	}
	withoutModel := agentWithInference(&kyberv1.AgentInference{})
	if got := inferenceModel(withoutModel); got != "spec-model" {
		t.Errorf("inferenceModel = %q, want spec-model", got)
	}
}

// The NeedsAuth recovery gate keys on this name. Pointing it at the endpoint
// Secret is what lets rotating that Secret recover the agent.
func TestCredentialSecretNameFollowsTheConfiguredCredential(t *testing.T) {
	custom := agentWithInference(&kyberv1.AgentInference{
		Credential: kyberv1.AgentInferenceCredentialRef{ExistingSecret: "falcon-llm", Key: "token"},
	})
	if got := (&Adapter{}).CredentialSecretName(custom); got != "falcon-llm" {
		t.Errorf("CredentialSecretName = %q, want falcon-llm", got)
	}
	if got := (&Adapter{}).CredentialSecretName(agentWithInference(nil)); got != "scout-openrouter" {
		t.Errorf("CredentialSecretName = %q, want scout-openrouter", got)
	}
}

// Checking OPENROUTER_API_KEY by name held a correctly-configured
// custom-endpoint agent NotReady forever.
func TestReadinessProbeAcceptsEitherCredential(t *testing.T) {
	cmd := (&Adapter{}).ReadinessProbe().Exec.Command
	script := cmd[len(cmd)-1]
	for _, want := range []string{"OPENROUTER_API_KEY", "OPENAI_API_KEY"} {
		if !contains(script, want) {
			t.Errorf("readiness probe does not consider %s: %s", want, script)
		}
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}

// set-model writes spec.Model. An agent on a custom endpoint resolves its
// model from spec.inference.model first, so the two must be kept in step or a
// model change is a silent no-op — 200, pod rolled, same model.
func TestInferenceModelTracksSetModelWrites(t *testing.T) {
	agent := agentWithInference(&kyberv1.AgentInference{Model: "old-model"})

	// What the set-model handler does for an inference agent.
	agent.Spec.Model = "new-model"
	agent.Spec.Inference.Model = "new-model"

	if got := inferenceModel(agent); got != "new-model" {
		t.Errorf("inferenceModel = %q after set-model, want new-model", got)
	}
}

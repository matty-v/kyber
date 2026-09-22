package codex

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
)

func codexAgent(inf *kyberv1.AgentInference, auth kyberv1.AgentAuthType) *kyberv1.Agent {
	agent := &kyberv1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "pilot"},
		Spec: kyberv1.AgentSpec{
			Runtime:   "codex",
			Model:     "spec-model",
			Inference: inf,
		},
	}
	agent.Spec.Secrets.AuthType = auth
	return agent
}

func splitEnv(vars []corev1.EnvVar) (map[string]string, map[string]corev1.SecretKeySelector) {
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

// Every deployed Codex agent is in this state. A regression here rolls the
// whole fleet onto a broken credential path.
func TestEnvVarsWithoutInferenceIsUnchanged(t *testing.T) {
	for _, auth := range []kyberv1.AgentAuthType{kyberv1.AgentAuthTypeOAuth, kyberv1.AgentAuthTypeAPIKey} {
		values, refs := splitEnv((&Adapter{}).EnvVars(codexAgent(nil, auth)))

		if values["CODEX_MODEL"] != "spec-model" {
			t.Errorf("auth %s: CODEX_MODEL = %q", auth, values["CODEX_MODEL"])
		}
		if _, ok := refs["CODEX_AUTH_JSON"]; !ok {
			t.Errorf("auth %s: CODEX_AUTH_JSON must still be injected", auth)
		}
		if _, leaked := values["KYBER_INFERENCE_BASE_URL"]; leaked {
			t.Errorf("auth %s: a default agent must not carry an endpoint", auth)
		}
		wantOpenAI := auth == kyberv1.AgentAuthTypeAPIKey
		if _, ok := refs["OPENAI_API_KEY"]; ok != wantOpenAI {
			t.Errorf("auth %s: OPENAI_API_KEY present = %v, want %v", auth, ok, wantOpenAI)
		}
	}
}

func TestEnvVarsWithInferencePointsAtTheEndpoint(t *testing.T) {
	agent := codexAgent(&kyberv1.AgentInference{
		BaseURL:    "https://llm.voget.io/v1",
		API:        "openai",
		Model:      "qwen3.6-35b-a3b",
		Credential: kyberv1.AgentInferenceCredentialRef{ExistingSecret: "pilot-inference", Key: "token"},
	}, kyberv1.AgentAuthTypeOAuth)
	values, refs := splitEnv((&Adapter{}).EnvVars(agent))

	if values["KYBER_INFERENCE_BASE_URL"] != "https://llm.voget.io/v1" {
		t.Errorf("base URL = %q", values["KYBER_INFERENCE_BASE_URL"])
	}
	if values["KYBER_INFERENCE_PROVIDER"] != CustomProviderID {
		t.Errorf("provider = %q, want %q", values["KYBER_INFERENCE_PROVIDER"], CustomProviderID)
	}
	if values["CODEX_MODEL"] != "qwen3.6-35b-a3b" {
		t.Errorf("CODEX_MODEL = %q, want the endpoint model", values["CODEX_MODEL"])
	}
	ref, ok := refs[InferenceCredentialEnv]
	if !ok {
		t.Fatalf("%s is not injected for an endpoint agent", InferenceCredentialEnv)
	}
	if ref.Name != "pilot-inference" || ref.Key != "token" {
		t.Errorf("credential ref = %s/%s", ref.Name, ref.Key)
	}
	// A SecretKeyRef to a Secret that does not exist is fatal to pod creation.
	// An endpoint agent has neither of the Codex credential Secrets.
	if _, leaked := refs["CODEX_AUTH_JSON"]; leaked {
		t.Error("an endpoint agent must not reference the codex-auth Secret")
	}
}

// Even on an api-key agent, the endpoint credential must replace the OpenAI
// one rather than both being referenced.
func TestEndpointReplacesTheOpenAIKeyOnAPIKeyAgents(t *testing.T) {
	agent := codexAgent(&kyberv1.AgentInference{
		BaseURL:    "https://llm.voget.io/v1",
		API:        "openai",
		Credential: kyberv1.AgentInferenceCredentialRef{ExistingSecret: "pilot-inference", Key: "token"},
	}, kyberv1.AgentAuthTypeAPIKey)
	_, refs := splitEnv((&Adapter{}).EnvVars(agent))

	ref := refs[InferenceCredentialEnv]
	if ref.Name != "pilot-inference" {
		t.Errorf("%s resolves to %q, want the endpoint Secret", InferenceCredentialEnv, ref.Name)
	}
	if _, leaked := refs["CODEX_AUTH_JSON"]; leaked {
		t.Error("codex-auth must not be referenced alongside an endpoint")
	}
}

func TestInferenceModelFallsBackToSpecModel(t *testing.T) {
	withModel := codexAgent(&kyberv1.AgentInference{Model: "endpoint-model"}, kyberv1.AgentAuthTypeOAuth)
	if got := inferenceModel(withModel); got != "endpoint-model" {
		t.Errorf("inferenceModel = %q, want endpoint-model", got)
	}
	withoutModel := codexAgent(&kyberv1.AgentInference{}, kyberv1.AgentAuthTypeOAuth)
	if got := inferenceModel(withoutModel); got != "spec-model" {
		t.Errorf("inferenceModel = %q, want spec-model", got)
	}
}

// The NeedsAuth recovery gate keys on this name, so rotating the endpoint
// Secret is what recovers an endpoint agent.
func TestCredentialSecretNameFollowsTheConfiguredCredential(t *testing.T) {
	endpoint := codexAgent(&kyberv1.AgentInference{
		Credential: kyberv1.AgentInferenceCredentialRef{ExistingSecret: "pilot-inference", Key: "token"},
	}, kyberv1.AgentAuthTypeOAuth)
	if got := (&Adapter{}).CredentialSecretName(endpoint); got != "pilot-inference" {
		t.Errorf("CredentialSecretName = %q, want pilot-inference", got)
	}
	if got := (&Adapter{}).CredentialSecretName(codexAgent(nil, kyberv1.AgentAuthTypeOAuth)); got != "pilot-codex-auth" {
		t.Errorf("CredentialSecretName = %q, want pilot-codex-auth", got)
	}
	if got := (&Adapter{}).CredentialSecretName(codexAgent(nil, kyberv1.AgentAuthTypeAPIKey)); got != "pilot-openai" {
		t.Errorf("CredentialSecretName = %q, want pilot-openai", got)
	}
}

// Clearing spec.inference on an agent that never had <agent>-openai minted must
// not make the pod unschedulable. A REQUIRED SecretKeyRef to a missing Secret
// fails with CreateContainerConfigError before the boot script runs, so the
// agent never reaches the exit-42/NeedsAuth path and cannot be recovered
// through the auth UI.
func TestClearedInferenceLeavesCredentialRefsOptional(t *testing.T) {
	agent := codexAgent(nil, kyberv1.AgentAuthTypeAPIKey)
	for _, v := range (&Adapter{}).EnvVars(agent) {
		if v.ValueFrom == nil || v.ValueFrom.SecretKeyRef == nil {
			continue
		}
		opt := v.ValueFrom.SecretKeyRef.Optional
		if opt == nil || !*opt {
			t.Errorf("%s references a Secret without Optional set; a missing Secret would block pod creation", v.Name)
		}
	}
}

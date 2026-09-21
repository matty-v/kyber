package api

import (
	"net/url"
	"strings"
	"testing"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
)

func validInference() *kyberv1.AgentInference {
	return &kyberv1.AgentInference{
		BaseURL:    "https://llm.example.com/v1",
		API:        "openai",
		Credential: kyberv1.AgentInferenceCredentialRef{ExistingSecret: "falcon-llm", Key: "token"},
	}
}

func TestValidateInferenceAcceptsAWellFormedEndpoint(t *testing.T) {
	if err := validateInference("hermes", validInference()); err != nil {
		t.Fatalf("valid endpoint rejected: %v", err)
	}
}

// Nil is how every existing agent looks. It must stay valid on any runtime,
// including ones that do not support the feature at all.
func TestValidateInferenceAllowsNilOnAnyRuntime(t *testing.T) {
	for _, runtime := range []string{"hermes", "claude-code", "codex", "nonexistent"} {
		if err := validateInference(runtime, nil); err != nil {
			t.Errorf("nil inference rejected on %s: %v", runtime, err)
		}
	}
}

// Writing the field on a runtime that ignores it stores a setting that
// silently does nothing — the failure mode this feature exists to remove.
func TestValidateInferenceRejectsRuntimesThatIgnoreIt(t *testing.T) {
	for _, runtime := range []string{"claude-code", "codex", "nonexistent"} {
		err := validateInference(runtime, validInference())
		if err == nil {
			t.Errorf("runtime %s accepted an inference endpoint it does not read", runtime)
			continue
		}
		if !strings.Contains(err.Error(), "does not support") {
			t.Errorf("runtime %s error = %q, want it to name the lack of support", runtime, err)
		}
	}
}

func TestValidateInferenceRejectsBadEndpoints(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*kyberv1.AgentInference)
		wantSub string
	}{
		{
			// Plain HTTP to a public host puts the bearer token on the wire
			// in cleartext.
			name:    "plaintext to a public host",
			mutate:  func(i *kyberv1.AgentInference) { i.BaseURL = "http://llm.example.com/v1" },
			wantSub: "HTTPS",
		},
		{
			// Built rather than written as a literal. A hardcoded URL with
			// userinfo in it is itself the credential-in-URL shape secret
			// scanners flag, so spelling one out here — even to prove we
			// reject it — trips the repo's scan on every push.
			name: "credentials embedded in the URL",
			mutate: func(i *kyberv1.AgentInference) {
				u := url.URL{Scheme: "https", Host: "llm.example.com", Path: "/v1"}
				// Userinfo with no password still trips the check, and keeps
				// any password-shaped literal out of this file entirely.
				u.User = url.User("operator")
				i.BaseURL = u.String()
			},
			wantSub: "must not embed credentials",
		},
		{
			name:    "query string",
			mutate:  func(i *kyberv1.AgentInference) { i.BaseURL = "https://llm.example.com/v1?key=abc" },
			wantSub: "query string",
		},
		{
			name:    "not a URL",
			mutate:  func(i *kyberv1.AgentInference) { i.BaseURL = "llm.example.com" },
			wantSub: "absolute http or https URL",
		},
		{
			name:    "unsupported scheme",
			mutate:  func(i *kyberv1.AgentInference) { i.BaseURL = "ftp://llm.example.com/v1" },
			wantSub: "absolute http or https URL",
		},
		{
			// The enum exists so anthropic can be added later; until it is
			// implemented, accepting it would produce an agent that cannot
			// talk to its endpoint.
			name:    "unimplemented wire protocol",
			mutate:  func(i *kyberv1.AgentInference) { i.API = "anthropic" },
			wantSub: "openai",
		},
		{
			name:    "secret name is not a valid k8s name",
			mutate:  func(i *kyberv1.AgentInference) { i.Credential.ExistingSecret = "Not A Secret" },
			wantSub: "existingSecret",
		},
		{
			name:    "empty secret key",
			mutate:  func(i *kyberv1.AgentInference) { i.Credential.Key = "" },
			wantSub: "credential.key",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inf := validInference()
			tc.mutate(inf)
			err := validateInference("hermes", inf)
			if err == nil {
				t.Fatalf("%s was accepted", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error = %q, want it to mention %q", err, tc.wantSub)
			}
		})
	}
}

// An in-cluster endpoint has no TLS to offer and the hop cannot leave the
// cluster, so plain HTTP is accepted for those hosts only.
func TestValidateInferenceAllowsPlaintextInsideTheCluster(t *testing.T) {
	for _, host := range []string{
		"http://llm.kyber-system.svc.cluster.local:8080/v1",
		"http://llm.kyber-system.svc:8080/v1",
		"http://llm:8080/v1",
		"http://localhost:8080/v1",
	} {
		inf := validInference()
		inf.BaseURL = host
		if err := validateInference("hermes", inf); err != nil {
			t.Errorf("cluster-internal %s rejected: %v", host, err)
		}
	}
}

// The response projection is the only place spec.inference crosses the API
// boundary. It must name where the credential lives and never carry a value.
func TestInferenceResponseCarriesNoSecretValue(t *testing.T) {
	if inferenceResponse(nil) != nil {
		t.Error("nil inference must project to nil")
	}
	got := inferenceResponse(&kyberv1.AgentInference{
		BaseURL:    "https://llm.example.com/v1",
		API:        "openai",
		Model:      "qwen3.6-35b-a3b",
		Credential: kyberv1.AgentInferenceCredentialRef{ExistingSecret: "falcon-llm", Key: "token"},
	})
	if got.CredentialSecret != "falcon-llm" || got.CredentialKey != "token" {
		t.Errorf("credential pointer = %s/%s", got.CredentialSecret, got.CredentialKey)
	}
	if got.BaseURL != "https://llm.example.com/v1" || got.API != "openai" || got.Model != "qwen3.6-35b-a3b" {
		t.Errorf("projection lost a field: %+v", got)
	}
}

// strings.Contains(host, ".svc.") matched llm.svc.attacker.com, classifying a
// public host as cluster-internal, waiving HTTPS, and putting the operator's
// bearer token on the internet in cleartext.
func TestValidateInferenceRejectsPlaintextToHostsThatMerelyLookInternal(t *testing.T) {
	for _, host := range []string{
		"http://llm.svc.attacker.com/v1",
		"http://svc.attacker.com/v1",
		"http://llm.svc.cluster.local.attacker.com/v1",
		"http://notsvc.attacker.com/v1",
	} {
		inf := validInference()
		inf.BaseURL = host
		err := validateInference("hermes", inf)
		if err == nil {
			t.Errorf("%s was treated as cluster-internal and allowed over plaintext", host)
			continue
		}
		if !strings.Contains(err.Error(), "HTTPS") {
			t.Errorf("%s error = %q, want the HTTPS requirement", host, err)
		}
	}
}

// Those same hosts are fine over HTTPS — the rule is about the plaintext
// waiver, not about the hostname itself.
func TestValidateInferenceAllowsExternalHostsOverHTTPS(t *testing.T) {
	inf := validInference()
	inf.BaseURL = "https://llm.svc.attacker.com/v1"
	if err := validateInference("hermes", inf); err != nil {
		t.Errorf("HTTPS to an external host rejected: %v", err)
	}
}

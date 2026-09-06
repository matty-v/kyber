package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/matty-v/kyber/pkg/api"
	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/runtimes"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type extensionRuntime struct{}

func (extensionRuntime) Type() string { return "extension-fixture" }
func (extensionRuntime) Adapter() runtimes.Adapter {
	probe := &corev1.Probe{ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"true"}}}}
	return extensionAdapter{runtimes.NewStubAdapter("test.invalid/fixture:pinned", []string{"/fixture/start"}, nil, nil, probe, probe, 30, "/persist/brief.json", "/persist/state.json", "FIXTURE_MODEL")}
}
func (extensionRuntime) Probe() runtimes.Probe { return extensionProbe{} }
func (extensionRuntime) Descriptor() runtimes.Descriptor {
	return runtimes.Descriptor{ID: "extension-fixture", Name: "Extension", ContractVersion: runtimes.ContractVersion, Profile: runtimes.InteractiveProfile, Cancellation: "notify_only", AuthModes: []runtimes.AuthMode{{ID: kyberv1.AgentAuthTypeAPIKey, Name: "Fixture key", Flow: "api-key", InputField: "fixtureKey", SecretSuffix: "fixture"}}}
}
func (extensionRuntime) Authentication() runtimes.Authentication { return extensionAuth{} }

type extensionAuth struct{}

func (extensionAuth) Validate(mode kyberv1.AgentAuthType, input runtimes.AuthInput) error {
	if mode != kyberv1.AgentAuthTypeAPIKey || input["fixtureKey"] == "" {
		return fmt.Errorf("fixture key required")
	}
	return nil
}
func (extensionAuth) Prepare(_ context.Context, _ kyberv1.AgentAuthType, input runtimes.AuthInput, _ runtimes.AuthOptions) ([]runtimes.Credential, error) {
	return []runtimes.Credential{{Suffix: "fixture", Data: map[string][]byte{"token": []byte(input["fixtureKey"])}}}, nil
}
func (extensionAuth) Pending(kyberv1.AgentAuthType, map[string][]byte) bool { return false }

// This fixture tests the public registration/credential boundary, not boot or
// native CLI conformance. It deliberately has no optional features.
func TestNewRuntimeDiscoveryAndCredentialCreation(t *testing.T) {
	if _, ok := runtimes.Get("extension-fixture"); !ok {
		runtimes.Register(extensionRuntime{})
	}
	client := fake.NewClientBuilder().WithScheme(mustNewScheme(t)).WithObjects(defaultMachine()).Build()
	server := &api.Server{K8sClient: client, APIKey: testAPIKey, Namespace: "kyber-system", ValidRuntimes: map[string]bool{"extension-fixture": true}, RuntimeImages: map[string]string{"extension-fixture": "test.invalid/fixture:pinned"}}
	handler := server.BuildHandler()
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, authedRequest(t, "GET", "/api/v1/config", nil))
	var config api.ConfigResponse
	if rr.Code != 200 || json.Unmarshal(rr.Body.Bytes(), &config) != nil || len(config.Runtimes) != 1 || config.Runtimes[0].ID != "extension-fixture" {
		t.Fatalf("discovery status=%d body=%s", rr.Code, rr.Body.String())
	}
	body := `{"name":"new-harness","machine":"worker-1","runtime":"extension-fixture","secrets":{"authType":"api-key","runtimeAuth":{"fixtureKey":"fixture-only-value"}}}`
	rr = httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	handler.ServeHTTP(rr, req)
	if rr.Code != 201 {
		t.Fatalf("create status=%d body=%s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "fixture-only-value") {
		t.Fatal("credential echoed in response")
	}
	secret := &corev1.Secret{}
	if err := client.Get(context.Background(), types.NamespacedName{Namespace: "kyber-system", Name: "new-harness-fixture"}, secret); err != nil {
		t.Fatal(err)
	}
	if string(secret.Data["token"]) != "fixture-only-value" {
		t.Fatal("strategy credential was not persisted")
	}
}

type extensionAdapter struct{ runtimes.Adapter }

func (extensionAdapter) Type() string { return "extension-fixture" }

type extensionProbe struct{}

func (extensionProbe) Type() string { return "extension-fixture" }

func TestGenericCredentialInputPreservesPKCEValidation(t *testing.T) {
	for _, input := range []string{`{"oauthCode":"test"}`, `{"oauthCode":"test","pkceVerifier":"verifier"}`} {
		handler, _ := buildAgentHandler(t)
		body := `{"name":"pkce-fixture","machine":"worker-1","runtime":"claude-code","secrets":{"authType":"oauth","runtimeAuth":` + input + `}}`
		req := httptest.NewRequest("POST", "/api/v1/agents", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+testAPIKey)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		if rr.Code != 400 || !strings.Contains(rr.Body.String(), "pkce") {
			t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
		}
	}
}

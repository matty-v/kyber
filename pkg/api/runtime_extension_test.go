package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/matty-v/kyber/pkg/api"
	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/runtimes"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type extensionRuntime struct{}

func (extensionRuntime) Type() string { return "extension-fixture" }
func (extensionRuntime) Adapter() runtimes.Adapter {
	probe := &corev1.Probe{ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"true"}}}}
	return extensionAdapter{runtimes.NewStubAdapter("test.invalid/fixture:pinned", []string{"/fixture/start"}, nil, nil, probe, probe, 30, "/persist/brief.json", "/persist/state.json", "")}
}
func (extensionRuntime) Probe() runtimes.Probe { return extensionProbe{} }
func (extensionRuntime) Descriptor() runtimes.Descriptor {
	return runtimes.Descriptor{
		ID: "extension-fixture", Name: "Extension", ContractVersion: runtimes.ContractVersion,
		Profile: runtimes.InteractiveProfile, Cancellation: "notify_only",
		Features:  []runtimes.Feature{runtimes.JobTurnHooks, runtimes.TaskReceipts, runtimes.TaskTools},
		AuthModes: []runtimes.AuthMode{{ID: kyberv1.AgentAuthTypeAPIKey, Name: "Fixture key", Flow: "api-key", InputField: "fixtureKey", SecretSuffix: "fixture"}},
	}
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

func registerExtensionRuntime() {
	if _, ok := runtimes.Get("extension-fixture"); !ok {
		runtimes.Register(extensionRuntime{})
	}
}

// This fixture tests the public registration/credential boundary, not boot or
// native CLI conformance. It declares selected task/job features but deliberately
// omits model selection.
func TestNewRuntimeDiscoveryAndCredentialCreation(t *testing.T) {
	registerExtensionRuntime()
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

	for _, request := range []struct {
		name, method, path, body string
		want                     int
	}{
		{name: "catalog", method: http.MethodGet, path: "/api/v1/agents/new-harness/models", want: http.StatusNotImplemented},
		{name: "set model", method: http.MethodPost, path: "/api/v1/agents/new-harness/set-model", body: `{"model":"fixture-model"}`, want: http.StatusNotImplemented},
		{name: "patch model", method: http.MethodPatch, path: "/api/v1/agents/new-harness", body: `{"model":"fixture-model"}`, want: http.StatusBadRequest},
	} {
		t.Run(request.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			req := httptest.NewRequest(request.method, request.path, strings.NewReader(request.body))
			req.Header.Set("Authorization", "Bearer "+testAPIKey)
			handler.ServeHTTP(rr, req)
			if rr.Code != request.want || !strings.Contains(rr.Body.String(), "does not declare model-catalog support") {
				t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
			}
		})
	}

	rr = httptest.NewRecorder()
	explicitModel := `{"name":"model-fixture","machine":"worker-1","runtime":"extension-fixture","model":"fixture-model","secrets":{"authType":"api-key","runtimeAuth":{"fixtureKey":"fixture-only-value"}}}`
	req = httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(explicitModel))
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "does not declare model-catalog support") {
		t.Fatalf("explicit-model create status=%d body=%s", rr.Code, rr.Body.String())
	}

	agent := &kyberv1.Agent{}
	if err := client.Get(context.Background(), types.NamespacedName{Namespace: "kyber-system", Name: "new-harness"}, agent); err != nil {
		t.Fatal(err)
	}
	agent.Status.Phase = kyberv1.AgentPhaseRunning
	agent.Status.PodName = "agent-new-harness"
	agent.Status.Runtime.InstalledVersion = "fixture"
	agent.Status.Runtime.Capabilities = &kyberv1.RuntimeCapabilitiesObservation{
		ContractVersion: runtimes.ContractVersion, InstalledVersion: "fixture", PodUID: "fixture-pod",
		ObservedAt: metav1.Now(), Features: map[string]bool{
			string(runtimes.JobTurnHooks): true, string(runtimes.TaskReceipts): true, string(runtimes.TaskTools): true,
		},
	}
	if err := client.Update(context.Background(), agent); err != nil {
		t.Fatal(err)
	}
	if err := client.Create(context.Background(), &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: agent.Status.PodName, Namespace: agent.Namespace, UID: "fixture-pod"}}); err != nil {
		t.Fatal(err)
	}
	for _, feature := range []runtimes.Feature{runtimes.JobTurnHooks, runtimes.TaskReceipts, runtimes.TaskTools} {
		if got := runtimes.AvailabilityFor(agent, feature, time.Now()); got.State != "available" {
			t.Fatalf("%s availability = %+v", feature, got)
		}
	}
	if got := runtimes.AvailabilityFor(agent, runtimes.ModelCatalog, time.Now()); got.Supported || got.Reason != "unsupported" {
		t.Fatalf("model catalog availability = %+v", got)
	}
	if err := server.WaitAgentCapabilities(context.Background(), agent.Name, 0, runtimes.TaskReceipts, runtimes.TaskTools); err != nil {
		t.Fatalf("task dispatch gate inherited model requirement: %v", err)
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

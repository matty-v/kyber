package api_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/matty-v/kyber/pkg/api"
	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/capabilities"
)

func capabilityAgent(t *testing.T) *kyberv1.Agent {
	t.Helper()
	declaration := &kyberv1.AgentPublicCapabilities{
		SchemaVersion: "v1alpha1",
		Identity:      kyberv1.AgentPublicCapabilityIdentity{DisplayName: "Reviewer", Description: "Reviews bounded changes."},
		Capabilities:  []kyberv1.AgentPublicCapability{{ID: "code-review", Version: "1", Name: "Review code", Description: "Produces findings.", InputModes: []string{"text/plain"}, OutputModes: []string{"application/json"}, Evidence: &kyberv1.AgentPublicCapabilityEvidence{RequiredSkills: []string{"private-review-skill"}}}},
	}
	_, digest, err := capabilities.NormalizeAndValidate(declaration)
	if err != nil {
		t.Fatal(err)
	}
	now := metav1.NewTime(time.Date(2026, 9, 1, 22, 0, 0, 0, time.UTC))
	return &kyberv1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "reviewer", Namespace: "kyber-system", UID: types.UID("agent-resource-1"), Generation: 3},
		Spec:       kyberv1.AgentSpec{PublicCapabilities: declaration},
		Status: kyberv1.AgentStatus{PublicCapabilities: &kyberv1.AgentPublicCapabilitiesStatus{
			ObservedGeneration: 3, ManifestRevision: digest, ObservedAt: &now,
			Conditions:   []metav1.Condition{{Type: "Valid", Status: metav1.ConditionTrue, Reason: "Validated", Message: "valid"}},
			Capabilities: []kyberv1.AgentPublicCapabilityAvailability{{ID: "code-review", Availability: "available"}},
		}},
	}
}

func capabilityHandler(t *testing.T) http.Handler {
	t.Helper()
	agent := capabilityAgent(t)
	server := &api.Server{
		K8sClient: fake.NewClientBuilder().WithScheme(mustNewScheme(t)).WithObjects(agent).Build(),
		APIKey:    testAPIKey, Namespace: "kyber-system",
		Callers: []api.ScopedCaller{
			{Name: "reader", PrincipalID: "principal_reader", TenantID: "tenant_test", CredentialID: "credential_reader", CredentialGeneration: 1, AgentResources: []string{"kyber-system/reviewer"}, Key: "capability-reader", Scopes: []string{"capabilities:read"}},
			{Name: "wrong-resource", PrincipalID: "principal_wrong", TenantID: "tenant_test", CredentialID: "credential_wrong", CredentialGeneration: 1, AgentResources: []string{"kyber-system/other"}, Key: "wrong-resource", Scopes: []string{"capabilities:read"}},
			{Name: "no-scope", Key: "no-scope", Scopes: []string{"requests:read"}},
			{Name: "writer", PrincipalID: "principal_writer", TenantID: "tenant_test", CredentialID: "credential_writer", CredentialGeneration: 1, AgentResources: []string{"kyber-system/reviewer"}, Key: "capability-writer", Scopes: []string{"capabilities:write"}},
		},
	}
	return server.BuildHandler()
}

func TestAgentCapabilitiesWriteRequiresScopeAndSupportsUnpublish(t *testing.T) {
	h := capabilityHandler(t)
	request := func(key, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPatch, "/api/v1/agents/reviewer", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr
	}
	if got := request("no-scope", `{"publicCapabilities":null}`); got.Code != http.StatusForbidden {
		t.Fatalf("no scope=%d %s", got.Code, got.Body.String())
	}
	if got := request("capability-writer", `{"publicCapabilities":null,"model":"smuggled-model"}`); got.Code != http.StatusForbidden {
		t.Fatalf("mixed lifecycle mutation=%d %s", got.Code, got.Body.String())
	}
	if got := request("no-scope", `{"a2aPeers":[{"name":"auditor","url":"https://agents.example/a2a","credential":{"existingSecret":"auditor-token","key":"token"}}]}`); got.Code != http.StatusForbidden {
		t.Fatalf("A2A peer mutation without lifecycle scope=%d %s", got.Code, got.Body.String())
	}
	if got := request("capability-writer", `{"publicCapabilities":null}`); got.Code != http.StatusOK {
		t.Fatalf("writer=%d %s", got.Code, got.Body.String())
	}
}

func capabilityRequest(h http.Handler, key, etag string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/reviewer/capabilities", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func TestAgentCapabilitiesProjectionAuthorizationAndETag(t *testing.T) {
	h := capabilityHandler(t)
	allowed := capabilityRequest(h, "capability-reader", "")
	if allowed.Code != http.StatusOK {
		t.Fatalf("allowed=%d %s", allowed.Code, allowed.Body.String())
	}
	for _, private := range []string{"evidence", "private-review-skill", "runtimeAdapters", "podIP", "model"} {
		if strings.Contains(allowed.Body.String(), private) {
			t.Fatalf("projection leaked %q: %s", private, allowed.Body.String())
		}
	}
	if etag := allowed.Header().Get("ETag"); etag == "" || capabilityRequest(h, "capability-reader", etag).Code != http.StatusNotModified {
		t.Fatalf("ETag did not revalidate: %q", etag)
	}
	if got := capabilityRequest(h, "wrong-resource", ""); got.Code != http.StatusNotFound {
		t.Fatalf("wrong resource=%d %s", got.Code, got.Body.String())
	}
	if got := capabilityRequest(h, "no-scope", ""); got.Code != http.StatusForbidden {
		t.Fatalf("no scope=%d %s", got.Code, got.Body.String())
	}
}

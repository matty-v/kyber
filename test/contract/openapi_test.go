//go:build integration

// Package contract contains API contract tests that validate the Kyber public API's
// request/response shapes against the OpenAPI schema in openapi.yaml.
//
// Tests use the httptest package with a fake k8s client (no real cluster needed)
// and validate each request/response against the schema using getkin/kin-openapi.
//
// Run with:
//
//	go test -tags integration ./test/contract/...
package contract

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/gorillamux"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/matty-v/kyber/pkg/api"
	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/requeststore"
)

const (
	contractAPIKey    = "contract-test-key"
	contractNS        = "kyber-system"
	openapiSchemaFile = "openapi.yaml"
)

var (
	openAPIDoc    *openapi3.T
	openAPIRouter routers.Router
)

func TestMain(m *testing.M) {
	loader := openapi3.NewLoader()
	var err error
	openAPIDoc, err = loader.LoadFromFile(openapiSchemaFile)
	if err != nil {
		panic("loading openapi.yaml: " + err.Error())
	}
	// Disable server validation so httptest requests (any host) pass routing.
	openAPIDoc.Servers = openapi3.Servers{
		{URL: "/"},
	}
	if err := openAPIDoc.Validate(loader.Context); err != nil {
		panic("validating openapi.yaml: " + err.Error())
	}

	openAPIRouter, err = gorillamux.NewRouter(openAPIDoc)
	if err != nil {
		panic("building openapi router: " + err.Error())
	}

	os.Exit(m.Run())
}

// newContractScheme builds a runtime.Scheme with CRD types.
func newContractScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := kyberv1.AddToScheme(s); err != nil {
		t.Fatalf("AddToScheme kyberv1: %v", err)
	}
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("AddToScheme corev1: %v", err)
	}
	return s
}

// newContractServer builds a test API server backed by a fake k8s client.
func newContractServer(t *testing.T, objs ...runtime.Object) http.Handler {
	t.Helper()
	scheme := newContractScheme(t)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...).Build()
	requests, err := requeststore.NewMemoryStore(requeststore.DefaultLimits())
	if err != nil {
		t.Fatalf("creating request store: %v", err)
	}
	srv := &api.Server{
		K8sClient:    fakeClient,
		APIKey:       contractAPIKey,
		Namespace:    contractNS,
		RequestStore: requests,
	}
	return srv.BuildHandler()
}

// authedReq builds an HTTP request with the contract test API key.
func authedReq(t *testing.T, method, path string, body interface{}) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encoding request body: %v", err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Authorization", "Bearer "+contractAPIKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req
}

// validateContract sends req to h, then validates both the request and response
// against the OpenAPI schema. Use this for happy-path requests that should match
// the schema. Returns the recorder so callers can inspect the response body.
func validateContract(t *testing.T, h http.Handler, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	return validateContractOptions(t, h, req, false)
}

// validateContractResponseOnly sends req to h and validates only the response
// against the OpenAPI schema. Use this for intentionally invalid requests where
// the test is checking the server's error response shape (e.g., 400 validation errors).
func validateContractResponseOnly(t *testing.T, h http.Handler, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	return validateContractOptions(t, h, req, true)
}

// validateContractOptions is the implementation shared by validateContract and
// validateContractResponseOnly. When skipReqValidation is true, request validation
// is skipped (used for intentional-error scenarios).
func validateContractOptions(t *testing.T, h http.Handler, req *http.Request, skipReqValidation bool) *httptest.ResponseRecorder {
	t.Helper()

	// Clone the body so we can read it multiple times.
	var reqBodyBytes []byte
	if req.Body != nil {
		var err error
		reqBodyBytes, err = io.ReadAll(req.Body)
		if err != nil {
			t.Fatalf("reading request body for validation: %v", err)
		}
		req.Body = io.NopCloser(bytes.NewReader(reqBodyBytes))
	}

	// Find the matching route.
	route, pathParams, err := openAPIRouter.FindRoute(req)
	if err != nil {
		t.Fatalf("FindRoute(%s %s): %v", req.Method, req.URL.Path, err)
	}

	noopOpts := &openapi3filter.Options{
		AuthenticationFunc: openapi3filter.NoopAuthenticationFunc,
	}

	// Build the request validation input (needed for response validation even if
	// we skip request validation).
	if reqBodyBytes != nil {
		req.Body = io.NopCloser(bytes.NewReader(reqBodyBytes))
	}
	reqInput := &openapi3filter.RequestValidationInput{
		Request:    req,
		PathParams: pathParams,
		Route:      route,
		Options:    noopOpts,
	}

	if !skipReqValidation {
		if err := openapi3filter.ValidateRequest(context.Background(), reqInput); err != nil {
			t.Errorf("request validation failed for %s %s: %v", req.Method, req.URL.Path, err)
		}
	}

	// Reset body for serving.
	if reqBodyBytes != nil {
		req.Body = io.NopCloser(bytes.NewReader(reqBodyBytes))
	}

	// Serve the request.
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	// Validate the response.
	respBody := rr.Body.Bytes()
	respInput := &openapi3filter.ResponseValidationInput{
		RequestValidationInput: reqInput,
		Status:                 rr.Code,
		Header:                 rr.Result().Header,
		Body:                   io.NopCloser(bytes.NewReader(respBody)),
		Options:                noopOpts,
	}
	if err := openapi3filter.ValidateResponse(context.Background(), respInput); err != nil {
		t.Errorf("response validation failed for %s %s (status %d): %v", req.Method, req.URL.Path, rr.Code, err)
	}

	return rr
}

// sampleMachineObj returns a Machine CRD for tests. Capacity matches the
// n2-standard-4 GCE capacity lookup so agent-creation capacity checks pass
// for reasonable pod requests.
func sampleMachineObj(name string) *kyberv1.Machine {
	return &kyberv1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: contractNS},
		Spec: kyberv1.MachineSpec{
			Provider:    kyberv1.MachineProviderGCE,
			MachineType: "n2-standard-4",
			DiskSizeGb:  100,
			Spot:        false,
			Zone:        "us-central1-a",
			Capacity: kyberv1.MachineCapacity{
				CPU:    resource.MustParse("4"),
				Memory: resource.MustParse("16Gi"),
			},
			DesiredPhase: kyberv1.MachinePhaseRunning,
		},
	}
}

// sampleAgentObj returns an Agent CRD for tests.
func sampleAgentObj(name string) *kyberv1.Agent {
	return &kyberv1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: contractNS},
		Spec: kyberv1.AgentSpec{
			Machine: "worker-1",
			Runtime: "claude-code",
			Model:   "claude-sonnet-4",
			Resources: kyberv1.AgentResources{
				CPU:    resource.MustParse("1"),
				Memory: resource.MustParse("2Gi"),
				Disk:   resource.MustParse("50Gi"),
			},
			Secrets:      kyberv1.AgentSecrets{AuthType: kyberv1.AgentAuthTypeOAuth},
			DesiredPhase: kyberv1.AgentPhaseRunning,
		},
	}
}

// ---------------------------------------------------------------------------
// Fleet
// ---------------------------------------------------------------------------

func TestContract_FleetSummary(t *testing.T) {
	h := newContractServer(t, sampleMachineObj("worker-1"), sampleAgentObj("dave"))
	req := authedReq(t, http.MethodGet, "/api/v1/fleet/summary", nil)
	rr := validateContract(t, h, req)
	if rr.Code != http.StatusOK {
		t.Errorf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Machines
// ---------------------------------------------------------------------------

func TestContract_ListMachines(t *testing.T) {
	h := newContractServer(t, sampleMachineObj("worker-1"))
	req := authedReq(t, http.MethodGet, "/api/v1/machines", nil)
	rr := validateContract(t, h, req)
	if rr.Code != http.StatusOK {
		t.Errorf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestContract_CreateMachine(t *testing.T) {
	h := newContractServer(t)
	req := authedReq(t, http.MethodPost, "/api/v1/machines", map[string]interface{}{
		"name":        "worker-new",
		"machineType": "n2-standard-4",
		"diskSizeGb":  100,
		"spot":        false,
		"zone":        "us-central1-a",
		"provider":    "gce",
	})
	rr := validateContract(t, h, req)
	if rr.Code != http.StatusCreated {
		t.Errorf("want 201, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestContract_CreateMachine_ValidationError(t *testing.T) {
	h := newContractServer(t)
	// Missing required fields — server returns 400. Request itself is intentionally
	// schema-invalid, so we only validate the error response shape.
	req := authedReq(t, http.MethodPost, "/api/v1/machines", map[string]interface{}{
		"name": "bad",
		// machineType, zone, diskSizeGb missing
	})
	rr := validateContractResponseOnly(t, h, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestContract_GetMachine(t *testing.T) {
	h := newContractServer(t, sampleMachineObj("worker-1"))
	req := authedReq(t, http.MethodGet, "/api/v1/machines/worker-1", nil)
	rr := validateContract(t, h, req)
	if rr.Code != http.StatusOK {
		t.Errorf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestContract_GetMachine_NotFound(t *testing.T) {
	h := newContractServer(t)
	req := authedReq(t, http.MethodGet, "/api/v1/machines/ghost", nil)
	rr := validateContract(t, h, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("want 404, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestContract_DeleteMachine(t *testing.T) {
	h := newContractServer(t, sampleMachineObj("worker-1"))
	req := authedReq(t, http.MethodDelete, "/api/v1/machines/worker-1?confirm=worker-1", nil)
	rr := validateContract(t, h, req)
	if rr.Code != http.StatusNoContent {
		t.Errorf("want 204, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestContract_DeleteMachine_NotFound(t *testing.T) {
	h := newContractServer(t)
	req := authedReq(t, http.MethodDelete, "/api/v1/machines/ghost?confirm=ghost", nil)
	rr := validateContract(t, h, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("want 404, got %d: %s", rr.Code, rr.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Agents
// ---------------------------------------------------------------------------

func TestContract_ListAgents(t *testing.T) {
	h := newContractServer(t, sampleAgentObj("dave"), sampleAgentObj("chewie"))
	req := authedReq(t, http.MethodGet, "/api/v1/agents", nil)
	rr := validateContract(t, h, req)
	if rr.Code != http.StatusOK {
		t.Errorf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestContract_CreateAgent(t *testing.T) {
	// Pre-seed Machine CR "worker-1" so the machine-existence validation in
	// POST /api/v1/agents passes against the in-memory fake k8s client.
	h := newContractServer(t, sampleMachineObj("worker-1"))
	req := authedReq(t, http.MethodPost, "/api/v1/agents", map[string]interface{}{
		"name":    "new-agent",
		"machine": "worker-1",
		"runtime": "claude-code",
		"model":   "claude-sonnet-4",
		"resources": map[string]interface{}{
			"cpu":    "1",
			"memory": "2Gi",
			"disk":   "50Gi",
		},
		"secrets": map[string]interface{}{
			"authType": "oauth",
		},
	})
	rr := validateContract(t, h, req)
	if rr.Code != http.StatusCreated {
		t.Errorf("want 201, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestContract_CreateAgent_ValidationError(t *testing.T) {
	h := newContractServer(t)
	// Missing required machine, runtime, model. Request itself is intentionally
	// schema-invalid, so we only validate the error response shape.
	req := authedReq(t, http.MethodPost, "/api/v1/agents", map[string]interface{}{
		"name": "broken-agent",
	})
	rr := validateContractResponseOnly(t, h, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestContract_GetAgent(t *testing.T) {
	h := newContractServer(t, sampleAgentObj("dave"))
	req := authedReq(t, http.MethodGet, "/api/v1/agents/dave", nil)
	rr := validateContract(t, h, req)
	if rr.Code != http.StatusOK {
		t.Errorf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestContract_GetAgent_NotFound(t *testing.T) {
	h := newContractServer(t)
	req := authedReq(t, http.MethodGet, "/api/v1/agents/ghost", nil)
	rr := validateContract(t, h, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("want 404, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestContract_SubmitAndGetAgentRequest(t *testing.T) {
	h := newContractServer(t, sampleAgentObj("kiosk"))
	created := validateContract(t, h, authedReq(t, http.MethodPost,
		"/api/v1/agents/kiosk/requests", map[string]string{
			"prompt":      "Describe Kyber",
			"correlation": "contract-test",
		}))
	if created.Code != http.StatusAccepted {
		t.Fatalf("POST request status = %d, body = %s", created.Code, created.Body.String())
	}
	var response struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &response); err != nil || response.ID == "" {
		t.Fatalf("decoding POST response: id=%q err=%v", response.ID, err)
	}

	read := validateContract(t, h, authedReq(t, http.MethodGet,
		"/api/v1/agents/kiosk/requests/"+response.ID, nil))
	if read.Code != http.StatusOK {
		t.Fatalf("GET request status = %d, body = %s", read.Code, read.Body.String())
	}
}

func TestContract_DeleteAgent(t *testing.T) {
	h := newContractServer(t, sampleAgentObj("dave"))
	req := authedReq(t, http.MethodDelete, "/api/v1/agents/dave?confirm=dave", nil)
	rr := validateContract(t, h, req)
	if rr.Code != http.StatusNoContent {
		t.Errorf("want 204, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestContract_DeleteAgent_NotFound(t *testing.T) {
	h := newContractServer(t)
	req := authedReq(t, http.MethodDelete, "/api/v1/agents/ghost?confirm=ghost", nil)
	rr := validateContract(t, h, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("want 404, got %d: %s", rr.Code, rr.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Auth (401 responses are also in the schema)
// ---------------------------------------------------------------------------

func TestContract_Unauthorized_NoKey(t *testing.T) {
	h := newContractServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents", nil)
	// No Authorization header.

	// We skip OpenAPI request validation here because the request itself is
	// intentionally missing the security header — we only validate the response shape.
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("want 401, got %d", rr.Code)
	}

	// Validate that the 401 response body matches the ErrorResponse schema.
	var errResp api.ErrorResponse
	if err := json.NewDecoder(rr.Body).Decode(&errResp); err != nil {
		t.Fatalf("decoding 401 body: %v", err)
	}
	if errResp.Error.Code == "" {
		t.Error("401 ErrorResponse.error.code must not be empty")
	}
}

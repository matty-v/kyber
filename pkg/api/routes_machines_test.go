package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apiresource "k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/matty-v/kyber/pkg/api"
	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/messagebuffer"
)

const testAPIKey = "test-key-xyz"

// buildMachineHandler creates a test HTTP handler backed by a fake client.
func buildMachineHandler(t *testing.T, scheme *runtime.Scheme, objs ...runtime.Object) http.Handler {
	t.Helper()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...).Build()
	s := &api.Server{
		K8sClient:        fakeClient,
		MessageBuffer:    messagebuffer.NewMemoryBuffer(),
		APIKey:           testAPIKey,
		Namespace:        "kyber-system",
		GCEVMTypeCatalog: api.DefaultGCEVMTypeCatalog(),
	}
	return s.BuildHandler()
}

func authedRequest(t *testing.T, method, target string, body interface{}) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encoding request body: %v", err)
		}
	}
	req := httptest.NewRequest(method, target, &buf)
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	req.Header.Set("Content-Type", "application/json")
	return req
}

// TestMachines_Create_HappyPath verifies that POST /api/v1/machines creates the machine.
func TestMachines_Create_HappyPath(t *testing.T) {
	h := buildMachineHandler(t, mustNewScheme(t))
	req := authedRequest(t, http.MethodPost, "/api/v1/machines", map[string]interface{}{
		"name":        "worker-1",
		"provider":    "gce",
		"machineType": "n1-standard-4",
		"diskSizeGb":  50,
		"spot":        true,
		"zone":        "us-central1-a",
	})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Errorf("want 201, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp api.MachineResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp.ID != "worker-1" {
		t.Errorf("ID: got %q, want %q", resp.ID, "worker-1")
	}
	if resp.Spec.MachineType != "n1-standard-4" {
		t.Errorf("MachineType: got %q", resp.Spec.MachineType)
	}
}

// TestMachines_Create_ValidationError verifies 400 for missing required fields.
func TestMachines_Create_ValidationError(t *testing.T) {
	h := buildMachineHandler(t, mustNewScheme(t))

	cases := []struct {
		name string
		body map[string]interface{}
	}{
		{"missing name", map[string]interface{}{"provider": "gce", "machineType": "n2-standard-4", "zone": "us-central1-a", "diskSizeGb": 10}},
		{"missing machineType", map[string]interface{}{"name": "w1", "provider": "gce", "zone": "us-central1-a", "diskSizeGb": 10}},
		{"missing zone", map[string]interface{}{"name": "w1", "provider": "gce", "machineType": "n2-standard-4", "diskSizeGb": 10}},
		{"diskSizeGb too small", map[string]interface{}{"name": "w1", "provider": "gce", "machineType": "n2-standard-4", "zone": "us-central1-a", "diskSizeGb": 5}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := authedRequest(t, http.MethodPost, "/api/v1/machines", tc.body)
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			if rr.Code != http.StatusBadRequest {
				t.Errorf("want 400, got %d: %s", rr.Code, rr.Body.String())
			}
		})
	}
}

// TestMachines_Create_Conflict verifies 409 when machine already exists.
func TestMachines_Create_Conflict(t *testing.T) {
	existing := &kyberv1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "worker-1", Namespace: "kyber-system"},
		Spec: kyberv1.MachineSpec{
			Provider: kyberv1.MachineProviderGCE, MachineType: "n1-standard-4", Zone: "us-central1-a", DiskSizeGb: 50,
		},
	}
	h := buildMachineHandler(t, mustNewScheme(t), existing)
	req := authedRequest(t, http.MethodPost, "/api/v1/machines", map[string]interface{}{
		"name": "worker-1", "provider": "gce", "machineType": "n1-standard-4", "zone": "us-central1-a", "diskSizeGb": 50,
	})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusConflict {
		t.Errorf("want 409, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestMachines_List verifies GET /api/v1/machines returns a list.
func TestMachines_List(t *testing.T) {
	m1 := &kyberv1.Machine{ObjectMeta: metav1.ObjectMeta{Name: "m1", Namespace: "kyber-system"}, Spec: kyberv1.MachineSpec{Provider: kyberv1.MachineProviderGCE, MachineType: "n2", Zone: "us-central1-a", DiskSizeGb: 10}}
	m2 := &kyberv1.Machine{ObjectMeta: metav1.ObjectMeta{Name: "m2", Namespace: "kyber-system"}, Spec: kyberv1.MachineSpec{Provider: kyberv1.MachineProviderGCE, MachineType: "n2", Zone: "us-central1-b", DiskSizeGb: 10}}
	h := buildMachineHandler(t, mustNewScheme(t), m1, m2)
	req := authedRequest(t, http.MethodGet, "/api/v1/machines", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp api.MachineListResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if len(resp.Items) != 2 {
		t.Errorf("want 2 items, got %d", len(resp.Items))
	}
}

// TestMachines_List_StableOrdering pins the response order to ascending ID
// so consumers see a stable order across refetches as the fleet grows (#263).
func TestMachines_List_StableOrdering(t *testing.T) {
	mk := func(name string) *kyberv1.Machine {
		return &kyberv1.Machine{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "kyber-system"},
			Spec:       kyberv1.MachineSpec{Provider: kyberv1.MachineProviderGCE, MachineType: "n2", Zone: "us-central1-a", DiskSizeGb: 10},
		}
	}
	h := buildMachineHandler(t, mustNewScheme(t), mk("zeta"), mk("alpha"), mk("mu"))
	req := authedRequest(t, http.MethodGet, "/api/v1/machines", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp api.MachineListResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	got := make([]string, len(resp.Items))
	for i, m := range resp.Items {
		got[i] = m.ID
	}
	want := []string{"alpha", "mu", "zeta"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("listMachines order = %v, want %v", got, want)
	}
}

// TestMachines_Get_Found verifies GET /api/v1/machines/{name} returns the machine.
func TestMachines_Get_Found(t *testing.T) {
	m := &kyberv1.Machine{ObjectMeta: metav1.ObjectMeta{Name: "worker-1", Namespace: "kyber-system"}, Spec: kyberv1.MachineSpec{Provider: kyberv1.MachineProviderGCE, MachineType: "n2-standard-4", Zone: "us-central1-a", DiskSizeGb: 50}}
	h := buildMachineHandler(t, mustNewScheme(t), m)
	req := authedRequest(t, http.MethodGet, "/api/v1/machines/worker-1", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestMachines_Get_NotFound verifies GET /api/v1/machines/{name} returns 404.
func TestMachines_Get_NotFound(t *testing.T) {
	h := buildMachineHandler(t, mustNewScheme(t))
	req := authedRequest(t, http.MethodGet, "/api/v1/machines/ghost", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Errorf("want 404, got %d", rr.Code)
	}
}

// TestMachines_Delete verifies DELETE /api/v1/machines/{name} returns 204.
func TestMachines_Delete(t *testing.T) {
	// kyber#565: machine DELETE now requires ?confirm=<name>, same gate as agents.
	m := &kyberv1.Machine{ObjectMeta: metav1.ObjectMeta{Name: "worker-1", Namespace: "kyber-system"}, Spec: kyberv1.MachineSpec{Provider: kyberv1.MachineProviderGCE, MachineType: "n2-standard-4", Zone: "us-central1-a", DiskSizeGb: 50}}
	h := buildMachineHandler(t, mustNewScheme(t), m)
	req := authedRequest(t, http.MethodDelete, "/api/v1/machines/worker-1?confirm=worker-1", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Errorf("want 204, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestMachines_Delete_NotFound verifies DELETE /api/v1/machines/{name} returns 404
// (with a matching ?confirm so it passes the gate and reaches the lookup).
func TestMachines_Delete_NotFound(t *testing.T) {
	h := buildMachineHandler(t, mustNewScheme(t))
	req := authedRequest(t, http.MethodDelete, "/api/v1/machines/ghost?confirm=ghost", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Errorf("want 404, got %d", rr.Code)
	}
}

// TestMachines_Delete_Unconfirmed_Rejected covers AC-3 (confirmation half): a
// machine DELETE without a matching ?confirm is rejected 400 and NOT deleted.
func TestMachines_Delete_Unconfirmed_Rejected(t *testing.T) {
	cases := []struct {
		name   string
		target string
	}{
		{"missing confirm", "/api/v1/machines/worker-1"},
		{"mismatched confirm", "/api/v1/machines/worker-1?confirm=worker-2"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := &kyberv1.Machine{ObjectMeta: metav1.ObjectMeta{Name: "worker-1", Namespace: "kyber-system"}, Spec: kyberv1.MachineSpec{Provider: kyberv1.MachineProviderGCE, MachineType: "n2-standard-4", Zone: "us-central1-a", DiskSizeGb: 50}}
			h := buildMachineHandler(t, mustNewScheme(t), m)
			req := authedRequest(t, http.MethodDelete, c.target, nil)
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("want 400, got %d: %s", rr.Code, rr.Body.String())
			}
			if !strings.Contains(rr.Body.String(), "confirmation_required") {
				t.Errorf("want confirmation_required error code, got %s", rr.Body.String())
			}
		})
	}
}

// TestMachines_Delete_Unauthorized_Rejected covers AC-3 (authz half): with
// enforcement on, an under-scoped caller is rejected 403 even with ?confirm.
func TestMachines_Delete_Unauthorized_Rejected(t *testing.T) {
	m := &kyberv1.Machine{ObjectMeta: metav1.ObjectMeta{Name: "worker-1", Namespace: "kyber-system"}, Spec: kyberv1.MachineSpec{Provider: kyberv1.MachineProviderGCE, MachineType: "n2-standard-4", Zone: "us-central1-a", DiskSizeGb: 50}}
	h := buildScopedMachineHandler(t, mustNewScheme(t), m)
	req := scopedRequest(http.MethodDelete, "/api/v1/machines/worker-1?confirm=worker-1", writeScopedKey)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Errorf("want 403, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestMachines_Start verifies POST /api/v1/machines/{name}/start sets desiredPhase.
func TestMachines_Start(t *testing.T) {
	m := &kyberv1.Machine{ObjectMeta: metav1.ObjectMeta{Name: "worker-1", Namespace: "kyber-system"}, Spec: kyberv1.MachineSpec{Provider: kyberv1.MachineProviderGCE, MachineType: "n2-standard-4", Zone: "us-central1-a", DiskSizeGb: 50}}
	h := buildMachineHandler(t, mustNewScheme(t), m)
	req := authedRequest(t, http.MethodPost, "/api/v1/machines/worker-1/start", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestMachines_Stop verifies POST /api/v1/machines/{name}/stop sets desiredPhase.
func TestMachines_Stop(t *testing.T) {
	m := &kyberv1.Machine{ObjectMeta: metav1.ObjectMeta{Name: "worker-1", Namespace: "kyber-system"}, Spec: kyberv1.MachineSpec{Provider: kyberv1.MachineProviderGCE, MachineType: "n2-standard-4", Zone: "us-central1-a", DiskSizeGb: 50}}
	h := buildMachineHandler(t, mustNewScheme(t), m)
	req := authedRequest(t, http.MethodPost, "/api/v1/machines/worker-1/stop", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestMachines_Auth verifies 401 is returned without the API key.
func TestMachines_Auth(t *testing.T) {
	h := buildMachineHandler(t, mustNewScheme(t))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/machines", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("want 401, got %d", rr.Code)
	}
}

// TestDeleteMachine_WithAgents_Returns422 verifies that deleting a machine with attached
// agents returns 422 Unprocessable Entity instead of succeeding.
func TestDeleteMachine_WithAgents_Returns422(t *testing.T) {
	machine := &kyberv1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "worker-1", Namespace: "kyber-system"},
		Spec: kyberv1.MachineSpec{
			Provider: kyberv1.MachineProviderGCE, MachineType: "n2-standard-4",
			Zone: "us-central1-a", DiskSizeGb: 50,
		},
	}
	agent := &kyberv1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "dave", Namespace: "kyber-system"},
		Spec: kyberv1.AgentSpec{
			Machine: "worker-1",
			Runtime: "claude-code",
			Model:   "claude-opus-4",
		},
	}
	h := buildMachineHandler(t, mustNewScheme(t), machine, agent)
	// ?confirm passes the gate so the request reaches the attached-agents guard.
	req := authedRequest(t, http.MethodDelete, "/api/v1/machines/worker-1?confirm=worker-1", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnprocessableEntity {
		t.Errorf("want 422, got %d: %s", rr.Code, rr.Body.String())
	}
}

// buildScopedMachineHandler builds a machine handler with enforcement ON and a
// write-only scoped caller, so the kyber#565 machine-DELETE authz gate can be
// exercised end-to-end.
func buildScopedMachineHandler(t *testing.T, scheme *runtime.Scheme, objs ...runtime.Object) http.Handler {
	t.Helper()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...).Build()
	s := &api.Server{
		K8sClient:        fakeClient,
		MessageBuffer:    messagebuffer.NewMemoryBuffer(),
		APIKey:           testAPIKey,
		AuthzEnforce:     true,
		Callers:          []api.ScopedCaller{{Name: "write-caller", Key: writeScopedKey, Scopes: []string{"lifecycle:write"}}},
		Namespace:        "kyber-system",
		GCEVMTypeCatalog: api.DefaultGCEVMTypeCatalog(),
	}
	return s.BuildHandler()
}

// mustNewScheme returns a scheme with both kyberv1 and corev1 registered.
func mustNewScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := kyberv1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme kyberv1: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme corev1: %v", err)
	}
	return scheme
}

// TestMachineToResponse_WithAllocatable verifies that machineToResponse includes
// allocatable when Machine.status.allocatable is populated, and omits it when nil.
func TestMachineToResponse_WithAllocatable(t *testing.T) {
	t.Run("allocatable populated", func(t *testing.T) {
		m := &kyberv1.Machine{
			ObjectMeta: metav1.ObjectMeta{Name: "worker-1", Namespace: "kyber-system"},
			Spec:       kyberv1.MachineSpec{Provider: kyberv1.MachineProviderGCE, MachineType: "n2-standard-4", Zone: "us-central1-a", DiskSizeGb: 50},
			Status: kyberv1.MachineStatus{
				Phase:    kyberv1.MachinePhaseReady,
				NodeName: "node-worker-1",
				ObservedCapacity: &kyberv1.MachineCapacity{
					CPU:    mustParseQuantity(t, "1750m"),
					Memory: mustParseQuantity(t, "7300Mi"),
				},
			},
		}
		h := buildMachineHandler(t, mustNewScheme(t), m)
		req := authedRequest(t, http.MethodGet, "/api/v1/machines/worker-1", nil)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
		}

		// Decode into a generic map so we can check the allocatable fields precisely.
		var body map[string]interface{}
		if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
			t.Fatalf("decoding response: %v", err)
		}
		status, ok := body["status"].(map[string]interface{})
		if !ok {
			t.Fatalf("status field missing or wrong type: %v", body["status"])
		}
		alloc, ok := status["allocatable"].(map[string]interface{})
		if !ok {
			t.Fatalf("status.allocatable missing or wrong type: %v", status["allocatable"])
		}
		if alloc["cpu"] != "1750m" {
			t.Errorf("allocatable.cpu: got %q, want %q", alloc["cpu"], "1750m")
		}
		if alloc["memory"] != "7300Mi" {
			t.Errorf("allocatable.memory: got %q, want %q", alloc["memory"], "7300Mi")
		}
	})

	t.Run("allocatable nil → omitted from response", func(t *testing.T) {
		m := &kyberv1.Machine{
			ObjectMeta: metav1.ObjectMeta{Name: "worker-2", Namespace: "kyber-system"},
			Spec:       kyberv1.MachineSpec{Provider: kyberv1.MachineProviderGCE, MachineType: "n2-standard-4", Zone: "us-central1-a", DiskSizeGb: 50},
			Status: kyberv1.MachineStatus{
				Phase: kyberv1.MachinePhaseProvisioning,
			},
		}
		h := buildMachineHandler(t, mustNewScheme(t), m)
		req := authedRequest(t, http.MethodGet, "/api/v1/machines/worker-2", nil)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
		}

		var body map[string]interface{}
		if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
			t.Fatalf("decoding response: %v", err)
		}
		status, ok := body["status"].(map[string]interface{})
		if !ok {
			t.Fatalf("status field missing or wrong type: %v", body["status"])
		}
		if _, present := status["allocatable"]; present {
			t.Errorf("expected allocatable to be absent when nil, but it was present: %v", status["allocatable"])
		}
	})
}

// mustParseQuantity parses a resource.Quantity string, fataling on error.
func mustParseQuantity(t *testing.T, s string) apiresource.Quantity {
	t.Helper()
	q, err := apiresource.ParseQuantity(s)
	if err != nil {
		t.Fatalf("parsing quantity %q: %v", s, err)
	}
	return q
}

// TestCreateMachine_GCE_StampsCapacity verifies that POST /api/v1/machines with
// provider=gce stamps Spec.Capacity from the machine type lookup.
func TestCreateMachine_GCE_StampsCapacity(t *testing.T) {
	h := buildMachineHandler(t, mustNewScheme(t))
	req := authedRequest(t, http.MethodPost, "/api/v1/machines", map[string]interface{}{
		"name":        "gce-worker",
		"provider":    "gce",
		"machineType": "n1-standard-4",
		"diskSizeGb":  50,
		"zone":        "us-central1-a",
	})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", rr.Code, rr.Body.String())
	}

	// Verify the response includes the stamped capacity.
	var body map[string]interface{}
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	spec, ok := body["spec"].(map[string]interface{})
	if !ok {
		t.Fatalf("spec field missing or wrong type: %v", body["spec"])
	}
	capacity, ok := spec["capacity"].(map[string]interface{})
	if !ok {
		t.Fatalf("spec.capacity missing or wrong type: %v", spec["capacity"])
	}
	if capacity["cpu"] != "4" {
		t.Errorf("spec.capacity.cpu: got %q, want %q", capacity["cpu"], "4")
	}
	if capacity["memory"] != "15Gi" {
		t.Errorf("spec.capacity.memory: got %q, want %q", capacity["memory"], "15Gi")
	}
}

func TestCreateMachine_NewProviderKinds(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		body     map[string]interface{}
	}{
		{
			name:     "fake uses managed inputs",
			provider: "fake",
			body: map[string]interface{}{
				"name": "fake-worker", "provider": "fake", "machineType": "e2-small",
				"diskSizeGb": 20, "spot": true, "zone": "local-a",
			},
		},
		{
			name:     "static uses declared capacity",
			provider: "static",
			body: map[string]interface{}{
				"name": "static-worker", "provider": "static",
				"capacity": map[string]interface{}{"cpu": "4", "memory": "8Gi"},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fakeClient := fake.NewClientBuilder().WithScheme(mustNewScheme(t)).Build()
			s := &api.Server{
				K8sClient:       fakeClient,
				MessageBuffer:   messagebuffer.NewMemoryBuffer(),
				APIKey:          testAPIKey,
				Namespace:       "kyber-system",
				ComputeProvider: tc.provider,
			}
			req := authedRequest(t, http.MethodPost, "/api/v1/machines", tc.body)
			rr := httptest.NewRecorder()
			s.BuildHandler().ServeHTTP(rr, req)
			if rr.Code != http.StatusCreated {
				t.Fatalf("status = %d, want 201: %s", rr.Code, rr.Body.String())
			}
		})
	}
}

func TestCreateMachineRejectsProviderMismatchedToInstall(t *testing.T) {
	fakeClient := fake.NewClientBuilder().WithScheme(mustNewScheme(t)).Build()
	s := &api.Server{
		K8sClient:       fakeClient,
		MessageBuffer:   messagebuffer.NewMemoryBuffer(),
		APIKey:          testAPIKey,
		Namespace:       "kyber-system",
		ComputeProvider: "fake",
	}
	req := authedRequest(t, http.MethodPost, "/api/v1/machines", map[string]interface{}{
		"name": "wrong-provider", "provider": "gce", "machineType": "e2-small",
		"diskSizeGb": 20, "zone": "local-a",
	})
	rr := httptest.NewRecorder()
	s.BuildHandler().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rr.Code, rr.Body.String())
	}
}

// TestCreateMachine_Mock_RejectsGCEFields verifies that POST /api/v1/machines with
// provider=mock and GCE-only fields (e.g. machineType) returns 400.
func TestCreateMachine_Mock_RejectsGCEFields(t *testing.T) {
	h := buildMachineHandler(t, mustNewScheme(t))
	req := authedRequest(t, http.MethodPost, "/api/v1/machines", map[string]interface{}{
		"name":        "mock-worker",
		"provider":    "mock",
		"machineType": "n2-standard-4",
		"capacity":    map[string]interface{}{"cpu": "4", "memory": "16Gi"},
	})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "machineType") {
		t.Errorf("error body should mention the offending field machineType; got %s",
			rr.Body.String())
	}
}

// TestCreateMachine_GCE_RejectsCapacity verifies that POST /api/v1/machines with
// provider=gce and a capacity field returns 400 — capacity is derived from machineType,
// not user-supplied.
func TestCreateMachine_GCE_RejectsCapacity(t *testing.T) {
	h := buildMachineHandler(t, mustNewScheme(t))
	req := authedRequest(t, http.MethodPost, "/api/v1/machines", map[string]interface{}{
		"name":        "worker-1",
		"provider":    "gce",
		"machineType": "n2-standard-4",
		"diskSizeGb":  50,
		"zone":        "us-central1-a",
		"capacity":    map[string]interface{}{"cpu": "8", "memory": "32Gi"},
	})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "capacity") {
		t.Errorf("error should mention capacity; got %s", rr.Body.String())
	}
}

// TestCreateMachine_Mock_RejectsSecondMachine verifies that a second provider=mock
// machine returns 409 Conflict.
func TestCreateMachine_Mock_RejectsSecondMachine(t *testing.T) {
	h := buildMachineHandler(t, mustNewScheme(t))

	// First mock machine — should succeed.
	req1 := authedRequest(t, http.MethodPost, "/api/v1/machines", map[string]interface{}{
		"name":     "mock-worker-1",
		"provider": "mock",
		"capacity": map[string]interface{}{"cpu": "4", "memory": "16Gi"},
	})
	rr1 := httptest.NewRecorder()
	h.ServeHTTP(rr1, req1)
	if rr1.Code != http.StatusCreated {
		t.Fatalf("first mock machine: want 201, got %d: %s", rr1.Code, rr1.Body.String())
	}

	// Second mock machine — should be rejected.
	req2 := authedRequest(t, http.MethodPost, "/api/v1/machines", map[string]interface{}{
		"name":     "mock-worker-2",
		"provider": "mock",
		"capacity": map[string]interface{}{"cpu": "2", "memory": "8Gi"},
	})
	rr2 := httptest.NewRecorder()
	h.ServeHTTP(rr2, req2)
	if rr2.Code != http.StatusConflict {
		t.Fatalf("second mock machine: want 409, got %d: %s", rr2.Code, rr2.Body.String())
	}
	body := rr2.Body.String()
	if !strings.Contains(body, "one Machine") {
		t.Errorf("want error body to contain %q, got: %s", "one Machine", body)
	}
}

// TestCreateMachine_GCE_CustomCatalog verifies that a Server with a custom
// GCEVMTypeCatalog only accepts types present in that catalog.
func TestCreateMachine_GCE_CustomCatalog(t *testing.T) {
	customCatalog, err := api.ParseVMTypeCatalog(`[{"type":"e2-medium","cpu":"2","memory":"4Gi"}]`)
	if err != nil {
		t.Fatalf("ParseVMTypeCatalog: %v", err)
	}
	fakeClient := fake.NewClientBuilder().WithScheme(mustNewScheme(t)).Build()
	s := &api.Server{
		K8sClient:        fakeClient,
		MessageBuffer:    messagebuffer.NewMemoryBuffer(),
		APIKey:           testAPIKey,
		Namespace:        "kyber-system",
		GCEVMTypeCatalog: customCatalog,
	}
	h := s.BuildHandler()

	// e2-medium is in the custom catalog — should succeed.
	req := authedRequest(t, http.MethodPost, "/api/v1/machines", map[string]interface{}{
		"name": "worker-1", "provider": "gce", "machineType": "e2-medium",
		"diskSizeGb": 20, "zone": "us-central1-a",
	})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Errorf("e2-medium (in catalog): want 201, got %d: %s", rr.Code, rr.Body.String())
	}

	// n1-standard-4 is in the default catalog but NOT in this custom catalog — should fail.
	req2 := authedRequest(t, http.MethodPost, "/api/v1/machines", map[string]interface{}{
		"name": "worker-2", "provider": "gce", "machineType": "n1-standard-4",
		"diskSizeGb": 20, "zone": "us-central1-a",
	})
	rr2 := httptest.NewRecorder()
	h.ServeHTTP(rr2, req2)
	if rr2.Code != http.StatusBadRequest {
		t.Errorf("n1-standard-4 (not in custom catalog): want 400, got %d: %s", rr2.Code, rr2.Body.String())
	}
}

// TestCreateMachine_GCE_UnknownMachineType verifies that an unknown machineType returns
// 400 and includes the list of valid types in the error response.
func TestCreateMachine_GCE_UnknownMachineType(t *testing.T) {
	h := buildMachineHandler(t, mustNewScheme(t))
	req := authedRequest(t, http.MethodPost, "/api/v1/machines", map[string]interface{}{
		"name":        "worker-1",
		"provider":    "gce",
		"machineType": "xyz-massive-999",
		"diskSizeGb":  50,
		"zone":        "us-central1-a",
	})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "xyz-massive-999") {
		t.Errorf("error body should echo the invalid type; got: %s", body)
	}
	if !strings.Contains(body, "valid types") {
		t.Errorf("error body should list valid types; got: %s", body)
	}
}

// TestMachineToResponse_EmitsNewCapacityFields verifies that Status.ObservedCapacity /
// Status.AssignableCapacity / Status.AvailableCapacity (the three fields shipped by
// #140) are emitted on the wire so the PWA's capacity-aware UI can read them
// directly. The legacy `allocatable` field is also still emitted for backward compat.
func TestMachineToResponse_EmitsNewCapacityFields(t *testing.T) {
	m := &kyberv1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "razer", Namespace: "kyber-system"},
		Spec: kyberv1.MachineSpec{
			Provider: kyberv1.MachineProviderMock,
			Capacity: kyberv1.MachineCapacity{CPU: apiresource.MustParse("4"), Memory: apiresource.MustParse("8Gi")},
		},
		Status: kyberv1.MachineStatus{
			Phase:              kyberv1.MachinePhaseRunning,
			ObservedCapacity:   &kyberv1.MachineCapacity{CPU: apiresource.MustParse("4"), Memory: apiresource.MustParse("8Gi")},
			AssignableCapacity: &kyberv1.MachineCapacity{CPU: apiresource.MustParse("3"), Memory: apiresource.MustParse("7Gi")},
			AvailableCapacity:  &kyberv1.MachineCapacity{CPU: apiresource.MustParse("2"), Memory: apiresource.MustParse("5Gi")},
		},
	}

	resp := api.MachineToResponseForTest(m)
	if resp.Status.ObservedCapacity == nil {
		t.Fatal("ObservedCapacity is nil; expected populated")
	}
	if resp.Status.ObservedCapacity.CPU != "4" || resp.Status.ObservedCapacity.Memory != "8Gi" {
		t.Errorf("ObservedCapacity: got {%s, %s}, want {4, 8Gi}",
			resp.Status.ObservedCapacity.CPU, resp.Status.ObservedCapacity.Memory)
	}
	if resp.Status.AssignableCapacity == nil {
		t.Fatal("AssignableCapacity is nil")
	}
	if resp.Status.AssignableCapacity.CPU != "3" || resp.Status.AssignableCapacity.Memory != "7Gi" {
		t.Errorf("AssignableCapacity: got {%s, %s}, want {3, 7Gi}",
			resp.Status.AssignableCapacity.CPU, resp.Status.AssignableCapacity.Memory)
	}
	if resp.Status.AvailableCapacity == nil {
		t.Fatal("AvailableCapacity is nil")
	}
	if resp.Status.AvailableCapacity.CPU != "2" || resp.Status.AvailableCapacity.Memory != "5Gi" {
		t.Errorf("AvailableCapacity: got {%s, %s}, want {2, 5Gi}",
			resp.Status.AvailableCapacity.CPU, resp.Status.AvailableCapacity.Memory)
	}
	// Backward-compat: legacy `allocatable` field still emitted, mirrors ObservedCapacity.
	if resp.Status.Allocatable == nil || resp.Status.Allocatable.CPU != "4" {
		t.Errorf("legacy allocatable shape must still be emitted; got %+v", resp.Status.Allocatable)
	}
}

// TestMachineToResponse_EmitsEphemeralStorage covers #129 PR-C — ephemeral-storage
// must round-trip through machineToResponse on all three capacity tiers AND on
// spec.capacity. Regression guard: the field landed on the CRD type but we forgot
// to thread it through the wire DTO; PR-C self-review caught it.
func TestMachineToResponse_EmitsEphemeralStorage(t *testing.T) {
	m := &kyberv1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "razer", Namespace: "kyber-system"},
		Spec: kyberv1.MachineSpec{
			Provider: kyberv1.MachineProviderMock,
			Capacity: kyberv1.MachineCapacity{
				CPU:              apiresource.MustParse("4"),
				Memory:           apiresource.MustParse("8Gi"),
				EphemeralStorage: apiresource.MustParse("100Gi"),
			},
		},
		Status: kyberv1.MachineStatus{
			Phase: kyberv1.MachinePhaseRunning,
			ObservedCapacity: &kyberv1.MachineCapacity{
				CPU: apiresource.MustParse("4"), Memory: apiresource.MustParse("8Gi"),
				EphemeralStorage: apiresource.MustParse("200Gi"),
			},
			AssignableCapacity: &kyberv1.MachineCapacity{
				CPU: apiresource.MustParse("3"), Memory: apiresource.MustParse("7Gi"),
				EphemeralStorage: apiresource.MustParse("190Gi"),
			},
			AvailableCapacity: &kyberv1.MachineCapacity{
				CPU: apiresource.MustParse("2"), Memory: apiresource.MustParse("5Gi"),
				EphemeralStorage: apiresource.MustParse("140Gi"),
			},
		},
	}

	resp := api.MachineToResponseForTest(m)
	if got := resp.Status.ObservedCapacity.EphemeralStorage; got != "200Gi" {
		t.Errorf("ObservedCapacity.EphemeralStorage: got %q, want %q", got, "200Gi")
	}
	if got := resp.Status.AssignableCapacity.EphemeralStorage; got != "190Gi" {
		t.Errorf("AssignableCapacity.EphemeralStorage: got %q, want %q", got, "190Gi")
	}
	if got := resp.Status.AvailableCapacity.EphemeralStorage; got != "140Gi" {
		t.Errorf("AvailableCapacity.EphemeralStorage: got %q, want %q", got, "140Gi")
	}
	if resp.Spec.Capacity == nil || resp.Spec.Capacity.EphemeralStorage != "100Gi" {
		t.Errorf("Spec.Capacity.EphemeralStorage: got %+v, want 100Gi", resp.Spec.Capacity)
	}
	// Legacy `allocatable` mirror must also include the new field.
	if resp.Status.Allocatable == nil || resp.Status.Allocatable.EphemeralStorage != "200Gi" {
		t.Errorf("legacy Allocatable.EphemeralStorage: got %+v, want 200Gi", resp.Status.Allocatable)
	}
}

// TestMachineToResponse_OmitsZeroEphemeralStorage covers the wire-level fix that
// resource.Quantity{}.String() emits "0" — naive serialization would leak a
// truthy "0" onto the wire and the PWA's truthy-gate (asn.ephemeralStorage)
// would mis-render a Disk row at 0/0. quantityOrEmpty in machineToResponse
// returns "" for zero quantities, which omitempty drops.
func TestMachineToResponse_OmitsZeroEphemeralStorage(t *testing.T) {
	m := &kyberv1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "legacy", Namespace: "kyber-system"},
		Spec: kyberv1.MachineSpec{
			Provider: kyberv1.MachineProviderMock,
			Capacity: kyberv1.MachineCapacity{CPU: apiresource.MustParse("4"), Memory: apiresource.MustParse("8Gi")},
		},
		Status: kyberv1.MachineStatus{
			Phase: kyberv1.MachinePhaseRunning,
			// EphemeralStorage left as zero value — pre-PR-C node.
			ObservedCapacity:   &kyberv1.MachineCapacity{CPU: apiresource.MustParse("4"), Memory: apiresource.MustParse("8Gi")},
			AssignableCapacity: &kyberv1.MachineCapacity{CPU: apiresource.MustParse("3"), Memory: apiresource.MustParse("7Gi")},
			AvailableCapacity:  &kyberv1.MachineCapacity{CPU: apiresource.MustParse("2"), Memory: apiresource.MustParse("5Gi")},
		},
	}
	resp := api.MachineToResponseForTest(m)
	if resp.Status.ObservedCapacity.EphemeralStorage != "" {
		t.Errorf("zero EphemeralStorage must serialise as empty string, got %q",
			resp.Status.ObservedCapacity.EphemeralStorage)
	}
	if resp.Status.AssignableCapacity.EphemeralStorage != "" {
		t.Errorf("zero AssignableCapacity.EphemeralStorage must serialise as empty, got %q",
			resp.Status.AssignableCapacity.EphemeralStorage)
	}
	if resp.Status.AvailableCapacity.EphemeralStorage != "" {
		t.Errorf("zero AvailableCapacity.EphemeralStorage must serialise as empty, got %q",
			resp.Status.AvailableCapacity.EphemeralStorage)
	}
}

// TestMachines_Create_Mock_AcceptsEphemeralStorage covers the request-side
// wire fix for #129 PR-C. The PWA's mock create form sends
// `capacity.ephemeralStorage`; the API must accept it and write it through to
// spec.Capacity.EphemeralStorage on the new Machine CR.
func TestMachines_Create_Mock_AcceptsEphemeralStorage(t *testing.T) {
	scheme := mustNewScheme(t)
	h := buildMachineHandler(t, scheme)

	req := authedRequest(t, http.MethodPost, "/api/v1/machines", map[string]interface{}{
		"name":     "local",
		"provider": "mock",
		"capacity": map[string]interface{}{
			"cpu":              "4",
			"memory":           "16Gi",
			"ephemeralStorage": "100Gi",
		},
	})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", rr.Code, rr.Body.String())
	}

	// Read it back and verify spec.capacity.ephemeralStorage round-trips.
	getReq := authedRequest(t, http.MethodGet, "/api/v1/machines/local", nil)
	getRR := httptest.NewRecorder()
	h.ServeHTTP(getRR, getReq)
	if getRR.Code != http.StatusOK {
		t.Fatalf("get want 200, got %d: %s", getRR.Code, getRR.Body.String())
	}
	var resp api.MachineResponse
	if err := json.NewDecoder(getRR.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp.Spec.Capacity == nil || resp.Spec.Capacity.EphemeralStorage != "100Gi" {
		t.Errorf("spec.capacity.ephemeralStorage: got %+v, want 100Gi", resp.Spec.Capacity)
	}
}

// TestMachines_Create_Mock_AutoFillsCapacityFromNode covers kyber#240 — when
// the mock create body omits `capacity`, the API auto-fills it from the
// cluster's first Ready node's allocatable. The operator should never have to
// type CPU/memory/disk on standalone.
func TestMachines_Create_Mock_AutoFillsCapacityFromNode(t *testing.T) {
	scheme := mustNewScheme(t)
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-node"},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:              apiresource.MustParse("8"),
				corev1.ResourceMemory:           apiresource.MustParse("32Gi"),
				corev1.ResourceEphemeralStorage: apiresource.MustParse("400Gi"),
			},
			Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
			},
		},
	}
	h := buildMachineHandler(t, scheme, node)

	req := authedRequest(t, http.MethodPost, "/api/v1/machines", map[string]interface{}{
		"name":     "local",
		"provider": "mock",
		// no capacity — exercise the auto-fill path
	})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", rr.Code, rr.Body.String())
	}

	getReq := authedRequest(t, http.MethodGet, "/api/v1/machines/local", nil)
	getRR := httptest.NewRecorder()
	h.ServeHTTP(getRR, getReq)
	if getRR.Code != http.StatusOK {
		t.Fatalf("get want 200, got %d: %s", getRR.Code, getRR.Body.String())
	}
	var resp api.MachineResponse
	if err := json.NewDecoder(getRR.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp.Spec.Capacity == nil {
		t.Fatal("spec.capacity nil; auto-fill didn't run")
	}
	if resp.Spec.Capacity.CPU != "8" {
		t.Errorf("auto-filled CPU: got %q, want %q", resp.Spec.Capacity.CPU, "8")
	}
	if resp.Spec.Capacity.Memory != "32Gi" {
		t.Errorf("auto-filled Memory: got %q, want %q", resp.Spec.Capacity.Memory, "32Gi")
	}
	if resp.Spec.Capacity.EphemeralStorage != "400Gi" {
		t.Errorf("auto-filled EphemeralStorage: got %q, want %q", resp.Spec.Capacity.EphemeralStorage, "400Gi")
	}
}

// TestMachines_Create_Mock_AutoFill_NoNodes covers the no-node failure mode —
// auto-fill needs at least one Ready node to do its job.
func TestMachines_Create_Mock_AutoFill_NoNodes(t *testing.T) {
	scheme := mustNewScheme(t)
	h := buildMachineHandler(t, scheme) // no Node fixtures

	req := authedRequest(t, http.MethodPost, "/api/v1/machines", map[string]interface{}{
		"name":     "local",
		"provider": "mock",
	})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "no nodes") {
		t.Errorf("error body should explain the failure; got: %s", rr.Body.String())
	}
}

// TestMachines_Create_Mock_RejectsInvalidEphemeralStorage covers the parse
// error path — bad quantity strings should bounce with VALIDATION_ERROR
// rather than land in the CR as a malformed value.
func TestMachines_Create_Mock_RejectsInvalidEphemeralStorage(t *testing.T) {
	scheme := mustNewScheme(t)
	h := buildMachineHandler(t, scheme)

	req := authedRequest(t, http.MethodPost, "/api/v1/machines", map[string]interface{}{
		"name":     "local",
		"provider": "mock",
		"capacity": map[string]interface{}{
			"cpu":              "4",
			"memory":           "16Gi",
			"ephemeralStorage": "not-a-quantity",
		},
	})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "capacity.ephemeralStorage") {
		t.Errorf("error body should name the bad field; got: %s", rr.Body.String())
	}
}

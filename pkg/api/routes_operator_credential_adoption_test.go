package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

type adoptionManagerFixture struct {
	namespace    string
	calls        int
	beforeReturn func()
}

func (f *adoptionManagerFixture) Namespace() string { return f.namespace }
func (f *adoptionManagerFixture) Prepare(context.Context, Caller) (OperatorCredentialAdoptionPlan, error) {
	f.calls++
	if f.beforeReturn != nil {
		f.beforeReturn()
	}
	return OperatorCredentialAdoptionPlan{Version: strings.Repeat("a", 64), KeyCount: 42}, nil
}
func (f *adoptionManagerFixture) Start(ctx context.Context, r OperatorCredentialAdoptionStart, _ Caller, _ string, authorize func(context.Context) error) (OperatorCredentialAdoptionOperation, error) {
	f.calls++
	if err := authorize(ctx); err != nil {
		return OperatorCredentialAdoptionOperation{}, err
	}
	return OperatorCredentialAdoptionOperation{OperationID: r.OperationID, Status: "pending"}, nil
}
func (f *adoptionManagerFixture) Outcome(context.Context, string) (OperatorCredentialAdoptionOperation, error) {
	f.calls++
	return OperatorCredentialAdoptionOperation{OperationID: uuid.NewString(), Status: "pending"}, nil
}
func (f *adoptionManagerFixture) Pending(context.Context) ([]OperatorCredentialAdoptionOperation, error) {
	f.calls++
	return []OperatorCredentialAdoptionOperation{}, nil
}
func (f *adoptionManagerFixture) Recover(ctx context.Context, r OperatorCredentialAdoptionRecovery, _ Caller, _ string, authorize func(context.Context) error) (OperatorCredentialAdoptionOperation, error) {
	f.calls++
	if err := authorize(ctx); err != nil {
		return OperatorCredentialAdoptionOperation{}, err
	}
	return OperatorCredentialAdoptionOperation{OperationID: r.OperationID, Status: "pending"}, nil
}
func adoptionHTTP(handler http.Handler, key, method, path, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+key)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}
func TestAdoptionRoutesProtectEveryAdministratorSurface(t *testing.T) {
	manager := &adoptionManagerFixture{namespace: "kyber-system"}
	server := &Server{Namespace: "kyber-system", APIKey: "root-test", Callers: []ScopedCaller{{Name: "Restricted", Key: "key-test", Scopes: []string{"lifecycle:admin", "metrics:read"}, AgentResources: []string{"kyber-system/alpha"}}}, OperatorCredentialAdoption: manager}
	handler := server.BuildHandler()
	id := uuid.NewString()
	requests := []struct{ method, path, body string }{
		{"GET", "/api/v1/operator-credentials/adoption", ""},
		{"POST", "/api/v1/operator-credentials/adoption", `{"operationId":"` + id + `","version":"` + strings.Repeat("a", 64) + `","confirmOwnership":true}`},
		{"GET", "/api/v1/operator-credential-adoptions?operationId=" + id, ""},
		{"GET", "/api/v1/operator-credential-adoptions/pending", ""},
		{"POST", "/api/v1/operator-credential-adoptions/recover", `{"operationId":"` + id + `","attemptId":"` + uuid.NewString() + `"}`},
	}
	for _, request := range requests {
		response := adoptionHTTP(handler, "key-test", request.method, request.path, request.body)
		if response.Code != 403 || manager.calls != 0 {
			t.Fatal("restricted caller reached adoption manager", request.path, response.Code)
		}
	}
	for _, request := range requests {
		response := adoptionHTTP(handler, "root-test", request.method, request.path, request.body)
		if response.Code != 200 && response.Code != 202 {
			t.Fatal("administrator request failed", request.path, response.Code)
		}
		if response.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("protected response cacheable")
		}
	}
	if manager.calls != len(requests) {
		t.Fatal("unexpected manager calls")
	}
}
func TestAdoptionRoutesRejectMalformedBodiesBeforeBackend(t *testing.T) {
	manager := &adoptionManagerFixture{namespace: "kyber-system"}
	server := &Server{Namespace: "kyber-system", APIKey: "root-test", OperatorCredentialAdoption: manager}
	handler := server.BuildHandler()
	for _, body := range []string{`{}`, `{"operationId":"x","operationId":"y","version":"v","confirmOwnership":true}`, `{"operationId":"x","version":"v","confirmOwnership":true,"key":"forbidden"}`, `[]`, `{"operationId":"x","version":"v","confirmOwnership":true}{}`} {
		response := adoptionHTTP(handler, "root-test", "POST", "/api/v1/operator-credentials/adoption", body)
		if response.Code != 400 || manager.calls != 0 {
			t.Fatal("invalid setup reached backend", response.Code)
		}
	}
}
func TestAdoptionRoutesClearResultWhenCurrentAccessChanges(t *testing.T) {
	manager := &adoptionManagerFixture{namespace: "kyber-system"}
	server := &Server{Namespace: "kyber-system", APIKey: "root-test", OperatorCredentialAdoption: manager}
	handler := server.BuildHandler()
	manager.beforeReturn = func() { server.auth.SetKey("other-test") }
	response := adoptionHTTP(handler, "root-test", "GET", "/api/v1/operator-credentials/adoption", "")
	if response.Code != 403 || strings.Contains(response.Body.String(), `"keyCount"`) || strings.Contains(response.Body.String(), `"version"`) {
		t.Fatal("stale privileged plan returned after access changed", response.Code)
	}
}
func TestAdoptionRoutesFailClosedWhenUnavailableOrMisbound(t *testing.T) {
	for _, manager := range []operatorCredentialAdoptionManager{nil, &adoptionManagerFixture{namespace: "other-installation"}} {
		server := &Server{Namespace: "kyber-system", APIKey: "root-test", OperatorCredentialAdoption: manager}
		response := adoptionHTTP(server.BuildHandler(), "root-test", "GET", "/api/v1/operator-credentials/adoption", "")
		if response.Code != 503 {
			t.Fatal("unsupported/misbound adoption looked available", response.Code)
		}
		var body map[string]any
		if json.Unmarshal(response.Body.Bytes(), &body) != nil || response.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("unsafe unavailable response")
		}
	}
}

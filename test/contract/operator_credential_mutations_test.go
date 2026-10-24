//go:build integration

package contract

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/matty-v/kyber/internal/testutil"
	"github.com/matty-v/kyber/pkg/api"
	"github.com/matty-v/kyber/pkg/operatorcredentials"
	"github.com/matty-v/kyber/pkg/securityaudit"
)

type contractCredentialAuthority struct {
	mu       sync.Mutex
	document operatorcredentials.Document
	version  string
}

func (a *contractCredentialAuthority) Load(context.Context) (operatorcredentials.Snapshot, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return operatorcredentials.Snapshot{UID: "contract-registry", Version: a.version, Document: a.document.Clone()}, nil
}
func (a *contractCredentialAuthority) Replace(_ context.Context, uid, version string, next operatorcredentials.Document) (operatorcredentials.Snapshot, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if uid != "contract-registry" || version != a.version {
		return operatorcredentials.Snapshot{}, operatorcredentials.ErrConflict
	}
	a.document = next.Clone()
	a.version = "2"
	return operatorcredentials.Snapshot{UID: uid, Version: a.version, Document: a.document.Clone()}, nil
}
func (a *contractCredentialAuthority) Fence(context.Context, string, string) (operatorcredentials.Snapshot, error) {
	return operatorcredentials.Snapshot{}, operatorcredentials.ErrUnavailable
}

type contractCredentialJournal struct {
	*testutil.SecurityAudit
	mu        sync.Mutex
	operation *securityaudit.CredentialOperation
}

func (j *contractCredentialJournal) GetCredentialOperation(_ context.Context, namespace, id string) (securityaudit.CredentialOperation, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.operation == nil || j.operation.Intent.Admission.Namespace != namespace || j.operation.Intent.Admission.OperationID != id {
		return securityaudit.CredentialOperation{}, securityaudit.ErrOperationNotFound
	}
	result := *j.operation
	result.NewlyAdmitted = false
	return result, nil
}
func (j *contractCredentialJournal) AdmitCredentialOperation(ctx context.Context, intent securityaudit.CredentialIntent) (securityaudit.CredentialOperation, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if _, err := j.Append(ctx, intent.Admission); err != nil {
		return securityaudit.CredentialOperation{}, err
	}
	j.operation = &securityaudit.CredentialOperation{Intent: intent}
	result := *j.operation
	result.NewlyAdmitted = true
	return result, nil
}
func (j *contractCredentialJournal) PendingCredentialOperations(context.Context, string, string, int) ([]securityaudit.CredentialOperation, error) {
	return nil, securityaudit.ErrUnavailable
}
func (j *contractCredentialJournal) FinishCredentialOperation(ctx context.Context, namespace, id, outcome string, at time.Time) (securityaudit.Event, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.operation == nil {
		return securityaudit.Event{}, securityaudit.ErrOperationNotFound
	}
	event := j.operation.Intent.Admission
	event.ID = j.operation.Intent.CompletionEventID
	event.Phase = "completion"
	event.Outcome = outcome
	event.ObservedAt = at
	if _, err := j.Append(ctx, event); err != nil {
		return securityaudit.Event{}, err
	}
	j.operation.Completion = &event
	return event, nil
}

func TestContract_OperatorCredentialManagedChangeAndOutcome(t *testing.T) {
	authority := &contractCredentialAuthority{version: "1", document: operatorcredentials.Document{SchemaVersion: 1, Namespace: contractNS, Credentials: []operatorcredentials.Credential{(operatorcredentials.Credential{ID: "contract-admin", PrincipalID: "contract-principal", TenantID: "contract-tenant", Name: "Contract administrator", Role: operatorcredentials.RoleAdministrator, State: operatorcredentials.StateActive, Generation: 1, PermissionRevision: 1, Source: operatorcredentials.Source{Kind: "managed"}}).WithSecretMaterial([]byte(contractAPIKey))}}}
	journal := &contractCredentialJournal{SecurityAudit: testutil.NewSecurityAudit()}
	server := &api.Server{Namespace: contractNS, APIKey: contractAPIKey, OperatorCredentialAuthority: authority, SecurityAudit: journal}
	handler := server.BuildHandler()
	inventory := validateContract(t, handler, authedReq(t, http.MethodGet, "/api/v1/operator-credentials", nil))
	var current struct {
		Version             string `json:"version"`
		ManagementAvailable bool   `json:"managementAvailable"`
	}
	if inventory.Code != http.StatusOK || json.Unmarshal(inventory.Body.Bytes(), &current) != nil || current.Version == "" || !current.ManagementAvailable {
		t.Fatal("managed inventory metadata missing")
	}
	operationID := uuid.NewString()
	key := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{'x'}, 32))
	payload, err := json.Marshal(map[string]any{"operationId": operationID, "action": "create", "key": key, "policy": map[string]any{"name": "Contract integration", "role": "restricted", "scopes": []string{"tasks:read"}, "agentResources": []string{}, "machineResources": []string{}}})
	if err != nil {
		t.Fatal("fixture serialization failed")
	}
	request := authedReq(t, http.MethodPost, "/api/v1/operator-credentials/mutations", json.RawMessage(payload))
	request.Header.Set("If-Match", current.Version)
	result := validateContract(t, handler, request)
	if result.Code != http.StatusOK || strings.Contains(result.Body.String(), key) {
		t.Fatal("managed change failed or leaked material")
	}
	status := validateContract(t, handler, authedReq(t, http.MethodGet, "/api/v1/operator-credential-operations?operationId="+operationID, nil))
	if status.Code != http.StatusOK || strings.Contains(status.Body.String(), key) {
		t.Fatal("operation lookup failed or leaked material")
	}
	recovered := validateContract(t, handler, authedReq(t, http.MethodPost, "/api/v1/operator-credential-operations/reconcile", map[string]string{"operationId": operationID}))
	if recovered.Code != http.StatusOK || strings.Contains(recovered.Body.String(), key) {
		t.Fatal("recovery response failed or leaked material")
	}
	stale := authedReq(t, http.MethodPost, "/api/v1/operator-credentials/mutations", json.RawMessage(payload))
	stale.Header.Set("If-Match", current.Version)
	if response := validateContract(t, handler, stale); response.Code != http.StatusConflict {
		t.Fatal("stale review accepted")
	}
	missing := validateContract(t, handler, authedReq(t, http.MethodGet, "/api/v1/operator-credential-operations?operationId="+uuid.NewString(), nil))
	if missing.Code != http.StatusNotFound {
		t.Fatal("missing operation was invented")
	}
}

func TestContract_OperatorCredentialManagementUnavailable(t *testing.T) {
	server := &api.Server{Namespace: contractNS, APIKey: contractAPIKey}
	payload := `{"operationId":"` + uuid.NewString() + `","action":"revoke","credentialId":"example"}`
	request := authedReq(t, http.MethodPost, "/api/v1/operator-credentials/mutations", json.RawMessage(payload))
	request.Header.Set("If-Match", `"reviewed-version"`)
	if response := validateContract(t, server.BuildHandler(), request); response.Code != http.StatusServiceUnavailable {
		t.Fatal("unconfigured management reported available")
	}
}

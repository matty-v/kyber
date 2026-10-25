package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/matty-v/kyber/internal/testutil"
	"github.com/matty-v/kyber/pkg/operatorcredentials"
	"github.com/matty-v/kyber/pkg/securityaudit"
)

type maintenanceRecoveryJournalFixture struct {
	*credentialJournalFixture
	failEffect  bool
	afterEffect func()
}

func (j *maintenanceRecoveryJournalFixture) RecordCredentialMaintenanceEffect(_ context.Context, namespace, id string, effect securityaudit.CredentialMaintenanceEffect) (securityaudit.CredentialOperation, error) {
	j.mu.Lock()
	if j.failEffect {
		j.mu.Unlock()
		return securityaudit.CredentialOperation{}, securityaudit.ErrUnavailable
	}
	operation, ok := j.operations[id]
	if !ok || operation.Intent.Admission.Namespace != namespace {
		j.mu.Unlock()
		return securityaudit.CredentialOperation{}, securityaudit.ErrOperationNotFound
	}
	if operation.Completion != nil || operation.Effect != nil && *operation.Effect != effect {
		j.mu.Unlock()
		return securityaudit.CredentialOperation{}, securityaudit.ErrConflict
	}
	operation.Effect = &effect
	j.operations[id] = operation
	hook := j.afterEffect
	j.mu.Unlock()
	if hook != nil {
		hook()
	}
	return operation, nil
}
func (j *maintenanceRecoveryJournalFixture) PendingCredentialMaintenanceOperations(_ context.Context, namespace, after string, limit int) ([]securityaudit.CredentialOperation, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	var result []securityaudit.CredentialOperation
	for id, operation := range j.operations {
		if id > after && operation.Intent.Admission.Namespace == namespace && operation.Completion == nil && validMaintenanceOperation(operation) {
			result = append(result, operation)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Intent.Admission.OperationID < result[j].Intent.Admission.OperationID
	})
	if len(result) > limit {
		result = result[:limit]
	}
	return result, nil
}
func (j *maintenanceRecoveryJournalFixture) FinishCredentialMaintenanceOperation(ctx context.Context, namespace, id, outcome string, at time.Time, effect *securityaudit.CredentialMaintenanceEffect) (securityaudit.Event, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	operation, ok := j.operations[id]
	if !ok || operation.Intent.Admission.Namespace != namespace {
		return securityaudit.Event{}, securityaudit.ErrOperationNotFound
	}
	if j.failCompletion {
		return securityaudit.Event{}, securityaudit.ErrUnavailable
	}
	if !reflect.DeepEqual(operation.Effect, effect) {
		return securityaudit.Event{}, securityaudit.ErrConflict
	}
	if operation.Completion != nil {
		if operation.Completion.Outcome != outcome {
			return securityaudit.Event{}, securityaudit.ErrConflict
		}
		return *operation.Completion, nil
	}
	event := operation.Intent.Admission
	event.ID = operation.Intent.CompletionEventID
	event.Phase = "completion"
	event.Outcome = outcome
	event.ObservedAt = at
	if _, err := j.Append(ctx, event); err != nil {
		return securityaudit.Event{}, err
	}
	operation.Completion = &event
	j.operations[id] = operation
	return event, nil
}

type maintenanceRecoveryManagerFixture struct {
	status                 operatorcredentials.MaintenanceStatus
	namespace              string
	state                  string
	preparations, releases int
	lostReleaseAck         bool
	journal                *maintenanceRecoveryJournalFixture
}

func (m *maintenanceRecoveryManagerFixture) Namespace() string { return m.namespace }
func (m *maintenanceRecoveryManagerFixture) Inspect(context.Context) (operatorcredentials.MaintenanceStatus, error) {
	return m.status, nil
}
func (m *maintenanceRecoveryManagerFixture) WriterState(context.Context, string, string) (string, error) {
	return m.state, nil
}
func (m *maintenanceRecoveryManagerFixture) PrepareRecovery(_ context.Context, uid, version, id, replace string) (operatorcredentials.MaintenanceRecoveryHandle, error) {
	if m.status.UID != uid || m.status.Version != version {
		return nil, operatorcredentials.ErrConflict
	}
	if operation, err := m.journal.GetCredentialOperation(context.Background(), m.namespace, id); err != nil || operation.Completion != nil {
		return nil, operatorcredentials.ErrUnavailable
	}
	if m.status.RecoveryOperationID != "" && m.status.RecoveryOperationID != id && m.status.RecoveryOperationID != replace {
		return nil, operatorcredentials.ErrConflict
	}
	m.preparations++
	m.status.Version = uuid.NewString()
	m.status.RecoveryOperationID = id
	return &maintenanceRecoveryCapabilityFixture{manager: m, id: id, proof: operatorcredentials.MaintenanceReleaseProof{ResourceUID: uid, ResourceVersion: m.status.Version, ProofID: uuid.NewString()}}, nil
}
func (m *maintenanceRecoveryManagerFixture) ResumeRecovery(_ context.Context, id string, proof operatorcredentials.MaintenanceReleaseProof) (operatorcredentials.MaintenanceRecoveryHandle, error) {
	if proof.ResourceUID != m.status.UID || proof.ResourceVersion != m.status.Version || m.status.RecoveryOperationID != id {
		return nil, operatorcredentials.ErrConflict
	}
	return &maintenanceRecoveryCapabilityFixture{manager: m, id: id, proof: proof}, nil
}

type maintenanceRecoveryCapabilityFixture struct {
	manager *maintenanceRecoveryManagerFixture
	id      string
	proof   operatorcredentials.MaintenanceReleaseProof
}

func (c *maintenanceRecoveryCapabilityFixture) Proof() operatorcredentials.MaintenanceReleaseProof {
	return c.proof
}
func (c *maintenanceRecoveryCapabilityFixture) Release(context.Context) error {
	operation, err := c.manager.journal.GetCredentialOperation(context.Background(), c.manager.namespace, c.id)
	if err != nil || operation.Effect == nil || operation.Effect.ResourceVersion != c.proof.ResourceVersion || operation.Effect.ProofID != c.proof.ProofID {
		return errors.New("delete attempted before durable intent")
	}
	if c.manager.status.Version != c.proof.ResourceVersion {
		return operatorcredentials.ErrConflict
	}
	c.manager.releases++
	c.manager.status = operatorcredentials.MaintenanceStatus{}
	if c.manager.lostReleaseAck {
		return operatorcredentials.ErrUnavailable
	}
	return nil
}

type maintenanceReconcilerFixture struct {
	namespace string
	err       error
	checks    int
}

func (v *maintenanceReconcilerFixture) Namespace() string { return v.namespace }
func (v *maintenanceReconcilerFixture) Verify(context.Context, operatorcredentials.MaintenanceStatus) error {
	v.checks++
	return v.err
}
func maintenanceRecoveryHTTPFixture(t *testing.T) (*Server, *maintenanceRecoveryManagerFixture, *maintenanceRecoveryJournalFixture, *maintenanceReconcilerFixture, http.Handler) {
	t.Helper()
	journal := &maintenanceRecoveryJournalFixture{credentialJournalFixture: &credentialJournalFixture{SecurityAudit: testutil.NewSecurityAudit(), operations: map[string]securityaudit.CredentialOperation{}}}
	manager := &maintenanceRecoveryManagerFixture{namespace: "kyber-system", state: operatorcredentials.MaintenanceWriterStopped, journal: journal, status: operatorcredentials.MaintenanceStatus{Present: true, Kind: operatorcredentials.MaintenanceUpgrade, ExecutionID: uuid.NewString(), StartedAt: time.Now().UTC(), UID: uuid.NewString(), Version: "initial-version", Writer: &operatorcredentials.MaintenanceWriter{PodName: "original-pod", PodUID: uuid.NewString(), JobName: "original-job", JobUID: uuid.NewString(), Container: "upgrade"}}}
	verifier := &maintenanceReconcilerFixture{namespace: "kyber-system"}
	server := &Server{Namespace: "kyber-system", APIKey: "root-fixture", Callers: []ScopedCaller{usageCallerFixture("restricted-fixture", "Reporter")}, SecurityAudit: journal, OperatorCredentialMaintenance: manager, OperatorCredentialMaintenanceReconciler: verifier}
	return server, manager, journal, verifier, server.BuildHandler()
}
func maintenanceRecoveryRequest(t *testing.T, handler http.Handler, method, path, id, version, key string) *httptest.ResponseRecorder {
	t.Helper()
	body := ""
	if id != "" {
		raw, _ := json.Marshal(map[string]string{"operationId": id})
		body = string(raw)
	}
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+key)
	if version != "" {
		request.Header.Set("If-Match", version)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}
func TestManagedCredentialMaintenanceRecoveryRequiresAdmissionAndExactEffect(t *testing.T) {
	_, manager, journal, _, handler := maintenanceRecoveryHTTPFixture(t)
	version := credentialRegistryETag(manager.status.UID, manager.status.Version)
	journal.failAdmission = true
	if response := maintenanceRecoveryRequest(t, handler, "POST", "/api/v1/operator-credentials/maintenance/recover", uuid.NewString(), version, "root-fixture"); response.Code != 503 || manager.preparations != 0 || manager.releases != 0 {
		t.Fatal("unadmitted recovery started", response.Code)
	}
	journal.failAdmission = false
	response := maintenanceRecoveryRequest(t, handler, "POST", "/api/v1/operator-credentials/maintenance/recover", uuid.NewString(), version, "root-fixture")
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"status":"success"`) || manager.preparations != 1 || manager.releases != 1 {
		t.Fatal("verified release failed", response.Code, response.Body.String())
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("recovery response cacheable")
	}
}
func TestManagedCredentialMaintenanceRecoveryCheckIsReadOnlyAndResolveExplicit(t *testing.T) {
	_, manager, journal, _, handler := maintenanceRecoveryHTTPFixture(t)
	journal.failEffect = true
	id := uuid.NewString()
	version := credentialRegistryETag(manager.status.UID, manager.status.Version)
	response := maintenanceRecoveryRequest(t, handler, "POST", "/api/v1/operator-credentials/maintenance/recover", id, version, "root-fixture")
	if response.Code != 202 || manager.releases != 0 || manager.preparations != 1 {
		t.Fatal("unconfirmed delete intent released block", response.Code)
	}
	journal.failEffect = false
	before := manager.status.Version
	response = maintenanceRecoveryRequest(t, handler, "GET", "/api/v1/operator-credentials/maintenance/operations?operationId="+id, "", "", "root-fixture")
	if response.Code != 200 || manager.releases != 0 || manager.preparations != 1 || manager.status.Version != before {
		t.Fatal("GET resolved an interrupted operation", response.Code)
	}
	response = maintenanceRecoveryRequest(t, handler, "POST", "/api/v1/operator-credentials/maintenance/operations/resolve", id, "", "root-fixture")
	if response.Code != 200 || manager.releases != 1 || !strings.Contains(response.Body.String(), `"status":"success"`) {
		t.Fatal("explicit resolution failed", response.Code, response.Body.String())
	}
}
func TestManagedCredentialMaintenanceRecoveryLostDeleteAcknowledgementDoesNotRepeatIssuance(t *testing.T) {
	_, manager, _, _, handler := maintenanceRecoveryHTTPFixture(t)
	manager.lostReleaseAck = true
	id := uuid.NewString()
	version := credentialRegistryETag(manager.status.UID, manager.status.Version)
	response := maintenanceRecoveryRequest(t, handler, "POST", "/api/v1/operator-credentials/maintenance/recover", id, version, "root-fixture")
	if response.Code != 202 || manager.releases != 1 {
		t.Fatal("unknown delete recorded as final", response.Code)
	}
	response = maintenanceRecoveryRequest(t, handler, "POST", "/api/v1/operator-credentials/maintenance/operations/resolve", id, "", "root-fixture")
	if response.Code != 200 || manager.releases != 1 || !strings.Contains(response.Body.String(), `"status":"success"`) {
		t.Fatal("committed delete not reconciled", response.Code, response.Body.String())
	}
}
func TestManagedCredentialMaintenanceRecoveryRejectsRoleAndStateFailures(t *testing.T) {
	for _, scenario := range []string{"restricted", "stale-version", "live-writer", "release-unverified", "retired-after-admission", "retired-after-effect-admission"} {
		t.Run(scenario, func(t *testing.T) {
			server, manager, journal, verifier, handler := maintenanceRecoveryHTTPFixture(t)
			version := credentialRegistryETag(manager.status.UID, manager.status.Version)
			key := "root-fixture"
			switch scenario {
			case "restricted":
				key = "restricted-fixture"
			case "stale-version":
				version = `"stale"`
			case "live-writer":
				manager.state = operatorcredentials.MaintenanceWriterRunning
			case "release-unverified":
				verifier.err = errors.New("private release diagnostic")
			case "retired-after-admission":
				journal.afterAdmission = func() { server.auth.SetKey("replacement-fixture") }
			case "retired-after-effect-admission":
				journal.afterEffect = func() { server.auth.SetKey("replacement-fixture") }
			}
			response := maintenanceRecoveryRequest(t, handler, "POST", "/api/v1/operator-credentials/maintenance/recover", uuid.NewString(), version, key)
			if manager.releases != 0 || response.Code == 200 || strings.Contains(response.Body.String(), "private release diagnostic") {
				t.Fatal("unsafe recovery proceeded", response.Code)
			}
			if scenario != "retired-after-effect-admission" && manager.preparations != 0 {
				t.Fatal("unverified or unauthorized recovery fenced metadata")
			}
		})
	}
}

func TestManagedCredentialMaintenanceRecoveryCannotDisplacePendingEffect(t *testing.T) {
	// Retire the actor after the durable effect admission but before deletion.
	server, manager2, journal2, _, handler2 := maintenanceRecoveryHTTPFixture(t)
	journal2.afterEffect = func() { server.auth.SetKey("replacement-fixture") }
	oldID := uuid.NewString()
	version := credentialRegistryETag(manager2.status.UID, manager2.status.Version)
	response := maintenanceRecoveryRequest(t, handler2, "POST", "/api/v1/operator-credentials/maintenance/recover", oldID, version, "root-fixture")
	if response.Code != 403 || manager2.releases != 0 || manager2.preparations != 1 {
		t.Fatal("fixture did not retain admitted effect", response.Code)
	}
	journal2.afterEffect = nil
	version = credentialRegistryETag(manager2.status.UID, manager2.status.Version)
	response = maintenanceRecoveryRequest(t, handler2, "POST", "/api/v1/operator-credentials/maintenance/recover", uuid.NewString(), version, "replacement-fixture")
	if response.Code != 409 || manager2.preparations != 1 || manager2.releases != 0 {
		t.Fatal("pending effect displaced", response.Code)
	}
	response = maintenanceRecoveryRequest(t, handler2, "POST", "/api/v1/operator-credentials/maintenance/operations/resolve", oldID, "", "replacement-fixture")
	if response.Code != 200 || manager2.releases != 1 {
		t.Fatal("original effect could not resolve", response.Code, response.Body.String())
	}
}

func TestManagedCredentialMaintenanceRecoveryRejectsMalformedBodies(t *testing.T) {
	for _, body := range []string{`{}`, `{"operationId":"not-an-id"}`, `{"operationId":"00000000-0000-0000-0000-000000000000"}`, `{"operationId":"57e9038a-c35e-4b7b-bcb7-afcd532c28a1","operationId":"57e9038a-c35e-4b7b-bcb7-afcd532c28a1"}`, `{"operationId":"57e9038a-c35e-4b7b-bcb7-afcd532c28a1","force":true}`, `{"operationId":"57e9038a-c35e-4b7b-bcb7-afcd532c28a1"} {}`} {
		_, manager, _, _, handler := maintenanceRecoveryHTTPFixture(t)
		request := httptest.NewRequest(http.MethodPost, "/api/v1/operator-credentials/maintenance/recover", strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer root-fixture")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != 400 || manager.preparations != 0 || manager.releases != 0 {
			t.Fatal("malformed input accepted", response.Code)
		}
	}
}

func TestManagedCredentialMaintenancePendingReadsDoNotResolve(t *testing.T) {
	_, manager, journal, _, handler := maintenanceRecoveryHTTPFixture(t)
	journal.failEffect = true
	id := uuid.NewString()
	version := credentialRegistryETag(manager.status.UID, manager.status.Version)
	response := maintenanceRecoveryRequest(t, handler, "POST", "/api/v1/operator-credentials/maintenance/recover", id, version, "root-fixture")
	if response.Code != 202 {
		t.Fatal("fixture not pending", response.Code)
	}
	before := manager.status.Version
	response = maintenanceRecoveryRequest(t, handler, "GET", "/api/v1/operator-credentials/maintenance/operations/pending", "", "", "root-fixture")
	if response.Code != 200 || !strings.Contains(response.Body.String(), id) || manager.preparations != 1 || manager.releases != 0 || manager.status.Version != before {
		t.Fatal("pending read changed maintenance", response.Code, response.Body.String())
	}
	response = maintenanceRecoveryRequest(t, handler, "GET", "/api/v1/operator-credentials/maintenance/operations/pending?after=invalid", "", "", "root-fixture")
	if response.Code != 400 {
		t.Fatal("invalid continuation accepted", response.Code)
	}
	response = maintenanceRecoveryRequest(t, handler, "GET", "/api/v1/operator-credentials/maintenance/operations/pending", "", "", "restricted-fixture")
	if response.Code != 403 || strings.Contains(response.Body.String(), id) {
		t.Fatal("pending operation disclosed", response.Code)
	}
}

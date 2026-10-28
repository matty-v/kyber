package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/matty-v/kyber/internal/testutil"
	"github.com/matty-v/kyber/pkg/operatorcredentials"
	"github.com/matty-v/kyber/pkg/securityaudit"
)

type managedAuthorityFixture struct {
	mu                sync.Mutex
	document          operatorcredentials.Document
	version           int
	writes            int
	commitThenLoseAck bool
	loseFenceAck      bool
	outageAfterFence  bool
	loadUnavailable   bool
	beforeWrite       func()
}

func (a *managedAuthorityFixture) Load(context.Context) (operatorcredentials.Snapshot, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.loadUnavailable {
		return operatorcredentials.Snapshot{}, operatorcredentials.ErrUnavailable
	}
	return operatorcredentials.Snapshot{UID: "managed-fixture", Version: strconv.Itoa(a.version), Document: a.document.Clone()}, nil
}
func (a *managedAuthorityFixture) Replace(_ context.Context, uid, version string, next operatorcredentials.Document) (operatorcredentials.Snapshot, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if uid != "managed-fixture" || version != strconv.Itoa(a.version) {
		return operatorcredentials.Snapshot{}, operatorcredentials.ErrConflict
	}
	if a.beforeWrite != nil {
		a.beforeWrite()
	}
	if next.Validate() != nil {
		return operatorcredentials.Snapshot{}, operatorcredentials.ErrInvalid
	}
	a.document = next.Clone()
	a.version++
	a.writes++
	if a.commitThenLoseAck {
		return operatorcredentials.Snapshot{}, operatorcredentials.ErrUnavailable
	}
	return operatorcredentials.Snapshot{UID: uid, Version: strconv.Itoa(a.version), Document: a.document.Clone()}, nil
}
func (a *managedAuthorityFixture) Fence(_ context.Context, uid, version string) (operatorcredentials.Snapshot, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if uid != "managed-fixture" || version != strconv.Itoa(a.version) {
		return operatorcredentials.Snapshot{}, operatorcredentials.ErrConflict
	}
	a.version++
	if a.outageAfterFence {
		a.loadUnavailable = true
	}
	if a.loseFenceAck || a.outageAfterFence {
		return operatorcredentials.Snapshot{}, operatorcredentials.ErrUnavailable
	}
	return operatorcredentials.Snapshot{UID: uid, Version: strconv.Itoa(a.version), Document: a.document.Clone()}, nil
}

type credentialJournalFixture struct {
	*testutil.SecurityAudit
	mu             sync.Mutex
	operations     map[string]securityaudit.CredentialOperation
	failAdmission  bool
	failCompletion bool
	afterAdmission func()
}

func (j *credentialJournalFixture) GetCredentialOperation(_ context.Context, namespace, id string) (securityaudit.CredentialOperation, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	got, ok := j.operations[id]
	if !ok || got.Intent.Admission.Namespace != namespace {
		return securityaudit.CredentialOperation{}, securityaudit.ErrOperationNotFound
	}
	got.NewlyAdmitted = false
	return got, nil
}
func (j *credentialJournalFixture) AdmitCredentialOperation(ctx context.Context, intent securityaudit.CredentialIntent) (securityaudit.CredentialOperation, error) {
	if intent.Validate() != nil {
		return securityaudit.CredentialOperation{}, securityaudit.ErrInvalid
	}
	j.mu.Lock()
	if j.failAdmission {
		j.mu.Unlock()
		return securityaudit.CredentialOperation{}, securityaudit.ErrUnavailable
	}
	if got, ok := j.operations[intent.Admission.OperationID]; ok {
		j.mu.Unlock()
		got.NewlyAdmitted = false
		return got, nil
	}
	if _, err := j.Append(ctx, intent.Admission); err != nil {
		j.mu.Unlock()
		return securityaudit.CredentialOperation{}, err
	}
	result := securityaudit.CredentialOperation{Intent: intent}
	j.operations[intent.Admission.OperationID] = result
	hook := j.afterAdmission
	j.mu.Unlock()
	if hook != nil {
		hook()
	}
	result.NewlyAdmitted = true
	return result, nil
}
func (j *credentialJournalFixture) PendingCredentialOperations(context.Context, string, string, int) ([]securityaudit.CredentialOperation, error) {
	return nil, securityaudit.ErrUnavailable
}
func (j *credentialJournalFixture) FinishCredentialOperation(ctx context.Context, namespace, id, outcome string, at time.Time) (securityaudit.Event, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.failCompletion {
		return securityaudit.Event{}, securityaudit.ErrUnavailable
	}
	got, ok := j.operations[id]
	if !ok || got.Intent.Admission.Namespace != namespace {
		return securityaudit.Event{}, securityaudit.ErrOperationNotFound
	}
	if got.Completion != nil {
		if got.Completion.Outcome != outcome {
			return securityaudit.Event{}, securityaudit.ErrConflict
		}
		return *got.Completion, nil
	}
	event := got.Intent.Admission
	event.ID = got.Intent.CompletionEventID
	event.Phase = "completion"
	event.Outcome = outcome
	event.ObservedAt = at
	if _, err := j.Append(ctx, event); err != nil {
		return securityaudit.Event{}, err
	}
	got.Completion = &event
	j.operations[id] = got
	return event, nil
}

func managedHTTPFixture(t *testing.T) (*Server, *managedAuthorityFixture, *credentialJournalFixture, http.Handler) {
	t.Helper()
	authority := &managedAuthorityFixture{document: registryFixtureDocument(t), version: 1}
	journal := &credentialJournalFixture{SecurityAudit: testutil.NewSecurityAudit(), operations: map[string]securityaudit.CredentialOperation{}}
	server := &Server{Namespace: "kyber-system", APIKey: "original-installation-fixture", OperatorCredentialAuthority: authority, SecurityAudit: journal}
	return server, authority, journal, server.BuildHandler()
}
func managedMutationHTTP(t *testing.T, handler http.Handler, request credentialMutationRequest, etag, key string) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"operationId": request.OperationID, "action": request.Action, "credentialId": request.CredentialID, "policy": request.Policy, "key": request.Key})
	if err != nil {
		t.Fatal("fixture body encoding failed")
	}
	r := httptest.NewRequest(http.MethodPost, "/api/v1/operator-credentials/mutations", bytes.NewReader(raw))
	r.Header.Set("Authorization", "Bearer "+key)
	r.Header.Set("If-Match", etag)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, r)
	return response
}
func managedCreateFixtureRequest() credentialMutationRequest {
	return credentialMutationRequest{OperationID: uuid.NewString(), Action: "create", Policy: mutationFixturePolicy(), Key: mutationFixtureKey()}
}

func TestManagedCredentialHTTPRequiresAdmissionBeforeEffectAndReturnsNoKey(t *testing.T) {
	_, authority, journal, handler := managedHTTPFixture(t)
	request := managedCreateFixtureRequest()
	authority.beforeWrite = func() {
		journal.mu.Lock()
		defer journal.mu.Unlock()
		if _, ok := journal.operations[request.OperationID]; !ok {
			t.Error("key write preceded required admission")
		}
	}
	response := managedMutationHTTP(t, handler, request, credentialRegistryETag("managed-fixture", "1"), "original-installation-fixture")
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d", response.Code)
	}
	var view credentialOperationView
	if json.Unmarshal(response.Body.Bytes(), &view) != nil || view.Status != "success" || view.CredentialID == "" || view.OperationID != request.OperationID {
		t.Fatal("missing confirmed operation result")
	}
	if strings.Contains(response.Body.String(), request.Key) || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("key material escaped or result is cacheable")
	}
	if authority.writes != 1 {
		t.Fatal("unexpected write count")
	}
	for _, record := range journal.Records() {
		if record.RequestID == "" || record.RequestID == request.OperationID {
			t.Fatal("HTTP request correlation was lost")
		}
	}
	query := httptest.NewRequest(http.MethodGet, "/api/v1/operator-credential-operations?operationId="+request.OperationID, nil)
	query.Header.Set("Authorization", "Bearer original-installation-fixture")
	observed := httptest.NewRecorder()
	handler.ServeHTTP(observed, query)
	if observed.Code != http.StatusOK || strings.Contains(observed.Body.String(), request.Key) {
		t.Fatal("operation lookup exposed material or failed")
	}
	replay := managedMutationHTTP(t, handler, request, credentialRegistryETag("managed-fixture", "2"), "original-installation-fixture")
	if replay.Code < 400 || authority.writes != 1 {
		t.Fatal("repeated POST reissued a key or claimed a new success")
	}
}

func TestManagedCredentialHTTPAuditFailureAndStaleReviewNeverWrite(t *testing.T) {
	for _, kind := range []string{"audit unavailable", "stale review", "wrong registry review", "restricted caller"} {
		t.Run(kind, func(t *testing.T) {
			_, authority, journal, handler := managedHTTPFixture(t)
			etag := credentialRegistryETag("managed-fixture", "1")
			key := "original-installation-fixture"
			want := http.StatusServiceUnavailable
			switch kind {
			case "audit unavailable":
				journal.failAdmission = true
			case "stale review":
				etag = credentialRegistryETag("managed-fixture", "old")
				want = http.StatusConflict
			case "wrong registry review":
				etag = credentialRegistryETag("other-registry", "1")
				want = http.StatusConflict
			case "restricted caller":
				key = "client-key-fixture"
				want = http.StatusForbidden
			}
			response := managedMutationHTTP(t, handler, managedCreateFixtureRequest(), etag, key)
			if response.Code != want || authority.writes != 0 {
				t.Fatalf("status=%d writes=%d", response.Code, authority.writes)
			}
		})
	}
}

func TestManagedCredentialHTTPRechecksAdminAfterAdmission(t *testing.T) {
	_, authority, journal, handler := managedHTTPFixture(t)
	journal.afterAdmission = func() {
		authority.mu.Lock()
		defer authority.mu.Unlock()
		for i := range authority.document.Credentials {
			if authority.document.Credentials[i].ID == installationAdminCredentialID {
				authority.document.Credentials[i].Role = operatorcredentials.RoleRestricted
				authority.document.Credentials[i].Scopes = []string{string(ScopeTasksRead)}
			}
		}
		authority.version++
	}
	response := managedMutationHTTP(t, handler, managedCreateFixtureRequest(), credentialRegistryETag("managed-fixture", "1"), "original-installation-fixture")
	var view credentialOperationView
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &view) != nil || view.Status != "failure" || authority.writes != 0 {
		t.Fatal("post-admission policy change did not stop mutation")
	}
}

func TestManagedCredentialHTTPUnknownEffectOrCompletionRemainsPending(t *testing.T) {
	for _, kind := range []string{"lost write acknowledgement", "completion unavailable"} {
		t.Run(kind, func(t *testing.T) {
			_, authority, journal, handler := managedHTTPFixture(t)
			if kind == "lost write acknowledgement" {
				authority.commitThenLoseAck = true
			} else {
				journal.failCompletion = true
			}
			request := managedCreateFixtureRequest()
			response := managedMutationHTTP(t, handler, request, credentialRegistryETag("managed-fixture", "1"), "original-installation-fixture")
			var view credentialOperationView
			if response.Code != http.StatusAccepted || json.Unmarshal(response.Body.Bytes(), &view) != nil || view.Status != "pending" || authority.writes != 1 {
				t.Fatal("unknown acknowledgement became false success/failure")
			}
			got, err := journal.GetCredentialOperation(context.Background(), "kyber-system", request.OperationID)
			if err != nil || got.Completion != nil {
				t.Fatal("uncertain completion was finalized", err)
			}
		})
	}
}

func TestManagedCredentialMutationInputRejectsDuplicatesAliasesAndTrailingValues(t *testing.T) {
	for _, raw := range []string{`{"operationId":"a","operationId":"b"}`, `{"OperationId":"a"}`, `{"policy":{"name":"a","Name":"b"}}`, `{} {}`, `null`} {
		r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(raw))
		if _, err := decodeCredentialMutationRequest(httptest.NewRecorder(), r); !errors.Is(err, errCredentialMutationInvalid) {
			t.Fatal("ambiguous mutation input accepted")
		}
	}
}

func reconcileMutationHTTP(t *testing.T, handler http.Handler, operationID string) *httptest.ResponseRecorder {
	t.Helper()
	raw, _ := json.Marshal(map[string]string{"operationId": operationID})
	request := httptest.NewRequest(http.MethodPost, "/api/v1/operator-credential-operations/reconcile", bytes.NewReader(raw))
	request.Header.Set("Authorization", "Bearer original-installation-fixture")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func TestManagedCredentialRecoveryConfirmsCommittedWriteWithoutReissuing(t *testing.T) {
	_, authority, journal, handler := managedHTTPFixture(t)
	authority.commitThenLoseAck = true
	request := managedCreateFixtureRequest()
	initial := managedMutationHTTP(t, handler, request, credentialRegistryETag("managed-fixture", "1"), "original-installation-fixture")
	if initial.Code != http.StatusAccepted {
		t.Fatal("lost acknowledgement not pending")
	}
	recovered := reconcileMutationHTTP(t, handler, request.OperationID)
	var view credentialOperationView
	if recovered.Code != http.StatusOK || json.Unmarshal(recovered.Body.Bytes(), &view) != nil || view.Status != "success" || authority.writes != 1 {
		t.Fatal("committed operation was reissued or not recovered")
	}
	got, err := journal.GetCredentialOperation(context.Background(), "kyber-system", request.OperationID)
	if err != nil || got.Completion == nil || got.Completion.Outcome != "success" {
		t.Fatal("recovery completion not durable in fixture", err)
	}
	again := reconcileMutationHTTP(t, handler, request.OperationID)
	if again.Code != http.StatusOK || authority.writes != 1 {
		t.Fatal("recovery retry reissued a key")
	}
}

func TestManagedCredentialRecoveryFencesUncommittedAndLostFenceAcknowledgements(t *testing.T) {
	for _, fault := range []string{"acknowledged", "lost fence acknowledgement", "authority outage after fence"} {
		t.Run(fault, func(t *testing.T) {
			_, authority, journal, handler := managedHTTPFixture(t)
			snapshot, err := authority.Load(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			registry, err := compileRegistryCredentials(snapshot.Document, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			actor, err := registry.authenticate(registryBearer("original-installation-fixture"))
			if err != nil {
				t.Fatal(err)
			}
			request := managedCreateFixtureRequest()
			next, reference, err := prepareManagedCredentialMutation(snapshot, *actor, request, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			intent := securityaudit.CredentialIntent{Admission: securityaudit.Event{ID: reference.AdmissionEventID, OperationID: reference.ID, ObservedAt: reference.At, Namespace: "kyber-system", PrincipalID: reference.ActorPrincipalID, CredentialID: reference.ActorCredentialID, Generation: reference.ActorGeneration, CredentialAuthority: "registry", RequestID: reference.RequestID, Action: "credential:create", ResourceKind: "credential", ResourceName: reference.TargetCredentialID, Phase: "authorization", Outcome: "allow"}, CompletionEventID: reference.CompletionEventID, ExpectedVersion: snapshot.Version, RegistryUID: snapshot.UID}
			if _, err := journal.AdmitCredentialOperation(context.Background(), intent); err != nil {
				t.Fatal(err)
			}
			get := httptest.NewRequest(http.MethodGet, "/api/v1/operator-credential-operations?operationId="+request.OperationID, nil)
			get.Header.Set("Authorization", "Bearer original-installation-fixture")
			observed := httptest.NewRecorder()
			handler.ServeHTTP(observed, get)
			current, _ := authority.Load(context.Background())
			if observed.Code != http.StatusOK || current.Version != snapshot.Version {
				t.Fatal("read-only polling fenced an active write")
			}
			authority.loseFenceAck = fault == "lost fence acknowledgement"
			authority.outageAfterFence = fault == "authority outage after fence"
			recovered := reconcileMutationHTTP(t, handler, request.OperationID)
			if fault == "authority outage after fence" {
				var uncertain credentialOperationView
				if recovered.Code != http.StatusAccepted || json.Unmarshal(recovered.Body.Bytes(), &uncertain) != nil || uncertain.Status != "pending" {
					t.Fatal("authority outage invented a final outcome")
				}
				probe := httptest.NewRecorder()
				handler.ServeHTTP(probe, registryBearer("original-installation-fixture"))
				if probe.Code != http.StatusServiceUnavailable {
					t.Fatal("authority outage fell back or requested login")
				}
				authority.mu.Lock()
				authority.loadUnavailable = false
				authority.outageAfterFence = false
				authority.mu.Unlock()
				recovered = reconcileMutationHTTP(t, handler, request.OperationID)
			}
			var view credentialOperationView
			if recovered.Code != http.StatusOK || json.Unmarshal(recovered.Body.Bytes(), &view) != nil || view.Status != "failure" || authority.writes != 0 {
				t.Fatal("uncommitted operation not safely finalized")
			}
			if _, err := authority.Replace(context.Background(), snapshot.UID, snapshot.Version, next); !errors.Is(err, operatorcredentials.ErrConflict) {
				t.Fatal("late key write survived recovery", err)
			}
		})
	}
}

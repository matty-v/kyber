package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/matty-v/kyber/internal/testutil"
	"github.com/matty-v/kyber/pkg/api"
	"github.com/matty-v/kyber/pkg/securityaudit"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestSensitiveAuditFailureBlocksBeforeBackend(t *testing.T) {
	for _, path := range []string{"/api/v1/agents/alpha/secrets/TOKEN", "/api/v1/agents/alpha/secrets", "/api/v1/agents/alpha/logs", "/api/v1/agents/alpha/exec", "/api/v1/machines/worker-1/logs", "/api/v1/machines/worker-1/exec", "/api/v1/logging/logs", "/api/v1/logging/export"} {
		for _, unavailable := range []bool{false, true} {
			t.Run(path+map[bool]string{false: "/unconfigured", true: "/failed"}[unavailable], func(t *testing.T) {
				s := &api.Server{APIKey: testAPIKey, Namespace: "kyber-system"}
				if unavailable {
					store := testutil.NewSecurityAudit()
					store.Fail = errors.New("private database detail credential=do-not-expose")
					s.SecurityAudit = store
				}
				// Both Kubernetes backends are nil: reaching either would panic. A failed
				// required audit write must reject before any lookup, read or upgrade.
				rec := httptest.NewRecorder()
				s.BuildHandler().ServeHTTP(rec, auditRootRequest(http.MethodGet, path))
				if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "security_audit_unavailable") || rec.Header().Get("Cache-Control") != "no-store" {
					t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
				}
				if strings.Contains(rec.Body.String(), "do-not-expose") {
					t.Fatal("backend details leaked")
				}
			})
		}
	}
}
func TestSensitiveAuditDenialDoesNotReadBackendOrPrivateRequest(t *testing.T) {
	for _, storeFails := range []bool{false, true} {
		store := testutil.NewSecurityAudit()
		if storeFails {
			store.Fail = securityaudit.ErrUnavailable
		}
		s := &api.Server{APIKey: testAPIKey, Namespace: "kyber-system", SecurityAudit: store, Callers: []api.ScopedCaller{{Name: "task-client", Key: "audit-denied-fixture", PrincipalID: "principal_fixture", CredentialID: "credential_fixture", CredentialGeneration: 1, Scopes: []string{"tasks:read"}, AgentResources: []string{"kyber-system/alpha"}}}}
		req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/alpha/secrets/DO_NOT_RECORD?cmd=PRIVATE_ARGUMENT", nil)
		req.Header.Set("Authorization", "Bearer audit-denied-fixture")
		rec := httptest.NewRecorder()
		s.BuildHandler().ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("deny status=%d", rec.Code)
		}
		events := store.Records()
		if storeFails {
			if len(events) != 0 {
				t.Fatal("failed append reported recorded")
			}
			continue
		}
		if len(events) != 1 || events[0].Outcome != "deny" || events[0].Action != "secrets:read" || events[0].ResourceName != "alpha" || events[0].ResourceKind != "agent" || events[0].RequestID != rec.Header().Get(api.RequestIDHeader) {
			t.Fatalf("denial metadata=%+v", events)
		}
		payload, _ := json.Marshal(events)
		for _, private := range []string{"DO_NOT_RECORD", "PRIVATE_ARGUMENT", "audit-denied-fixture"} {
			if strings.Contains(string(payload), private) {
				t.Fatal("request content recorded")
			}
		}
	}
}
func TestSensitiveAuditAdmissionAndFailedCompletionAreSeparate(t *testing.T) {
	store := testutil.NewSecurityAudit()
	s := &api.Server{APIKey: testAPIKey, Namespace: "kyber-system", SecurityAudit: store, K8sClient: fake.NewClientBuilder().WithScheme(mustNewScheme(t)).Build()}
	rec := httptest.NewRecorder()
	s.BuildHandler().ServeHTTP(rec, auditRootRequest(http.MethodGet, "/api/v1/agents/missing/secrets/TOKEN"))
	events := store.Records()
	if rec.Code != http.StatusNotFound || len(events) != 2 {
		t.Fatalf("status=%d records=%d", rec.Code, len(events))
	}
	if events[0].Phase != "authorization" || events[0].Outcome != "allow" || events[1].Phase != "completion" || events[1].Outcome != "failure" || events[0].ID == events[1].ID || events[0].OperationID != events[1].OperationID || events[0].RequestID != events[1].RequestID {
		t.Fatalf("decision was confused with completion: %+v", events)
	}
}
func TestSensitiveAuditOutageDoesNotPreventSignInAndInventory(t *testing.T) {
	s := &api.Server{APIKey: testAPIKey, Namespace: "kyber-system"}
	h := s.BuildHandler()
	for _, path := range []string{"/api/v1/version", "/api/v1/operator-credentials"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, auditRootRequest(http.MethodGet, path))
		if rec.Code != http.StatusOK {
			t.Fatalf("operator discovery blocked: %s status=%d", path, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, auditRootRequest(http.MethodPost, "/api/v1/browser-session"))
	if rec.Code != http.StatusNoContent {
		t.Fatal("sign-in depends on audit database", rec.Code)
	}
}

func auditRootRequest(method, path string) *http.Request {
	r := httptest.NewRequest(method, path, nil)
	r.Header.Set("Authorization", "Bearer "+testAPIKey)
	return r
}

func TestSensitiveAuditSuccessfulReadForBearerAndBrowser(t *testing.T) {
	for _, browser := range []bool{false, true} {
		store := testutil.NewSecurityAudit()
		h := auditedSecretFixture(t, store)
		req := auditRootRequest("GET", "/api/v1/agents/alpha/secrets/TOKEN")
		if browser {
			login := httptest.NewRecorder()
			h.ServeHTTP(login, auditRootRequest("POST", "/api/v1/browser-session"))
			if login.Code != 204 {
				t.Fatal(login.Code)
			}
			req = httptest.NewRequest("GET", "/api/v1/agents/alpha/secrets/TOKEN", nil)
			req.AddCookie(login.Result().Cookies()[0])
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 200 || rec.Body.String() != "private-audit-fixture-value" {
			t.Fatalf("value read status=%d", rec.Code)
		}
		events := store.Records()
		if len(events) != 2 || events[0].Outcome != "allow" || events[1].Outcome != "success" || events[0].CredentialID != "credential_installation_admin" || events[0].Method != "GET" || events[0].CredentialAuthority != "configuration" {
			t.Fatalf("incorrect read history: %+v", events)
		}
		b, _ := json.Marshal(events)
		if strings.Contains(string(b), "private-audit-fixture-value") {
			t.Fatal("sensitive body entered audit history")
		}
	}
}

type failCompletionAudit struct{ *testutil.SecurityAudit }

func (s failCompletionAudit) Append(ctx context.Context, e securityaudit.Event) (securityaudit.Record, error) {
	if e.Phase == "completion" {
		return securityaudit.Record{}, securityaudit.ErrUnavailable
	}
	return s.SecurityAudit.Append(ctx, e)
}
func TestSensitiveAuditCompletionOutageLeavesTruthfulIncompleteAdmission(t *testing.T) {
	store := testutil.NewSecurityAudit()
	h := auditedSecretFixture(t, failCompletionAudit{store})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, auditRootRequest("GET", "/api/v1/agents/alpha/secrets/TOKEN"))
	if rec.Code != 200 {
		t.Fatal("committed admission did not permit read", rec.Code)
	}
	events := store.Records()
	if len(events) != 1 || events[0].Phase != "authorization" || events[0].Outcome != "allow" {
		t.Fatal("completion failure fabricated success")
	}
}
func auditedSecretFixture(t *testing.T, store securityaudit.Store) http.Handler {
	t.Helper()
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "alpha-user-secrets-kv", Namespace: "kyber-system"}, Data: map[string][]byte{"TOKEN": []byte("private-audit-fixture-value")}}
	c := fake.NewClientBuilder().WithScheme(mustNewScheme(t)).WithObjects(sampleAgentCRD("alpha"), secret).Build()
	s := &api.Server{APIKey: testAPIKey, Namespace: "kyber-system", SecurityAudit: store, K8sClient: c}
	return s.BuildHandler()
}

type blockedAdmissionAudit struct {
	*testutil.SecurityAudit
	started chan struct{}
	once    sync.Once
}

func (s *blockedAdmissionAudit) Append(ctx context.Context, e securityaudit.Event) (securityaudit.Record, error) {
	s.once.Do(func() { close(s.started) })
	<-ctx.Done()
	return securityaudit.Record{}, ctx.Err()
}
func TestSensitiveAuditSlowStoreDoesNotBlockOperatorDiscovery(t *testing.T) {
	store := &blockedAdmissionAudit{SecurityAudit: testutil.NewSecurityAudit(), started: make(chan struct{})}
	server := &api.Server{APIKey: testAPIKey, Namespace: "kyber-system", SecurityAudit: store}
	h := server.BuildHandler()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := auditRootRequest("GET", "/api/v1/agents/alpha/secrets/TOKEN").WithContext(ctx)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { defer close(done); h.ServeHTTP(rec, req) }()
	select {
	case <-store.started:
	case <-time.After(time.Second):
		t.Fatal("admission did not reach store")
	}
	version := httptest.NewRecorder()
	h.ServeHTTP(version, auditRootRequest("GET", "/api/v1/version"))
	if version.Code != 200 {
		t.Fatal("slow audit store prevented discovery")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("admission ignored cancellation")
	}
	if rec.Code != 503 || len(store.Records()) != 0 {
		t.Fatal("canceled admission started sensitive work", rec.Code)
	}
}

type cancelAfterSecretCommit struct {
	client.Client
	cancel context.CancelFunc
}

func (c cancelAfterSecretCommit) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	err := c.Client.Patch(ctx, obj, patch, opts...)
	if err == nil && obj.GetName() == "alpha-user-secrets-kv" {
		c.cancel()
	}
	return err
}
func TestSensitiveAuditCommittedMutationSurvivesResponseCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	agent := sampleAgentCRD("alpha")
	agent.Status.Phase = "Stopped"
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "alpha-user-secrets-kv", Namespace: "kyber-system"}, Data: map[string][]byte{"TOKEN": []byte("original-fixture")}}
	base := fake.NewClientBuilder().WithScheme(mustNewScheme(t)).WithObjects(agent, secret).Build()
	store := testutil.NewSecurityAudit()
	server := &api.Server{APIKey: testAPIKey, Namespace: "kyber-system", SecurityAudit: store, K8sClient: cancelAfterSecretCommit{base, cancel}}
	req := authedJSONRequest(t, "PUT", "/api/v1/agents/alpha/secrets/TOKEN", map[string]string{"value": "replacement-fixture"}).WithContext(ctx)
	rec := httptest.NewRecorder()
	server.BuildHandler().ServeHTTP(rec, req)
	if rec.Code != 204 || ctx.Err() == nil {
		t.Fatalf("fixture did not commit then lose response: %d", rec.Code)
	}
	records := store.Records()
	if len(records) != 2 || records[1].Outcome != "success" {
		t.Fatal("confirmed mutation was confused with rollback")
	}
}

// Simulate the API server committing before cancellation causes the client to
// lose its acknowledgement. The HTTP failure is not proof of rollback.
type cancelAfterAmbiguousSecretCommit struct {
	client.Client
	cancel context.CancelFunc
}

func (c cancelAfterAmbiguousSecretCommit) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	err := c.Client.Patch(ctx, obj, patch, opts...)
	if err == nil && obj.GetName() == "alpha-user-secrets-kv" {
		c.cancel()
		return context.Canceled
	}
	return err
}
func TestSensitiveAuditUnacknowledgedCommitIsUnknown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	agent := sampleAgentCRD("alpha")
	agent.Status.Phase = "Stopped"
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "alpha-user-secrets-kv", Namespace: "kyber-system"}, Data: map[string][]byte{"TOKEN": []byte("original-fixture")}}
	base := fake.NewClientBuilder().WithScheme(mustNewScheme(t)).WithObjects(agent, secret).Build()
	audit := testutil.NewSecurityAudit()
	server := &api.Server{APIKey: testAPIKey, Namespace: "kyber-system", SecurityAudit: audit, K8sClient: cancelAfterAmbiguousSecretCommit{base, cancel}}
	request := authedJSONRequest(t, "PUT", "/api/v1/agents/alpha/secrets/TOKEN", map[string]string{"value": "replacement-fixture"}).WithContext(ctx)
	server.BuildHandler().ServeHTTP(httptest.NewRecorder(), request)
	committed := &corev1.Secret{}
	if err := base.Get(context.Background(), client.ObjectKey{Namespace: "kyber-system", Name: "alpha-user-secrets-kv"}, committed); err != nil || string(committed.Data["TOKEN"]) != "replacement-fixture" {
		t.Fatal("fixture did not commit")
	}
	records := audit.Records()
	if len(records) != 2 || records[1].Phase != "completion" || records[1].Outcome != "unknown" {
		t.Fatal("unacknowledged write was misreported as failed or completed")
	}
}

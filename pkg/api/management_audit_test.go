package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/matty-v/kyber/pkg/archivejob"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/matty-v/kyber/internal/testutil"
	"github.com/matty-v/kyber/pkg/api"
	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestManagementAuditOutageBlocksBeforeLifecycleBackend(t *testing.T) {
	cases := []struct{ method, path, body string }{
		{"POST", "/api/v1/agents/alpha/start", ""},
		{"POST", "/api/v1/agents/alpha/stop", ""},
		{"POST", "/api/v1/agents/alpha/restart", ""},
		{"POST", "/api/v1/agents/alpha/set-model", `{"model":"fixture-model","force":true}`},
		{"POST", "/api/v1/agents/alpha/set-runtime-version", `{"runtimeVersion":"1.0"}`},
		{"POST", "/api/v1/agents/alpha/set-resources", `{"cpu":"1"}`},
		{"POST", "/api/v1/agents/alpha/increase-disk", `{"sizeGiB":60}`},
		{"DELETE", "/api/v1/agents/alpha?confirm=alpha", ""},
		{"POST", "/api/v1/machines/worker-1/start", ""},
		{"DELETE", "/api/v1/machines/worker-1?confirm=worker-1", ""},
		{"POST", "/api/v1/rotate-api-key", ""},
	}
	for _, tc := range cases {
		for _, failed := range []bool{false, true} {
			t.Run(tc.path+map[bool]string{false: "/missing", true: "/failed"}[failed], func(t *testing.T) {
				s := &api.Server{Namespace: "kyber-system", APIKey: testAPIKey, APIKeySecretName: "fixture-api-key"}
				if failed {
					store := testutil.NewSecurityAudit()
					store.Fail = errors.New("private audit backend detail")
					s.SecurityAudit = store
				}
				r := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
				r.Header.Set("Authorization", "Bearer "+testAPIKey)
				w := httptest.NewRecorder()
				s.BuildHandler().ServeHTTP(w, r)
				if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "security_audit_unavailable") || strings.Contains(w.Body.String(), "private audit backend detail") {
					t.Fatal("audit outage did not block safely", w.Code, w.Body.String())
				}
				// Both backends are nil: reaching the lifecycle or credential write would panic.
			})
		}
	}
}

type managementCommitClient struct {
	client.Client
	cancel              context.CancelFunc
	loseAcknowledgement bool
}

func (c managementCommitClient) Patch(ctx context.Context, obj client.Object, p client.Patch, options ...client.PatchOption) error {
	err := c.Client.Patch(ctx, obj, p, options...)
	if err == nil && obj.GetName() == "alpha" {
		if c.cancel != nil {
			c.cancel()
		}
		if c.loseAcknowledgement {
			return errors.New("fixture write committed but acknowledgement was lost")
		}
	}
	return err
}

func TestManagementAuditDistinguishesCommitFromAcknowledgement(t *testing.T) {
	for _, lost := range []bool{false, true} {
		t.Run(map[bool]string{false: "confirmed-write-response-lost", true: "unacknowledged-write"}[lost], func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			agent := sampleAgentCRD("alpha")
			agent.Spec.DesiredPhase = kyberv1.AgentPhaseRunning
			base := fake.NewClientBuilder().WithScheme(mustNewScheme(t)).WithObjects(agent).Build()
			audit := testutil.NewSecurityAudit()
			backend := managementCommitClient{Client: base, loseAcknowledgement: lost}
			if !lost {
				backend.cancel = cancel
			}
			s := &api.Server{Namespace: "kyber-system", APIKey: testAPIKey, SecurityAudit: audit, K8sClient: backend}
			r := auditRootRequest("POST", "/api/v1/agents/alpha/stop").WithContext(ctx)
			w := httptest.NewRecorder()
			s.BuildHandler().ServeHTTP(w, r)
			current := &kyberv1.Agent{}
			if err := base.Get(context.Background(), client.ObjectKey{Namespace: "kyber-system", Name: "alpha"}, current); err != nil || current.Spec.DesiredPhase != kyberv1.AgentPhaseStopped {
				t.Fatal("fixture did not commit lifecycle intent", err)
			}
			records := audit.Records()
			want := "success"
			if lost {
				want = "unknown"
			}
			if len(records) != 2 || records[0].Action != "lifecycle:write" || records[0].Outcome != "allow" || records[1].Phase != "completion" || records[1].Outcome != want || records[0].OperationID != records[1].OperationID {
				t.Fatal("audit confused write acknowledgement with effects", records)
			}
			if lost && (w.Code != http.StatusInternalServerError || ctx.Err() != nil) {
				t.Fatal("ambiguous write fixture must fail without cancelling the request")
			}
		})
	}
}

func TestManagementAuditUnidentifiedKeyDoesNotAcquireAnIdentity(t *testing.T) {
	audit := testutil.NewSecurityAudit()
	s := &api.Server{Namespace: "kyber-system", APIKey: testAPIKey, SecurityAudit: audit, Callers: []api.ScopedCaller{{Name: "unidentified-fixture", Key: "unidentified-fixture-key", Scopes: []string{"lifecycle:write"}, AgentResources: []string{"kyber-system/alpha"}}}}
	r := auditRootRequest("POST", "/api/v1/agents/alpha/stop")
	r.Header.Set("Authorization", "Bearer unidentified-fixture-key")
	w := httptest.NewRecorder()
	s.BuildHandler().ServeHTTP(w, r)
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "credential_identity_required") {
		t.Fatal("unidentified key was not diagnosed before backend access", w.Code)
	}
	records := audit.Records()
	if len(records) != 1 || records[0].Outcome != "deny" || records[0].PrincipalID != "" || records[0].CredentialID != "" {
		t.Fatal("denial invented attribution", records)
	}
}

func TestManagementAuditImportRetainsItsOwnAdmission(t *testing.T) {
	const key = "archive-only-fixture-key"
	h := newExportHarness(t, api.ScopedCaller{Name: "archive-only", Key: key, Scopes: []string{"archives:admin"}, AgentResources: []string{"kyber-system/" + exportAgent, "kyber-system/" + restoredAgent}})
	id, _ := h.completedExport()
	audit := testutil.NewSecurityAudit()
	h.server.SecurityAudit = audit
	response := h.post("/api/v1/agent-imports", key, importBody(map[string]string{"exportId": id}, restoredAgent, "20Gi"))
	if response.Code != http.StatusAccepted {
		t.Fatal("archive-only import was forced to acquire lifecycle authority", response.Code, response.Body.String())
	}
	records := audit.Records()
	if len(records) != 2 || records[0].Action != "archives:import" || records[0].ResourceName != restoredAgent || records[1].Outcome != "success" || records[1].OperationID != records[0].OperationID {
		t.Fatal("nested Agent create replaced import attribution", records)
	}
}

func TestManagementAuditOutageBlocksArchiveEffectsAndContents(t *testing.T) {
	h := newExportHarness(t)
	id, _ := h.completedExport()
	minted := h.do("POST", "/api/v1/agents/"+exportAgent+"/exports/"+id+"/download-link", testAPIKey)
	var link struct{ URL string }
	if minted.Code != http.StatusOK || json.Unmarshal(minted.Body.Bytes(), &link) != nil || link.URL == "" {
		t.Fatal("archive link fixture failed")
	}
	audit := testutil.NewSecurityAudit()
	audit.Fail = errors.New("private history backend detail")
	h.server.SecurityAudit = audit
	for _, action := range []string{"start", "link", "download", "cancel", "delete", "import", "upload"} {
		t.Run(action, func(t *testing.T) {
			var response *httptest.ResponseRecorder
			switch action {
			case "start":
				response = h.do("POST", "/api/v1/agents/"+exportAgent+"/exports", testAPIKey)
			case "link":
				response = h.do("POST", "/api/v1/agents/"+exportAgent+"/exports/"+id+"/download-link", testAPIKey)
			case "download":
				response = h.do("GET", link.URL, "")
			case "cancel":
				// A completed job is refused before admission; it must remain unchanged.
				response = h.do("POST", "/api/v1/agents/"+exportAgent+"/exports/"+id+"/cancel", testAPIKey)
				if response.Code != http.StatusConflict {
					t.Fatal("completed archive cancellation changed", response.Code)
				}
				return
			case "delete":
				response = h.do("DELETE", "/api/v1/agents/"+exportAgent+"/exports/"+id, testAPIKey)
			case "import":
				response = h.post("/api/v1/agent-imports", testAPIKey, importBody(map[string]string{"exportId": id}, restoredAgent, "20Gi"))
			case "upload":
				response = h.post("/api/v1/archive-uploads", testAPIKey, map[string]int{"size": 1024})
			}
			if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "security_audit_unavailable") || strings.Contains(response.Body.String(), "private history backend detail") || response.Header().Get("Content-Type") == "application/zip" {
				t.Fatal("archive operation escaped audit outage", response.Code)
			}
			for _, kind := range []archivejob.Kind{archivejob.KindExport, archivejob.KindImport, archivejob.KindUpload} {
				jobs, err := h.svc.Jobs.List(context.Background(), kind, "", 100)
				want := 0
				if kind == archivejob.KindExport {
					want = 1
				}
				if err != nil || len(jobs) != want {
					t.Fatal("audit outage created or removed an archive job", kind, len(jobs), err)
				}
			}
			if h.job(id).State != archivejob.StateCompleted {
				t.Fatal("audit outage destroyed archive")
			}
			if _, err := h.newAgent(); !apierrors.IsNotFound(err) {
				t.Fatal("audit outage created restore target", err)
			}
		})
	}
}

func TestManagementAuditArchiveDenialsRetainOnlyKnownMetadata(t *testing.T) {
	const key = "archive-denied-fixture"
	h := newExportHarness(t, api.ScopedCaller{Name: "no-archive-access", Key: key, Scopes: []string{"tasks:read"}, AgentResources: []string{"kyber-system/" + exportAgent}})
	audit := testutil.NewSecurityAudit()
	h.server.SecurityAudit = audit
	response := h.do("POST", "/api/v1/agents/"+exportAgent+"/exports", key)
	if response.Code != http.StatusForbidden {
		t.Fatal("archive scope denial changed", response.Code)
	}
	records := audit.Records()
	if len(records) != 1 || records[0].Outcome != "deny" || records[0].Action != "archives:export" || records[0].ResourceName != exportAgent {
		t.Fatal("archive denial metadata missing", records)
	}
	payload, _ := json.Marshal(records)
	if strings.Contains(string(payload), key) {
		t.Fatal("denial exposed key material")
	}
}

func TestManagementAuditKnownArchiveRefusalIsNotUnknown(t *testing.T) {
	h := newExportHarness(t)
	agent := h.agent()
	agent.Status.Phase = kyberv1.AgentPhaseStarting
	if err := h.c.Update(context.Background(), agent); err != nil {
		t.Fatal(err)
	}
	audit := testutil.NewSecurityAudit()
	h.server.SecurityAudit = audit
	response := h.do("POST", "/api/v1/agents/"+exportAgent+"/exports", testAPIKey)
	records := audit.Records()
	if response.Code != http.StatusConflict || len(records) != 2 || records[1].Outcome != "failure" {
		t.Fatal("definite pre-effect refusal was reported as uncertain", response.Code, records)
	}
}

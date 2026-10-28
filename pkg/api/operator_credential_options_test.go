package api

import (
	"context"
	"encoding/json"
	"github.com/google/uuid"
	"github.com/matty-v/kyber/pkg/securityaudit"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestManagedCredentialPermissionChoicesAreValidAndUnique(t *testing.T) {
	seen := map[Scope]bool{}
	for _, option := range managedPermissionLabels {
		if seen[option.scope] || option.label == "" {
			t.Fatalf("duplicate or unnamed choice: %s", option.scope)
		}
		seen[option.scope] = true
		if option.scope == ScopeTasksPlatformAdmin || option.scope == ScopeTasksAdmin {
			t.Fatalf("reserved administrator scope offered: %s", option.scope)
		}
		target := managedPermissionTarget(option.scope)
		if target != "none" && target != "agent" && target != "machine" {
			t.Fatalf("invalid target kind: %s", target)
		}
	}
	if !seen[ScopeAgentExec] || !seen[ScopeMachineExec] || !seen[ScopeMetricsRead] {
		t.Fatal("core supported actions missing")
	}
}

func TestManagedCredentialSelfRotationRefreshesOnlySameBrowserIdentity(t *testing.T) {
	snapshot, actor := mutationFixture(t)
	request := credentialMutationRequest{OperationID: uuid.NewString(), Action: "rotate", CredentialID: actor.CredentialID, Key: mutationFixtureKey()}
	next, _, err := prepareManagedCredentialMutation(snapshot, actor, request, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	authority := &memoryOperatorAuthority{document: next}
	server := &Server{auth: registryFixtureAuthenticator(t, authority)}
	for _, mode := range []string{"cookie", "bearer", "other-key"} {
		t.Run(mode, func(t *testing.T) {
			r := registryCookie("prior-session-not-reused")
			mutation := request
			if mode == "bearer" {
				r.Header.Set("Authorization", "Bearer old-value")
			}
			if mode == "other-key" {
				mutation.CredentialID = "other-admin"
			}
			w := httptest.NewRecorder()
			server.refreshManagedRotationSession(w, r, mutation, actor)
			cookies := w.Result().Cookies()
			if mode != "cookie" {
				if len(cookies) != 0 {
					t.Fatal("rotation switched another connection")
				}
				return
			}
			if len(cookies) != 1 {
				t.Fatal("self rotation did not refresh browser session")
			}
			authenticated, err := server.auth.Authenticate(registryCookie(cookies[0].Value))
			if err != nil || authenticated.PrincipalID != actor.PrincipalID || authenticated.TenantID != actor.TenantID || authenticated.CredentialID != actor.CredentialID || authenticated.CredentialGeneration != actor.CredentialGeneration+1 {
				t.Fatal("rotation changed identity or failed to advance session", err)
			}
		})
	}
}

type pendingChoicesJournal struct {
	*credentialJournalFixture
	onPending func(context.Context, string, string, int) ([]securityaudit.CredentialOperation, error)
}

func (j *pendingChoicesJournal) PendingCredentialOperations(ctx context.Context, namespace, after string, limit int) ([]securityaudit.CredentialOperation, error) {
	return j.onPending(ctx, namespace, after, limit)
}
func TestManagedCredentialChoiceAndPendingHTTPRequireCurrentAdmin(t *testing.T) {
	server, authority, journal, _ := managedHTTPFixture(t)
	pendingCalls := 0
	server.SecurityAudit = &pendingChoicesJournal{credentialJournalFixture: journal, onPending: func(_ context.Context, namespace, after string, limit int) ([]securityaudit.CredentialOperation, error) {
		pendingCalls++
		if namespace != "kyber-system" || after != "" || limit != 26 {
			t.Fatal("unbounded or cross-namespace pending query")
		}
		result := make([]securityaudit.CredentialOperation, 26)
		for i := range result {
			result[i].Intent.Admission.OperationID = uuid.NewString()
		}
		return result, nil
	}}
	handler := server.BuildHandler()
	for _, path := range []string{"/api/v1/operator-credentials/options", "/api/v1/operator-credential-operations/pending"} {
		for _, key := range []string{"original-installation-fixture", "client-key-fixture"} {
			r := httptest.NewRequest(http.MethodGet, path, nil)
			r.Header.Set("Authorization", "Bearer "+key)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if key == "client-key-fixture" {
				if w.Code != http.StatusForbidden {
					t.Fatalf("restricted key status=%d", w.Code)
				}
				continue
			}
			if w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("admin request status=%d", w.Code)
			}
			if strings.HasSuffix(path, "pending") {
				var page operatorCredentialPendingPage
				if json.Unmarshal(w.Body.Bytes(), &page) != nil || len(page.Items) != 25 || page.Next == "" {
					t.Fatal("pending pagination failed")
				}
			} else {
				var options operatorCredentialOptions
				if json.Unmarshal(w.Body.Bytes(), &options) != nil || options.Version != 1 || options.Namespace != "kyber-system" || len(options.Permissions) != len(managedPermissionLabels) {
					t.Fatal("invalid permission catalog")
				}
			}
			if strings.Contains(w.Body.String(), "fixture") {
				t.Fatal("private metadata escaped options/pending")
			}
		}
	}
	if pendingCalls != 1 || authority.writes != 0 || authority.version != 1 {
		t.Fatal("read-only endpoints queried as restricted user or mutated authority")
	}
}

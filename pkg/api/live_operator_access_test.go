package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/matty-v/kyber/internal/testutil"
	"github.com/matty-v/kyber/pkg/operatorcredentials"
)

func TestLiveAccessBoundAndCancelledWriter(t *testing.T) {
	if operatorAccessCheckInterval+operatorAccessCheckTimeout+operatorStreamWriteTimeout+operatorWebSocketCloseGrace >= 10*time.Second {
		t.Fatal("access cancellation bound exceeds ten seconds")
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	rec := httptest.NewRecorder()
	w := &liveOperatorWriter{ResponseWriter: rec, ctx: ctx}
	if _, err := w.Write([]byte("before")); err != nil {
		t.Fatal(err)
	}
	cancel(errOperatorAccessChanged)
	if _, err := w.Write([]byte("private-after")); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled writer accepted new bytes", err)
	}
	w.Flush()
	if strings.Contains(rec.Body.String(), "private-after") {
		t.Fatal("protected bytes released after cancellation")
	}
}

func TestLiveSensitiveAccessClosesOnCurrentAuthorityChanges(t *testing.T) {
	for _, mode := range []string{"revoked", "narrowed", "target removed", "rotated", "expired", "cookie narrowed", "authority unavailable"} {
		t.Run(mode, func(t *testing.T) {
			authority := &memoryOperatorAuthority{document: registryFixtureDocument(t)}
			audit := testutil.NewSecurityAudit()
			s := &Server{Namespace: "kyber-system", SecurityAudit: audit, auth: registryFixtureAuthenticator(t, authority)}
			stopped := make(chan error, 1)
			handler := authMiddleware(s.auth, s.auditSensitiveRequests(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !s.requireSensitiveResource(w, r, "alpha", ScopeLogsRead) {
					return
				}
				_, _ = io.WriteString(w, "admitted\n")
				w.(http.Flusher).Flush()
				<-r.Context().Done()
				stopped <- context.Cause(r.Context())
				if _, err := io.WriteString(w, "private-after\n"); err == nil {
					t.Error("handler wrote after access cancellation")
				}
			})))
			server := httptest.NewServer(handler)
			defer server.Close()
			request, _ := http.NewRequest(http.MethodGet, server.URL+"/api/v1/agents/alpha/logs", nil)
			request.Header.Set("Authorization", "Bearer client-key-fixture")
			if mode == "cookie narrowed" {
				request.Header.Del("Authorization")
				caller := mustRegistryCaller(t, s.auth, registryBearer("client-key-fixture"))
				token := mustRegistryCookie(t, s.auth, *caller)
				request.AddCookie(&http.Cookie{Name: browserSessionCookie, Value: token})
			}

			response, err := server.Client().Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			first := make([]byte, len("admitted\n"))
			if _, err := io.ReadFull(response.Body, first); err != nil || string(first) != "admitted\n" {
				t.Fatal("stream did not start", err)
			}
			changed := time.Now()
			authority.edit(func(d *operatorcredentials.Document) {
				c := &d.Credentials[2]
				switch mode {
				case "revoked":
					*c = c.WithSecretMaterial(nil)
					c.State = "revoked"
					c.PermissionRevision++
				case "narrowed", "cookie narrowed":
					c.Scopes = []string{"metrics:read"}
					c.PermissionRevision++
				case "target removed":
					c.AgentResources = []string{"kyber-system/other"}
					c.PermissionRevision++
				case "rotated":
					*c = c.WithSecretMaterial([]byte("rotated-live-fixture"))
					c.Generation++
				case "expired":
					expired := time.Now().Add(-time.Second)
					c.ExpiresAt = &expired
					c.PermissionRevision++
				}
			})
			if mode == "authority unavailable" {
				authority.mu.Lock()
				authority.err = errors.New("private authority transport detail")
				authority.mu.Unlock()
			}
			select {
			case cause := <-stopped:
				if mode == "authority unavailable" {
					if !errors.Is(cause, errOperatorAuthorityUnavailable) {
						t.Fatal("outage was misreported as revocation", cause)
					}
				} else if !errors.Is(cause, errOperatorAccessChanged) {
					t.Fatal("access change did not cancel stream", cause)
				}
				if time.Since(changed) >= 10*time.Second {
					t.Fatal("stream retained access beyond propagation bound")
				}
			case <-time.After(10 * time.Second):
				t.Fatal("stream did not close within propagation bound")
			}
			tail, _ := io.ReadAll(response.Body)
			if strings.Contains(string(tail), "private-after") {
				t.Fatal("data escaped after cancellation")
			}
			// Wait for completion collection, without assuming concurrent inserts have
			// the same order as the handler's response teardown.
			deadline := time.Now().Add(3 * time.Second)
			for time.Now().Before(deadline) {
				records := audit.Records()
				completed := false
				for _, event := range records {
					if event.Phase == "completion" {
						completed = true
						if mode == "authority unavailable" && event.Outcome != "unknown" {
							t.Fatal("outage claimed a known denial or completion")
						}
					}
				}
				if completed {
					return
				}
				time.Sleep(10 * time.Millisecond)
			}
			t.Fatal("stream completion was not collected")
		})
	}
}

func TestLiveAccessPreservesKnownAndUnknownMutationOutcomes(t *testing.T) {
	for _, confirmed := range []bool{true, false} {
		t.Run(map[bool]string{true: "confirmed", false: "uncertain"}[confirmed], func(t *testing.T) {
			authority := &memoryOperatorAuthority{document: registryFixtureDocument(t)}
			authority.edit(func(d *operatorcredentials.Document) {
				d.Credentials[2].Scopes = append(d.Credentials[2].Scopes, "secrets:write")
			})
			audit := testutil.NewSecurityAudit()
			s := &Server{Namespace: "kyber-system", SecurityAudit: audit, auth: registryFixtureAuthenticator(t, authority)}
			handler := authMiddleware(s.auth, s.auditSensitiveRequests(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !s.requireSensitiveResource(w, r, "alpha", ScopeSecretsWrite) {
					return
				}
				if confirmed {
					markSensitiveOutcome(r, "success")
				}
				authority.edit(func(d *operatorcredentials.Document) {
					d.Credentials[2] = d.Credentials[2].WithSecretMaterial(nil)
					d.Credentials[2].State = "revoked"
					d.Credentials[2].PermissionRevision++
				})
				<-r.Context().Done()
				if !errors.Is(context.Cause(r.Context()), errOperatorAccessChanged) {
					t.Error("mutation fixture was not closed by live revocation")
				}
			})))
			request := registryBearer("client-key-fixture")
			request.URL.Path = "/api/v1/agents/alpha/secrets/TOKEN"
			request.Method = http.MethodPut
			ctx, cancel := context.WithTimeout(request.Context(), 8*time.Second)
			defer cancel()
			request = request.WithContext(ctx)
			handler.ServeHTTP(httptest.NewRecorder(), request)
			want := "unknown"
			if confirmed {
				want = "success"
			}
			completed := false
			for _, record := range audit.Records() {
				if record.Phase == "completion" {
					completed = true
					if record.Outcome != want {
						t.Fatal("mutation certainty was lost during cancellation", record.Outcome)
					}
				}
			}
			if !completed {
				t.Fatal("mutation outcome absent")
			}
		})
	}
}

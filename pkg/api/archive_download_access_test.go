package api

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/matty-v/kyber/pkg/archivejob"
	"github.com/matty-v/kyber/pkg/operatorcredentials"
)

func TestCurrentArchiveDownloadRegistryAccess(t *testing.T) {
	for _, action := range []string{"rename", "unrelated-rotation", "narrow", "target", "rotate", "revoke", "expired", "outage"} {
		t.Run(action, func(t *testing.T) {
			authority := &memoryOperatorAuthority{document: registryFixtureDocument(t)}
			authority.edit(func(d *operatorcredentials.Document) {
				d.Credentials[2].Scopes = append(d.Credentials[2].Scopes, string(ScopeArchivesAdmin))
			})
			s := &Server{Namespace: "kyber-system", auth: registryFixtureAuthenticator(t, authority), Archives: &ArchiveService{SigningKey: []byte("private-archive-fixture-key")}}
			r := registryBearer("client-key-fixture")
			caller := mustRegistryCaller(t, s.auth, r)
			r = r.WithContext(context.WithValue(r.Context(), callerCtxKey{}, caller))
			token, err := s.signCurrentArchiveDownload(r, &archivejob.Job{ID: "export-fixture", Agent: "alpha"}, time.Now().Add(time.Minute))
			if err != nil {
				t.Fatal(err)
			}
			claims, ok := s.parseCurrentArchiveDownload(token)
			if !ok {
				t.Fatal("issued token rejected")
			}
			if _, err := s.resolveArchiveDownload(context.Background(), claims); err != nil {
				t.Fatal("fresh capability rejected", err)
			}
			if _, ok := s.parseCurrentArchiveDownload(token + "x"); ok {
				t.Fatal("tampered capability accepted")
			}
			if action == "outage" {
				authority.mu.Lock()
				authority.err = errors.New("fixture authority unavailable")
				authority.mu.Unlock()
			} else {
				authority.edit(func(d *operatorcredentials.Document) {
					c := &d.Credentials[2]
					switch action {
					case "rename":
						c.Name = "Renamed archive caller"
					case "unrelated-rotation":
						d.Credentials[1] = d.Credentials[1].WithSecretMaterial([]byte("other-rotated-fixture"))
						d.Credentials[1].Generation++
					case "narrow":
						c.Scopes = []string{"logs:read"}
						c.PermissionRevision++
					case "target":
						c.AgentResources = []string{"kyber-system/other"}
						c.PermissionRevision++
					case "rotate":
						*c = c.WithSecretMaterial([]byte("archive-rotated-fixture"))
						c.Generation++
					case "expired":
						past := time.Now().Add(-time.Minute)
						c.ExpiresAt = &past
						c.PermissionRevision++
					case "revoke":
						*c = c.WithSecretMaterial(nil)
						c.State = operatorcredentials.StateRevoked
						c.PermissionRevision++
					}
				})
			}
			_, err = s.resolveArchiveDownload(context.Background(), claims)
			switch action {
			case "rename", "unrelated-rotation":
				if err != nil {
					t.Fatal("unrelated edit invalidated download", err)
				}
			case "outage":
				if !errors.Is(err, errOperatorAuthorityUnavailable) {
					t.Fatal("outage misrepresented as revocation", err)
				}
			default:
				if err == nil {
					t.Fatal("changed access retained download")
				}
			}
		})
	}
}

func TestCurrentArchiveDownloadConfigurationRotationAndPrecedence(t *testing.T) {
	a := NewAPIKeyAuthenticator("root-fixture", ScopedCaller{Name: "scoped", Key: "scoped-fixture", Scopes: []string{string(ScopeArchivesAdmin)}, AgentResources: []string{"kyber-system/alpha"}, PrincipalID: installationAdminPrincipalID, TenantID: installationTenantID, CredentialID: installationAdminCredentialID, CredentialGeneration: 1})
	s := &Server{Namespace: "kyber-system", auth: a, Archives: &ArchiveService{SigningKey: []byte("archive-private-fixture")}}
	issue := func(key string) archiveDownloadClaims {
		t.Helper()
		r := registryBearer(key)
		c := mustRegistryCaller(t, a, r)
		r = r.WithContext(context.WithValue(r.Context(), callerCtxKey{}, c))
		token, err := s.signCurrentArchiveDownload(r, &archivejob.Job{ID: "job-fixture", Agent: "alpha"}, time.Now().Add(time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		claims, ok := s.parseCurrentArchiveDownload(token)
		if !ok {
			t.Fatal("configuration link invalid")
		}
		return claims
	}
	root, scoped := issue("root-fixture"), issue("scoped-fixture")
	if root.Authority == scoped.Authority {
		t.Fatal("restricted identity fields borrowed root authority")
	}
	a.SetKey("rotated-root-fixture")
	if _, err := s.resolveArchiveDownload(context.Background(), root); err == nil {
		t.Fatal("root rotation retained old download")
	}
	if _, err := s.resolveArchiveDownload(context.Background(), scoped); err != nil {
		t.Fatal("root rotation invalidated restricted caller", err)
	}
	// A configuration capability cannot survive switching to another authority,
	// even when that authority has a record with matching identity fields.
	a.authority = &memoryOperatorAuthority{document: registryFixtureDocument(t)}
	a.namespace = "kyber-system"
	if _, err := s.resolveArchiveDownload(context.Background(), scoped); err == nil {
		t.Fatal("configuration capability crossed authority adoption")
	}
}

func TestCurrentArchiveDownloadLiveAccessOutlivesLinkAdmission(t *testing.T) {
	for _, outage := range []bool{false, true} {
		t.Run(map[bool]string{false: "revoked", true: "authority-outage"}[outage], func(t *testing.T) {
			authority := &memoryOperatorAuthority{document: registryFixtureDocument(t)}
			authority.edit(func(d *operatorcredentials.Document) {
				d.Credentials[2].Scopes = append(d.Credentials[2].Scopes, string(ScopeArchivesAdmin))
			})
			s := &Server{Namespace: "kyber-system", auth: registryFixtureAuthenticator(t, authority), Archives: &ArchiveService{SigningKey: []byte("archive-live-private-fixture")}}
			r := registryBearer("client-key-fixture")
			caller := mustRegistryCaller(t, s.auth, r)
			r = r.WithContext(context.WithValue(r.Context(), callerCtxKey{}, caller))
			token, err := s.signCurrentArchiveDownload(r, &archivejob.Job{ID: "job-live", Agent: "alpha"}, time.Now().Add(time.Minute))
			if err != nil {
				t.Fatal(err)
			}
			claims, ok := s.parseCurrentArchiveDownload(token)
			if !ok {
				t.Fatal("issued link invalid")
			}
			stopped := make(chan struct{})
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				current, err := s.resolveArchiveDownload(r.Context(), claims)
				if err != nil {
					t.Error("initial resolution failed")
					return
				}
				r = r.WithContext(context.WithValue(r.Context(), callerCtxKey{}, current))
				s.liveOperatorRequestsWithAccess(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					_, _ = io.WriteString(w, "ready\n")
					w.(http.Flusher).Flush()
					<-r.Context().Done()
					close(stopped)
				}), func(r *http.Request) (*Caller, error) { return s.resolveArchiveDownload(r.Context(), claims) }, func(_ *http.Request, c *Caller) bool {
					return c.Scopes.Has(ScopeArchivesAdmin) && c.AgentResources.Has("kyber-system/alpha")
				}).ServeHTTP(w, r)
			})
			server := httptest.NewServer(handler)
			defer server.Close()
			response, err := server.Client().Get(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			if line, err := bufio.NewReader(response.Body).ReadString('\n'); err != nil || !strings.Contains(line, "ready") {
				t.Fatal("download not established")
			}
			// Expiry is an admission limit, not a limit on a healthy in-progress transfer.
			s.Archives.Now = func() time.Time { return time.Now().Add(2 * time.Minute) }
			if _, ok := s.parseCurrentArchiveDownload(token); ok {
				t.Fatal("expired URL still admits new downloads")
			}
			select {
			case <-stopped:
				t.Fatal("healthy transfer stopped at URL expiry")
			case <-time.After(operatorAccessCheckInterval + 500*time.Millisecond):
			}
			if outage {
				authority.mu.Lock()
				authority.err = errors.New("fixture outage")
				authority.mu.Unlock()
			} else {
				authority.edit(func(d *operatorcredentials.Document) {
					d.Credentials[2].Scopes = []string{"logs:read"}
					d.Credentials[2].PermissionRevision++
				})
			}
			select {
			case <-stopped:
			case <-time.After(10 * time.Second):
				t.Fatal("archive transfer retained lost access")
			}
		})
	}
}

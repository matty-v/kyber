package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/matty-v/kyber/pkg/api"
	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/briefstore"
	"github.com/matty-v/kyber/pkg/githubapp"
)

// fakeScopedMinter records the args of the last MintScopedToken call and
// returns a canned token (or error). Satisfies api.IdentityRepoTokenMinter.
type fakeScopedMinter struct {
	gotRepos []string
	gotPerms map[string]string
	tok      *githubapp.InstallationToken
	err      error
}

func (f *fakeScopedMinter) MintScopedToken(_ context.Context, repos []string, perms map[string]string) (*githubapp.InstallationToken, error) {
	f.gotRepos = repos
	f.gotPerms = perms
	if f.err != nil {
		return nil, f.err
	}
	return f.tok, nil
}

func agentWithIdentityRepo(name, repo string) *kyberv1.Agent {
	a := &kyberv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "kyber-system"}}
	a.Spec.IdentityRepo.Repo = repo
	return a
}

// TestIdentityRepoToken_HappyPath verifies GET /internal/agents/{name}/identity-repo-token
// mints a token scoped to the agent's OWN identity repo (bare name, contents:write)
// and returns it with Cache-Control: no-store.
func TestIdentityRepoToken_HappyPath(t *testing.T) {
	scheme := mustNewScheme(t)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(agentWithIdentityRepo("dave", "matty-v/dave-agent")).Build()

	exp := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	minter := &fakeScopedMinter{tok: &githubapp.InstallationToken{Token: "ghs_scoped", ExpiresAt: exp}}

	srv := api.NewInternalServer(briefstore.NewMemoryStore(),
		api.WithKubeClient(fakeClient, "kyber-system"),
		api.WithIdentityRepoTokenMinter(minter))

	req := httptest.NewRequest(http.MethodGet, "/internal/agents/dave/identity-repo-token", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control: got %q, want no-store", got)
	}
	// Scoped to the BARE repo name (not owner/name) with contents:write.
	if len(minter.gotRepos) != 1 || minter.gotRepos[0] != "dave-agent" {
		t.Errorf("minted repos: got %v, want [dave-agent]", minter.gotRepos)
	}
	if minter.gotPerms["contents"] != "write" {
		t.Errorf("minted perms: got %v, want contents:write", minter.gotPerms)
	}
	var body struct {
		Repo      string `json:"repo"`
		Token     string `json:"token"`
		ExpiresAt string `json:"expires_at"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	if body.Token != "ghs_scoped" || body.Repo != "matty-v/dave-agent" {
		t.Errorf("body: got %+v", body)
	}
}

func TestIdentityRepoToken_MethodNotAllowed(t *testing.T) {
	scheme := mustNewScheme(t)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	minter := &fakeScopedMinter{tok: &githubapp.InstallationToken{Token: "x"}}
	srv := api.NewInternalServer(briefstore.NewMemoryStore(),
		api.WithKubeClient(fakeClient, "kyber-system"),
		api.WithIdentityRepoTokenMinter(minter))

	req := httptest.NewRequest(http.MethodPost, "/internal/agents/dave/identity-repo-token", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status: got %d, want 405", w.Code)
	}
}

// TestIdentityRepoToken_NotConfigured verifies that without a minter wired (an
// install with no Kyber App) the endpoint 503s — the agent's git helper then
// falls back to the generic PAT.
func TestIdentityRepoToken_NotConfigured(t *testing.T) {
	scheme := mustNewScheme(t)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(agentWithIdentityRepo("dave", "matty-v/dave-agent")).Build()
	srv := api.NewInternalServer(briefstore.NewMemoryStore(),
		api.WithKubeClient(fakeClient, "kyber-system"))
	req := httptest.NewRequest(http.MethodGet, "/internal/agents/dave/identity-repo-token", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status: got %d, want 503", w.Code)
	}
}

// TestIdentityRepoToken_NoIdentityRepo verifies an agent with no configured
// identity repo gets 404 and the minter is never called.
func TestIdentityRepoToken_NoIdentityRepo(t *testing.T) {
	scheme := mustNewScheme(t)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(agentWithIdentityRepo("dave", "")).Build()
	minter := &fakeScopedMinter{tok: &githubapp.InstallationToken{Token: "x"}}
	srv := api.NewInternalServer(briefstore.NewMemoryStore(),
		api.WithKubeClient(fakeClient, "kyber-system"),
		api.WithIdentityRepoTokenMinter(minter))
	req := httptest.NewRequest(http.MethodGet, "/internal/agents/dave/identity-repo-token", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404", w.Code)
	}
	if minter.gotRepos != nil {
		t.Errorf("minter should not be called with no identity repo; got %v", minter.gotRepos)
	}
}

// TestIdentityRepoToken_MintError verifies a GitHub mint failure surfaces as a
// generic 502 (no GitHub error detail leaked to the client).
func TestIdentityRepoToken_MintError(t *testing.T) {
	scheme := mustNewScheme(t)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(agentWithIdentityRepo("dave", "matty-v/dave-agent")).Build()
	minter := &fakeScopedMinter{err: errors.New("app lacks contents:write")}
	srv := api.NewInternalServer(briefstore.NewMemoryStore(),
		api.WithKubeClient(fakeClient, "kyber-system"),
		api.WithIdentityRepoTokenMinter(minter))
	req := httptest.NewRequest(http.MethodGet, "/internal/agents/dave/identity-repo-token", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status: got %d, want 502", w.Code)
	}
	// The GitHub error detail must not leak to the client.
	if strings.Contains(w.Body.String(), "contents:write") {
		t.Errorf("response leaked GitHub error detail: %q", w.Body.String())
	}
}

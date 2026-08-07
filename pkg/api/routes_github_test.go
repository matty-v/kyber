package api

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/matty-v/kyber/pkg/githubapp"
)

// repoWire is the on-the-wire shape /installation/repositories returns.
// Lives only in tests so the API package doesn't take a JSON dependency
// on the GitHub field names that pkg/githubapp already maps internally.
type repoWire struct {
	FullName      string `json:"full_name"`
	Description   string `json:"description"`
	Private       bool   `json:"private"`
	DefaultBranch string `json:"default_branch"`
	IsTemplate    bool   `json:"is_template"`
}

func newGithubTestKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating test key: %v", err)
	}
	return k
}

func writeGithubTokenResponse(w http.ResponseWriter, token string, expires time.Time) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"token":      token,
		"expires_at": expires.UTC().Format(time.RFC3339),
	})
}

// newGithubTestServer constructs a Server whose GithubAppClient is wired to
// the provided fakeGitHub httptest server. Returns the Server ready for
// handler invocation.
func newGithubTestServer(t *testing.T, ghURL string) *Server {
	t.Helper()
	c, err := githubapp.NewClient(
		githubapp.Config{AppID: 1, InstallationID: 99, PrivateKey: newGithubTestKey(t)},
		githubapp.WithBaseURL(ghURL),
	)
	if err != nil {
		t.Fatalf("githubapp.NewClient: %v", err)
	}
	return &Server{GithubAppClient: c}
}

func TestHandleGitHubRepos_SplitsTemplatesAndRepos(t *testing.T) {
	repos := []repoWire{
		{FullName: "matty-v/alice-agent", Private: true, DefaultBranch: "main"},
		{FullName: "matty-v/kyber-agent-template", Private: true, DefaultBranch: "main", IsTemplate: true},
		{FullName: "matty-v/foo", Description: "A foo repo", Private: false, DefaultBranch: "main"},
	}
	ghServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/app/installations/99/access_tokens":
			writeGithubTokenResponse(w, "ghs_test", time.Now().Add(time.Hour))
		case "/installation/repositories":
			page, _ := strconv.Atoi(r.URL.Query().Get("page"))
			if page != 1 {
				_ = json.NewEncoder(w).Encode(map[string]any{"total_count": len(repos), "repositories": []repoWire{}})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": len(repos), "repositories": repos})
		case "/installation/token":
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
	}))
	defer ghServer.Close()

	s := newGithubTestServer(t, ghServer.URL)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/github/repos", nil)
	rr := httptest.NewRecorder()
	s.handleGitHubRepos(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var got GitHubReposResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Repos) != 2 {
		t.Errorf("repos len = %d, want 2 (alice-agent + foo); got=%+v", len(got.Repos), got.Repos)
	}
	if len(got.Templates) != 1 {
		t.Fatalf("templates len = %d, want 1 (kyber-agent-template)", len(got.Templates))
	}
	if got.Templates[0].FullName != "matty-v/kyber-agent-template" {
		t.Errorf("template[0] = %+v, want kyber-agent-template", got.Templates[0])
	}
	if !got.Templates[0].IsTemplate {
		t.Error("template[0].IsTemplate should be true")
	}
}

// TestHandleGitHubRepos_CachesAcrossCalls verifies the 60s cache prevents a
// second hit to /installation/repositories within the TTL window. Without
// the cache the dropdown would paginate GitHub on every wizard re-open.
func TestHandleGitHubRepos_CachesAcrossCalls(t *testing.T) {
	var listCalls atomic.Int32
	ghServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/app/installations/99/access_tokens":
			writeGithubTokenResponse(w, "ghs_test", time.Now().Add(time.Hour))
		case "/installation/repositories":
			listCalls.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 1, "repositories": []repoWire{
				{FullName: "matty-v/foo", DefaultBranch: "main"},
			}})
		case "/installation/token":
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer ghServer.Close()

	s := newGithubTestServer(t, ghServer.URL)
	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/github/repos", nil)
		rr := httptest.NewRecorder()
		s.handleGitHubRepos(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("call %d: status = %d, want 200", i, rr.Code)
		}
	}
	if got := listCalls.Load(); got != 1 {
		t.Errorf("upstream /installation/repositories called %d times, want 1 (cache should absorb the rest)", got)
	}
}

func TestHandleGitHubRepos_503WhenAppNotConfigured(t *testing.T) {
	s := &Server{} // no GithubAppClient
	req := httptest.NewRequest(http.MethodGet, "/api/v1/github/repos", nil)
	rr := httptest.NewRecorder()
	s.handleGitHubRepos(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rr.Code)
	}
}

func TestHandleGitHubRepos_RejectsNonGet(t *testing.T) {
	ghServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer ghServer.Close()
	s := newGithubTestServer(t, ghServer.URL)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/github/repos", nil)
	rr := httptest.NewRecorder()
	s.handleGitHubRepos(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rr.Code)
	}
}

func TestHandleGitHubRepos_502OnUpstreamFailure(t *testing.T) {
	ghServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/app/installations/99/access_tokens":
			writeGithubTokenResponse(w, "ghs_test", time.Now().Add(time.Hour))
		case "/installation/repositories":
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"rate limited"}`))
		case "/installation/token":
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer ghServer.Close()
	s := newGithubTestServer(t, ghServer.URL)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/github/repos", nil)
	rr := httptest.NewRecorder()
	s.handleGitHubRepos(rr, req)
	if rr.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502; body=%s", rr.Code, rr.Body.String())
	}
}

func TestHandleGitHubRepoExists_TrueOn200(t *testing.T) {
	ghServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/app/installations/99/access_tokens":
			writeGithubTokenResponse(w, "ghs_test", time.Now().Add(time.Hour))
		case "/repos/matty-v/alice-agent":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"full_name":"matty-v/alice-agent"}`))
		case "/installation/token":
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
	}))
	defer ghServer.Close()
	s := newGithubTestServer(t, ghServer.URL)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/github/repos/matty-v/alice-agent/exists", nil)
	rr := httptest.NewRecorder()
	s.handleGitHubReposPath(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var got GitHubRepoExistsResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !got.Exists {
		t.Error("Exists = false, want true on 200")
	}
}

func TestHandleGitHubRepoExists_FalseOn404(t *testing.T) {
	ghServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/app/installations/99/access_tokens":
			writeGithubTokenResponse(w, "ghs_test", time.Now().Add(time.Hour))
		case "/repos/matty-v/never-existed":
			w.WriteHeader(http.StatusNotFound)
		case "/installation/token":
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer ghServer.Close()
	s := newGithubTestServer(t, ghServer.URL)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/github/repos/matty-v/never-existed/exists", nil)
	rr := httptest.NewRecorder()
	s.handleGitHubReposPath(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var got GitHubRepoExistsResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &got)
	if got.Exists {
		t.Error("Exists = true, want false on 404")
	}
}

func TestHandleGitHubRepoExists_RejectsBadName(t *testing.T) {
	ghServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Should not be reached — request fails validation before we hit
		// the upstream.
		t.Fatalf("upstream should not be called for invalid name; path=%q", r.URL.Path)
	}))
	defer ghServer.Close()
	s := newGithubTestServer(t, ghServer.URL)

	// Names that fail the regex `^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`:
	// leading hyphen, leading dot, slash injection (path-encoded), too long.
	for _, badName := range []string{
		"-leading-hyphen",
		".leading-dot",
		"with%20space",
		"a..b%2Fc%2Fetc%2Fpasswd",
	} {
		req := httptest.NewRequest(http.MethodGet,
			fmt.Sprintf("/api/v1/github/repos/matty-v/%s/exists", badName), nil)
		rr := httptest.NewRecorder()
		s.handleGitHubReposPath(rr, req)
		if rr.Code != http.StatusBadRequest && rr.Code != http.StatusNotFound {
			t.Errorf("name=%q: status = %d, want 400 or 404; body=%s",
				badName, rr.Code, rr.Body.String())
		}
	}
}

func TestHandleGitHubRepoExists_503WhenAppNotConfigured(t *testing.T) {
	s := &Server{}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/github/repos/matty-v/x/exists", nil)
	rr := httptest.NewRecorder()
	s.handleGitHubReposPath(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rr.Code)
	}
}

func TestHandleGitHubRepoExists_404OnUnknownSubpath(t *testing.T) {
	ghServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer ghServer.Close()
	s := newGithubTestServer(t, ghServer.URL)
	// Subtree handler with no /exists suffix and not the bare list endpoint.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/github/repos/matty-v/foo", nil)
	rr := httptest.NewRecorder()
	s.handleGitHubReposPath(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404; body=%s", rr.Code, rr.Body.String())
	}
}

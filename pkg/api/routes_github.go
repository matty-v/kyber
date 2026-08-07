package api

import (
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/matty-v/kyber/pkg/githubapp"
)

// Cache TTL for the /installation/repositories result. Short enough that
// freshly-installed repos appear quickly in the dropdown, long enough that
// re-opening the wizard mid-session doesn't paginate GitHub on every click.
const githubReposCacheTTL = 60 * time.Second

// GitHubRepoPayload is the per-repo shape returned by GET /api/v1/github/repos.
// Mirrors githubapp.Repository — duplicated here so the API surface is stable
// even if the underlying client struct gains fields.
type GitHubRepoPayload struct {
	FullName      string `json:"fullName"`
	Description   string `json:"description"`
	Private       bool   `json:"private"`
	DefaultBranch string `json:"defaultBranch"`
	IsTemplate    bool   `json:"isTemplate"`
}

// GitHubReposResponse is the body of GET /api/v1/github/repos. Two parallel
// lists keep the PWA simple: the dropdown shows existing repos, the
// "create new from template" form shows templates.
type GitHubReposResponse struct {
	Repos     []GitHubRepoPayload `json:"repos"`
	Templates []GitHubRepoPayload `json:"templates"`
}

// GitHubRepoExistsResponse is the body of GET /api/v1/github/repos/{owner}/{name}/exists.
// Returns true when the repo exists under the App installation, false on 404.
type GitHubRepoExistsResponse struct {
	Exists bool `json:"exists"`
}

// githubOwnerRepoRe validates a single path segment against GitHub's naming
// rules. Repository names allow alphanumerics, hyphens, dots, underscores;
// owners are stricter (no dots/underscores) but in practice the App-installed
// owner doesn't pass through this path — collision-check is owner-scoped to
// IdentityRepoOwner. Use the looser pattern for both to keep the regex simple.
var githubOwnerRepoRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)

// handleGitHubRepos serves GET /api/v1/github/repos.
//
// Returns the list of repos the Kyber GitHub App has access to, split into
// existing repos (identity repos a user might pick) and templates (for the
// create-new-from-template flow). Cached for githubReposCacheTTL.
func (s *Server) handleGitHubRepos(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED",
			"only GET is supported on /api/v1/github/repos")
		return
	}
	if s.GithubAppClient == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "GITHUB_APP_NOT_CONFIGURED",
			"GitHub App is not configured on this control plane")
		return
	}

	repos, err := s.cachedListRepositories(r)
	if err != nil {
		slog.Error("github: list installation repositories failed", "error", err)
		writeJSONError(w, http.StatusBadGateway, "GITHUB_LIST_FAILED",
			"failed to list GitHub repositories")
		return
	}

	resp := GitHubReposResponse{
		Repos:     make([]GitHubRepoPayload, 0, len(repos)),
		Templates: make([]GitHubRepoPayload, 0),
	}
	for _, rp := range repos {
		payload := GitHubRepoPayload{
			FullName:      rp.FullName,
			Description:   rp.Description,
			Private:       rp.Private,
			DefaultBranch: rp.DefaultBranch,
			IsTemplate:    rp.IsTemplate,
		}
		if rp.IsTemplate {
			resp.Templates = append(resp.Templates, payload)
		} else {
			resp.Repos = append(resp.Repos, payload)
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// cachedListRepositories returns the cached repo list when fresh, otherwise
// fetches from GitHub. The lock is held across the round-trip on a cache
// miss — this is a single-flight choice. Wizard sessions don't open the
// dropdown so often that lock contention matters; if it ever does, swap in
// golang.org/x/sync/singleflight.
func (s *Server) cachedListRepositories(r *http.Request) ([]githubapp.Repository, error) {
	s.githubReposCacheMu.Lock()
	defer s.githubReposCacheMu.Unlock()
	if time.Since(s.githubReposCacheAt) < githubReposCacheTTL && s.githubReposCache != nil {
		return s.githubReposCache, nil
	}
	repos, err := s.GithubAppClient.ListInstallationRepositories(r.Context())
	if err != nil {
		return nil, err
	}
	s.githubReposCache = repos
	s.githubReposCacheAt = time.Now()
	return repos, nil
}

// handleGitHubRepoExists serves GET /api/v1/github/repos/{owner}/{name}/exists.
//
// The PWA's create-new-from-template flow polls this while the user types the
// new repo name; a 200 with {"exists":true} disables the Continue gate.
func (s *Server) handleGitHubRepoExists(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED",
			"only GET is supported on /api/v1/github/repos/{owner}/{name}/exists")
		return
	}
	if s.GithubAppClient == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "GITHUB_APP_NOT_CONFIGURED",
			"GitHub App is not configured on this control plane")
		return
	}

	// Path: /api/v1/github/repos/{owner}/{name}/exists
	suffix := strings.TrimPrefix(r.URL.Path, "/api/v1/github/repos/")
	suffix = strings.TrimSuffix(suffix, "/exists")
	parts := strings.Split(suffix, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		writeJSONError(w, http.StatusNotFound, "not_found", "unknown github route")
		return
	}
	owner, name := parts[0], parts[1]
	if !githubOwnerRepoRe.MatchString(owner) {
		writeJSONErrorWithField(w, http.StatusBadRequest, "INVALID_OWNER",
			"invalid github owner", "owner")
		return
	}
	if !githubOwnerRepoRe.MatchString(name) {
		writeJSONErrorWithField(w, http.StatusBadRequest, "INVALID_REPO_NAME",
			"invalid github repo name", "name")
		return
	}

	exists, err := s.GithubAppClient.RepoExists(r.Context(), owner, name)
	if err != nil {
		slog.Error("github: repo exists check failed",
			"owner", owner, "name", name, "error", err)
		writeJSONError(w, http.StatusBadGateway, "GITHUB_EXISTS_FAILED",
			"failed to check GitHub repository")
		return
	}
	writeJSON(w, http.StatusOK, GitHubRepoExistsResponse{Exists: exists})
}

// handleGitHubReposPath dispatches the catch-all subtree under
// /api/v1/github/repos/. The collection root is registered separately on
// the bare path so the mux's longest-prefix match works.
func (s *Server) handleGitHubReposPath(w http.ResponseWriter, r *http.Request) {
	// /api/v1/github/repos/{owner}/{name}/exists is the only subtree route.
	if strings.HasSuffix(r.URL.Path, "/exists") {
		s.handleGitHubRepoExists(w, r)
		return
	}
	writeJSONError(w, http.StatusNotFound, "not_found", "unknown github route")
}

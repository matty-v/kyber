package githubapp_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/matty-v/kyber/pkg/githubapp"
)

// repoPayload mirrors the on-the-wire JSON GitHub returns for a repo
// inside /installation/repositories. Only the fields ListInstallationRepositories
// reads are populated.
type repoPayload struct {
	FullName      string `json:"full_name"`
	Description   string `json:"description"`
	Private       bool   `json:"private"`
	DefaultBranch string `json:"default_branch"`
	IsTemplate    bool   `json:"is_template"`
}

func TestListInstallationRepositories_SinglePage(t *testing.T) {
	key := newTestKey(t)
	repos := []repoPayload{
		{FullName: "matty-v/alice-agent", Private: true, DefaultBranch: "main"},
		{FullName: "matty-v/kyber-agent-template", Private: true, DefaultBranch: "main", IsTemplate: true},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/app/installations/99/access_tokens":
			writeTokenResponse(w, "ghs_test", time.Now().Add(time.Hour))
		case "/installation/repositories":
			page, _ := strconv.Atoi(r.URL.Query().Get("page"))
			if page != 1 {
				// Second page is empty — pagination short-circuits.
				_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 2, "repositories": []repoPayload{}})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 2, "repositories": repos})
		case "/installation/token":
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
	}))
	defer srv.Close()

	c, err := githubapp.NewClient(
		githubapp.Config{AppID: 1, InstallationID: 99, PrivateKey: key},
		githubapp.WithBaseURL(srv.URL),
	)
	if err != nil {
		t.Fatal(err)
	}

	got, err := c.ListInstallationRepositories(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 repos, got %d", len(got))
	}
	if got[0].FullName != "matty-v/alice-agent" {
		t.Errorf("repo[0]: %+v", got[0])
	}
	if !got[1].IsTemplate {
		t.Errorf("repo[1] should be marked IsTemplate, got %+v", got[1])
	}
}

func TestListInstallationRepositories_MultiPage(t *testing.T) {
	key := newTestKey(t)

	// 100 + 100 + 30 = 230 across three pages. The 30-result final page
	// is below the per_page limit so pagination stops naturally.
	pageReposCount := map[int]int{1: 100, 2: 100, 3: 30}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/app/installations/99/access_tokens":
			writeTokenResponse(w, "ghs_test", time.Now().Add(time.Hour))
		case "/installation/repositories":
			page, _ := strconv.Atoi(r.URL.Query().Get("page"))
			n := pageReposCount[page]
			repos := make([]repoPayload, n)
			for i := 0; i < n; i++ {
				repos[i] = repoPayload{FullName: fmt.Sprintf("matty-v/repo-%d-%d", page, i), Private: true}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 230, "repositories": repos})
		case "/installation/token":
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
	}))
	defer srv.Close()

	c, _ := githubapp.NewClient(
		githubapp.Config{AppID: 1, InstallationID: 99, PrivateKey: key},
		githubapp.WithBaseURL(srv.URL),
	)
	got, err := c.ListInstallationRepositories(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 230 {
		t.Fatalf("expected 230 repos across pages, got %d", len(got))
	}
}

func TestListInstallationRepositories_HTTPError(t *testing.T) {
	key := newTestKey(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/app/installations/99/access_tokens":
			writeTokenResponse(w, "ghs_test", time.Now().Add(time.Hour))
		case "/installation/repositories":
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"rate limited"}`))
		case "/installation/token":
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()

	c, _ := githubapp.NewClient(
		githubapp.Config{AppID: 1, InstallationID: 99, PrivateKey: key},
		githubapp.WithBaseURL(srv.URL),
	)
	_, err := c.ListInstallationRepositories(context.Background())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestRepoExists_Found(t *testing.T) {
	key := newTestKey(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/app/installations/99/access_tokens":
			writeTokenResponse(w, "ghs_test", time.Now().Add(time.Hour))
		case "/repos/matty-v/alice-agent":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"full_name":"matty-v/alice-agent"}`))
		case "/installation/token":
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
	}))
	defer srv.Close()

	c, _ := githubapp.NewClient(
		githubapp.Config{AppID: 1, InstallationID: 99, PrivateKey: key},
		githubapp.WithBaseURL(srv.URL),
	)
	exists, err := c.RepoExists(context.Background(), "matty-v", "alice-agent")
	if err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Fatal("expected exists=true on 200")
	}
}

func TestRepoExists_NotFound(t *testing.T) {
	key := newTestKey(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/app/installations/99/access_tokens":
			writeTokenResponse(w, "ghs_test", time.Now().Add(time.Hour))
		case "/repos/matty-v/never-existed":
			w.WriteHeader(http.StatusNotFound)
		case "/installation/token":
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()

	c, _ := githubapp.NewClient(
		githubapp.Config{AppID: 1, InstallationID: 99, PrivateKey: key},
		githubapp.WithBaseURL(srv.URL),
	)
	exists, err := c.RepoExists(context.Background(), "matty-v", "never-existed")
	if err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("expected exists=false on 404")
	}
}

func TestRepoExists_HTTPError(t *testing.T) {
	key := newTestKey(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/app/installations/99/access_tokens":
			writeTokenResponse(w, "ghs_test", time.Now().Add(time.Hour))
		case "/repos/matty-v/blocked":
			w.WriteHeader(http.StatusForbidden)
		case "/installation/token":
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()

	c, _ := githubapp.NewClient(
		githubapp.Config{AppID: 1, InstallationID: 99, PrivateKey: key},
		githubapp.WithBaseURL(srv.URL),
	)
	_, err := c.RepoExists(context.Background(), "matty-v", "blocked")
	if err == nil {
		t.Fatal("expected error on 403")
	}
}

func TestRepoExists_RequiresOwnerAndRepo(t *testing.T) {
	key := newTestKey(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	c, _ := githubapp.NewClient(
		githubapp.Config{AppID: 1, InstallationID: 99, PrivateKey: key},
		githubapp.WithBaseURL(srv.URL),
	)
	for _, tc := range []struct{ owner, repo string }{
		{"", "alice"},
		{"matty-v", ""},
		{"", ""},
	} {
		_, err := c.RepoExists(context.Background(), tc.owner, tc.repo)
		if err == nil {
			t.Errorf("expected error for owner=%q repo=%q", tc.owner, tc.repo)
		}
	}
}

package githubapp_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/matty-v/kyber/pkg/githubapp"
)

// scaffoldServer is a minimal fake GitHub API for CreateFromTemplate tests.
// It tracks which requests were made so tests can assert without extra fields.
type scaffoldServer struct {
	t *testing.T

	// Config
	templateOwner string
	templateRepo  string
	newOwner      string
	newRepo       string
	defaultBranch string
	files         []fakeFile // files in the generated repo

	// generate response
	generateStatus int
	generateBody   string // raw body for non-201/non-422 responses

	// treeEmptyResponses is the number of initial GET /git/trees calls that
	// should return HTTP 409 "Git Repository is empty" before the real tree
	// is served. Simulates the async delay between POST /generate returning
	// and GitHub actually populating the tree.
	treeEmptyResponses int

	// Observed
	generateCalled bool
	putPaths       []string // paths where PUT /contents was called
	treeCalls      int      // number of times the tree endpoint was hit
}

type fakeFile struct {
	path    string
	content string // raw UTF-8 (will be base64-encoded for the API)
	isBinary bool   // if true, the file is skipped in the tree (reported as non-blob)
}

func (s *scaffoldServer) handler() http.Handler {
	mux := http.NewServeMux()

	generatePath := fmt.Sprintf("/repos/%s/%s/generate", s.templateOwner, s.templateRepo)
	mux.HandleFunc(generatePath, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		s.generateCalled = true
		if s.generateStatus == 0 {
			s.generateStatus = http.StatusCreated
		}
		switch s.generateStatus {
		case http.StatusCreated:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"full_name":"%s/%s","default_branch":"%s"}`,
				s.newOwner, s.newRepo, s.defaultBranch)
		case http.StatusNotFound:
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprintf(w, `{"message":"Not Found"}`)
		case http.StatusUnprocessableEntity:
			w.WriteHeader(http.StatusUnprocessableEntity)
			if s.generateBody != "" {
				fmt.Fprint(w, s.generateBody)
			} else {
				fmt.Fprintf(w, `{"message":"Repository creation failed: Name already exists on this account"}`)
			}
		default:
			w.WriteHeader(s.generateStatus)
			fmt.Fprint(w, s.generateBody)
		}
	})

	repoPath := fmt.Sprintf("/repos/%s/%s", s.newOwner, s.newRepo)
	mux.HandleFunc(repoPath, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{"full_name":"%s/%s","default_branch":"%s"}`,
			s.newOwner, s.newRepo, s.defaultBranch)
	})

	treePath := fmt.Sprintf("/repos/%s/%s/git/trees/%s", s.newOwner, s.newRepo, s.defaultBranch)
	mux.HandleFunc(treePath, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		s.treeCalls++
		if s.treeEmptyResponses > 0 {
			s.treeEmptyResponses--
			w.WriteHeader(http.StatusConflict)
			fmt.Fprint(w, `{"message":"Git Repository is empty."}`)
			return
		}
		type item struct {
			Path string `json:"path"`
			Type string `json:"type"`
			SHA  string `json:"sha"`
		}
		items := make([]item, 0, len(s.files))
		for i, f := range s.files {
			typ := "blob"
			if f.isBinary {
				// Report binary files as tree nodes to test they're skipped
				// even if they sneak through as blobs with binary extensions.
				typ = "blob" // test isBinaryPath skip, not tree-type skip
			}
			items = append(items, item{
				Path: f.path,
				Type: typ,
				SHA:  fmt.Sprintf("sha%d", i),
			})
		}
		out, _ := json.Marshal(map[string]any{
			"sha":  "treesha",
			"tree": items,
		})
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(out)
	})

	contentsPrefix := fmt.Sprintf("/repos/%s/%s/contents/", s.newOwner, s.newRepo)
	mux.HandleFunc(contentsPrefix, func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, contentsPrefix)

		var file *fakeFile
		for i := range s.files {
			if s.files[i].path == path {
				file = &s.files[i]
				break
			}
		}
		if file == nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}

		switch r.Method {
		case http.MethodGet:
			encoded := base64.StdEncoding.EncodeToString([]byte(file.content))
			out, _ := json.Marshal(map[string]any{
				"content":  encoded,
				"sha":      "blobsha-" + path,
				"encoding": "base64",
			})
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write(out)
		case http.MethodPut:
			s.putPaths = append(s.putPaths, path)
			// Decode the new content and store it back so idempotency tests work.
			var body struct {
				Content string `json:"content"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			decoded, _ := base64.StdEncoding.DecodeString(body.Content)
			file.content = string(decoded)

			out, _ := json.Marshal(map[string]any{
				"content": map[string]any{"path": path},
				"commit":  map[string]any{"sha": "commitsha"},
			})
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write(out)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})

	return mux
}

func newScaffoldClient(t *testing.T, srv *httptest.Server) *githubapp.Client {
	t.Helper()
	key := newTestKey(t)
	c, err := githubapp.NewClient(
		githubapp.Config{AppID: 1, InstallationID: 1, PrivateKey: key},
		githubapp.WithBaseURL(srv.URL),
	)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
}

func TestCreateFromTemplate_HappyPath(t *testing.T) {
	s := &scaffoldServer{
		templateOwner: "matty-v",
		templateRepo:  "kyber-agent-template",
		newOwner:      "matty-v",
		newRepo:       "newbot-agent",
		defaultBranch: "main",
		files: []fakeFile{
			{path: "CLAUDE.md", content: "# {{ .AgentName }}\n{{ .Description }}\n"},
			{path: "IDENTITY.md", content: "Identity for {{ .AgentName }}."},
			{path: "README.md", content: "No placeholders here."},
		},
	}
	srv := httptest.NewServer(s.handler())
	defer srv.Close()

	c := newScaffoldClient(t, srv)
	fullName, err := c.CreateFromTemplate(
		context.Background(),
		"ghs_faketoken",
		"matty-v", "kyber-agent-template",
		"matty-v", "newbot-agent",
		githubapp.ScaffoldParams{AgentName: "newbot", Description: "My new bot"},
	)
	if err != nil {
		t.Fatalf("CreateFromTemplate: %v", err)
	}
	if fullName != "matty-v/newbot-agent" {
		t.Errorf("fullName: got %q, want matty-v/newbot-agent", fullName)
	}
	if !s.generateCalled {
		t.Error("generate endpoint not called")
	}
	// README.md has no placeholders → no PUT. CLAUDE.md and IDENTITY.md do → 2 PUTs.
	if len(s.putPaths) != 2 {
		t.Errorf("PUT paths: got %v, want exactly 2 (files with placeholders)", s.putPaths)
	}
	// Verify substitution happened.
	for _, f := range s.files {
		if strings.Contains(f.content, "{{ .") {
			t.Errorf("file %s still has unsubstituted placeholder: %q", f.path, f.content)
		}
	}
}

func TestCreateFromTemplate_TemplateMissing(t *testing.T) {
	s := &scaffoldServer{
		templateOwner:  "matty-v",
		templateRepo:   "kyber-agent-template",
		newOwner:       "matty-v",
		newRepo:        "newbot-agent",
		defaultBranch:  "main",
		generateStatus: http.StatusNotFound,
	}
	srv := httptest.NewServer(s.handler())
	defer srv.Close()

	c := newScaffoldClient(t, srv)
	_, err := c.CreateFromTemplate(
		context.Background(),
		"ghs_faketoken",
		"matty-v", "kyber-agent-template",
		"matty-v", "newbot-agent",
		githubapp.ScaffoldParams{AgentName: "newbot", Description: ""},
	)
	if err == nil {
		t.Fatal("expected error for missing template, got nil")
	}
	if !strings.Contains(err.Error(), "template not found") {
		t.Errorf("error should mention template not found: %v", err)
	}
}

func TestCreateFromTemplate_RepoAlreadyExists_ContinuesToSubstitution(t *testing.T) {
	// 422 "Name already exists" → not an error; proceeds to substitution.
	s := &scaffoldServer{
		templateOwner:  "matty-v",
		templateRepo:   "kyber-agent-template",
		newOwner:       "matty-v",
		newRepo:        "newbot-agent",
		defaultBranch:  "main",
		generateStatus: http.StatusUnprocessableEntity,
		generateBody:   `{"message":"Repository creation failed: Name already exists on this account"}`,
		files: []fakeFile{
			{path: "CLAUDE.md", content: "# {{ .AgentName }}\n"},
		},
	}
	srv := httptest.NewServer(s.handler())
	defer srv.Close()

	c := newScaffoldClient(t, srv)
	fullName, err := c.CreateFromTemplate(
		context.Background(),
		"ghs_faketoken",
		"matty-v", "kyber-agent-template",
		"matty-v", "newbot-agent",
		githubapp.ScaffoldParams{AgentName: "newbot", Description: "retry test"},
	)
	if err != nil {
		t.Fatalf("CreateFromTemplate: %v (expected idempotent success)", err)
	}
	if fullName != "matty-v/newbot-agent" {
		t.Errorf("fullName: got %q, want matty-v/newbot-agent", fullName)
	}
	// Substitution should still run.
	if len(s.putPaths) != 1 {
		t.Errorf("PUT paths: got %v, want 1", s.putPaths)
	}
}

func TestCreateFromTemplate_SubstitutionIdempotency(t *testing.T) {
	// Files that have already been substituted (no {{ . }} left) must not trigger PUTs.
	s := &scaffoldServer{
		templateOwner: "matty-v",
		templateRepo:  "kyber-agent-template",
		newOwner:      "matty-v",
		newRepo:       "newbot-agent",
		defaultBranch: "main",
		files: []fakeFile{
			// Already substituted — no placeholders remain.
			{path: "CLAUDE.md", content: "# newbot\nMy new bot\n"},
			// Also no placeholders.
			{path: "README.md", content: "No placeholders here."},
		},
	}
	srv := httptest.NewServer(s.handler())
	defer srv.Close()

	c := newScaffoldClient(t, srv)
	_, err := c.CreateFromTemplate(
		context.Background(),
		"ghs_faketoken",
		"matty-v", "kyber-agent-template",
		"matty-v", "newbot-agent",
		githubapp.ScaffoldParams{AgentName: "newbot", Description: "My new bot"},
	)
	if err != nil {
		t.Fatalf("CreateFromTemplate: %v", err)
	}
	if len(s.putPaths) != 0 {
		t.Errorf("PUT paths: got %v, want none (files already substituted)", s.putPaths)
	}
}

// TestCreateFromTemplate_EmptyRepoRetriedUntilPopulated covers GitHub's
// async template generation: POST /generate returns 201 + GET /repos
// returns 200 before the tree is populated, so GET /git/trees returns
// 409 "Git Repository is empty". listTree must retry rather than bail.
func TestCreateFromTemplate_EmptyRepoRetriedUntilPopulated(t *testing.T) {
	s := &scaffoldServer{
		templateOwner: "matty-v",
		templateRepo:  "kyber-agent-template",
		newOwner:      "matty-v",
		newRepo:       "newbot-agent",
		defaultBranch: "main",
		// Simulate the real-world race: first 3 tree calls land 409, then
		// GitHub finishes populating and the 4th succeeds.
		treeEmptyResponses: 3,
		files: []fakeFile{
			{path: "CLAUDE.md", content: "# {{ .AgentName }}\n"},
		},
	}
	srv := httptest.NewServer(s.handler())
	defer srv.Close()

	c := newScaffoldClient(t, srv)
	full, err := c.CreateFromTemplate(
		context.Background(),
		"ghs_faketoken",
		"matty-v", "kyber-agent-template",
		"matty-v", "newbot-agent",
		githubapp.ScaffoldParams{AgentName: "newbot", Description: "retry test"},
	)
	if err != nil {
		t.Fatalf("CreateFromTemplate: %v", err)
	}
	if full != "matty-v/newbot-agent" {
		t.Errorf("fullName: got %q, want matty-v/newbot-agent", full)
	}
	if s.treeCalls < 4 {
		t.Errorf("treeCalls: got %d, want >= 4 (3 empty + 1 populated)", s.treeCalls)
	}
	if len(s.putPaths) != 1 || s.putPaths[0] != "CLAUDE.md" {
		t.Errorf("PUT paths: got %v, want [CLAUDE.md]", s.putPaths)
	}
}

func TestCreateFromTemplate_BinaryFileSkipped(t *testing.T) {
	s := &scaffoldServer{
		templateOwner: "matty-v",
		templateRepo:  "kyber-agent-template",
		newOwner:      "matty-v",
		newRepo:       "newbot-agent",
		defaultBranch: "main",
		files: []fakeFile{
			{path: "CLAUDE.md", content: "# {{ .AgentName }}\n"},
			// Binary extensions — isBinaryPath should return true.
			{path: "avatar.png", content: "binarydata"},
			{path: "doc.pdf", content: "binarydata"},
		},
	}
	srv := httptest.NewServer(s.handler())
	defer srv.Close()

	c := newScaffoldClient(t, srv)
	_, err := c.CreateFromTemplate(
		context.Background(),
		"ghs_faketoken",
		"matty-v", "kyber-agent-template",
		"matty-v", "newbot-agent",
		githubapp.ScaffoldParams{AgentName: "newbot", Description: "binary test"},
	)
	if err != nil {
		t.Fatalf("CreateFromTemplate: %v", err)
	}
	// Only CLAUDE.md should have been PUT (binary files skipped).
	if len(s.putPaths) != 1 || s.putPaths[0] != "CLAUDE.md" {
		t.Errorf("PUT paths: got %v, want [CLAUDE.md]", s.putPaths)
	}
}

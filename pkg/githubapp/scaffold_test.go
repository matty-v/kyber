package githubapp_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/matty-v/kyber/pkg/githubapp"
)

// scaffoldServer is a minimal fake GitHub for CreateFromTemplate tests. It
// models the generated repo as real git objects (blobs, trees, commits, one
// branch) behind the Git Data API, and also serves the Contents API, where —
// as on GitHub — every PUT is a commit of its own. Tests assert on what the
// repo looks like afterwards and on how many commits it took to get there.
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
	// blobMissingResponses is the number of initial GET /git/blobs calls that
	// answer 404: a freshly generated repo serves its tree before every
	// replica can serve its blobs.
	blobMissingResponses int

	// Observed
	generateCalled bool
	treeCalls      int      // number of times the tree endpoint was hit
	blobCreates    int      // POST /git/blobs
	commits        []string // messages of every commit made, by any API
	refUpdates     int      // PATCH /git/refs

	mu        sync.Mutex
	init      bool
	head      string            // commit SHA the branch points at
	commitsBy map[string]commit // commit SHA → commit
	trees     map[string]map[string]entry
	blobs     map[string]string // blob SHA → content
	seq       int
}

type fakeFile struct {
	path    string
	content string // raw UTF-8 (will be base64-encoded for the API)
	mode    string // defaults to 100644
}

type commit struct{ tree, parent string }

type entry struct{ mode, sha string }

func (s *scaffoldServer) newSHA(kind string) string {
	s.seq++
	return fmt.Sprintf("%s%04d", kind, s.seq)
}

// setup seeds the repo with one commit holding s.files.
func (s *scaffoldServer) setup() {
	if s.init {
		return
	}
	s.init = true
	s.commitsBy = map[string]commit{}
	s.trees = map[string]map[string]entry{}
	s.blobs = map[string]string{}
	root := map[string]entry{}
	for _, f := range s.files {
		sha := s.newSHA("blob")
		s.blobs[sha] = f.content
		mode := f.mode
		if mode == "" {
			mode = "100644"
		}
		root[f.path] = entry{mode, sha}
	}
	treeSHA := s.newSHA("tree")
	s.trees[treeSHA] = root
	s.head = s.newSHA("commit")
	s.commitsBy[s.head] = commit{tree: treeSHA}
}

// fileAtHead returns a file's content in the branch's current tree.
func (s *scaffoldServer) fileAtHead(path string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.setup()
	e, ok := s.trees[s.commitsBy[s.head].tree][path]
	if !ok {
		return "", false
	}
	return s.blobs[e.sha], true
}

func (s *scaffoldServer) modeAtHead(path string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.trees[s.commitsBy[s.head].tree][path].mode
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
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

	git := repoPath + "/git/"
	mux.HandleFunc(git, func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.setup()
		rest := strings.TrimPrefix(r.URL.Path, git)
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(rest, "trees/"):
			s.treeCalls++
			if s.treeEmptyResponses > 0 {
				s.treeEmptyResponses--
				writeJSON(w, http.StatusConflict, map[string]string{"message": "Git Repository is empty."})
				return
			}
			ref := strings.TrimPrefix(rest, "trees/")
			if ref == s.defaultBranch {
				ref = s.head
			}
			c, ok := s.commitsBy[ref]
			if !ok {
				writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
				return
			}
			paths := make([]string, 0, len(s.trees[c.tree]))
			for p := range s.trees[c.tree] {
				paths = append(paths, p)
			}
			sort.Strings(paths)
			items := []map[string]string{}
			for _, p := range paths {
				e := s.trees[c.tree][p]
				items = append(items, map[string]string{"path": p, "mode": e.mode, "type": "blob", "sha": e.sha})
			}
			writeJSON(w, http.StatusOK, map[string]any{"sha": c.tree, "tree": items})

		case r.Method == http.MethodGet && rest == "ref/heads/"+s.defaultBranch:
			writeJSON(w, http.StatusOK, map[string]any{"object": map[string]string{"sha": s.head, "type": "commit"}})

		case r.Method == http.MethodGet && strings.HasPrefix(rest, "blobs/") && s.blobMissingResponses > 0:
			s.blobMissingResponses--
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		case r.Method == http.MethodGet && strings.HasPrefix(rest, "blobs/"):
			content, ok := s.blobs[strings.TrimPrefix(rest, "blobs/")]
			if !ok {
				writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
				return
			}
			// GitHub wraps base64 at 60 columns; make sure the client copes.
			enc := base64.StdEncoding.EncodeToString([]byte(content))
			var wrapped strings.Builder
			for len(enc) > 60 {
				wrapped.WriteString(enc[:60] + "\n")
				enc = enc[60:]
			}
			wrapped.WriteString(enc)
			writeJSON(w, http.StatusOK, map[string]string{"content": wrapped.String(), "encoding": "base64"})

		case r.Method == http.MethodPost && rest == "blobs":
			var in struct{ Content, Encoding string }
			_ = json.NewDecoder(r.Body).Decode(&in)
			raw, err := base64.StdEncoding.DecodeString(in.Content)
			if in.Encoding != "base64" || err != nil {
				writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "bad blob"})
				return
			}
			s.blobCreates++
			sha := s.newSHA("blob")
			s.blobs[sha] = string(raw)
			writeJSON(w, http.StatusCreated, map[string]string{"sha": sha})

		case r.Method == http.MethodPost && rest == "trees":
			var in struct {
				BaseTree string `json:"base_tree"`
				Tree     []struct{ Path, Mode, Type, SHA string }
			}
			_ = json.NewDecoder(r.Body).Decode(&in)
			base, ok := s.trees[in.BaseTree]
			if !ok {
				writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "base_tree not found"})
				return
			}
			next := map[string]entry{}
			for p, e := range base {
				next[p] = e
			}
			for _, e := range in.Tree {
				if _, ok := s.blobs[e.SHA]; !ok || e.Type != "blob" || e.Mode == "" {
					writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "bad tree entry " + e.Path})
					return
				}
				next[e.Path] = entry{e.Mode, e.SHA}
			}
			sha := s.newSHA("tree")
			s.trees[sha] = next
			writeJSON(w, http.StatusCreated, map[string]string{"sha": sha})

		case r.Method == http.MethodPost && rest == "commits":
			var in struct {
				Message string
				Tree    string
				Parents []string
			}
			_ = json.NewDecoder(r.Body).Decode(&in)
			if _, ok := s.trees[in.Tree]; !ok || len(in.Parents) != 1 {
				writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "bad commit"})
				return
			}
			s.commits = append(s.commits, in.Message)
			sha := s.newSHA("commit")
			s.commitsBy[sha] = commit{tree: in.Tree, parent: in.Parents[0]}
			writeJSON(w, http.StatusCreated, map[string]string{"sha": sha})

		case r.Method == http.MethodPatch && rest == "refs/heads/"+s.defaultBranch:
			var in struct {
				SHA   string
				Force bool
			}
			_ = json.NewDecoder(r.Body).Decode(&in)
			c, ok := s.commitsBy[in.SHA]
			if !ok || (!in.Force && c.parent != s.head) {
				writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "Update is not a fast forward"})
				return
			}
			s.refUpdates++
			s.head = in.SHA
			writeJSON(w, http.StatusOK, map[string]any{"object": map[string]string{"sha": s.head}})

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	// The Contents API: each PUT is a commit on the branch.
	contentsPrefix := repoPath + "/contents/"
	mux.HandleFunc(contentsPrefix, func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.setup()
		path := strings.TrimPrefix(r.URL.Path, contentsPrefix)
		root := s.trees[s.commitsBy[s.head].tree]
		e, ok := root[path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, http.StatusOK, map[string]string{
				"content":  base64.StdEncoding.EncodeToString([]byte(s.blobs[e.sha])),
				"sha":      e.sha,
				"encoding": "base64",
			})
		case http.MethodPut:
			var in struct{ Message, Content, SHA string }
			_ = json.NewDecoder(r.Body).Decode(&in)
			raw, _ := base64.StdEncoding.DecodeString(in.Content)
			blob := s.newSHA("blob")
			s.blobs[blob] = string(raw)
			next := map[string]entry{}
			for p, old := range root {
				next[p] = old
			}
			next[path] = entry{e.mode, blob}
			tree := s.newSHA("tree")
			s.trees[tree] = next
			sha := s.newSHA("commit")
			s.commitsBy[sha] = commit{tree: tree, parent: s.head}
			s.head = sha
			s.commits = append(s.commits, in.Message)
			writeJSON(w, http.StatusOK, map[string]any{"commit": map[string]string{"sha": sha}})
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

func newScaffoldServer(files ...fakeFile) *scaffoldServer {
	return &scaffoldServer{
		templateOwner: "matty-v",
		templateRepo:  "kyber-agent-template",
		newOwner:      "matty-v",
		newRepo:       "newbot-agent",
		defaultBranch: "main",
		files:         files,
	}
}

func scaffold(t *testing.T, s *scaffoldServer, params githubapp.ScaffoldParams) (string, error) {
	t.Helper()
	srv := httptest.NewServer(s.handler())
	t.Cleanup(srv.Close)
	return newScaffoldClient(t, srv).CreateFromTemplate(
		context.Background(),
		"ghs_faketoken",
		"matty-v", "kyber-agent-template",
		"matty-v", "newbot-agent",
		params,
	)
}

func TestCreateFromTemplate_HappyPath(t *testing.T) {
	s := newScaffoldServer(
		fakeFile{path: "CLAUDE.md", content: "# {{ .AgentName }}\n{{ .Description }}\n"},
		fakeFile{path: "IDENTITY.md", content: "Identity for {{ .AgentName }}."},
		fakeFile{path: "README.md", content: "No placeholders here."},
	)
	fullName, err := scaffold(t, s, githubapp.ScaffoldParams{AgentName: "newbot", Description: "My new bot"})
	if err != nil {
		t.Fatalf("CreateFromTemplate: %v", err)
	}
	if fullName != "matty-v/newbot-agent" {
		t.Errorf("fullName: got %q, want matty-v/newbot-agent", fullName)
	}
	if !s.generateCalled {
		t.Error("generate endpoint not called")
	}
	for path, want := range map[string]string{
		"CLAUDE.md":   "# newbot\nMy new bot\n",
		"IDENTITY.md": "Identity for newbot.",
		"README.md":   "No placeholders here.",
	} {
		if got, _ := s.fileAtHead(path); got != want {
			t.Errorf("%s at head = %q, want %q", path, got, want)
		}
	}
}

// MAT-90 G15: scaffolding used the Contents API, one PUT (and so one commit)
// per templated file — nine "Scaffold identity" commits at the base of every
// identity repo. It must land as one commit: a blob per changed file, one tree
// on the generated base, one commit, one ref update.
func TestCreateFromTemplate_SubstitutesInOneCommit(t *testing.T) {
	var files []fakeFile
	for i := 0; i < 9; i++ {
		files = append(files, fakeFile{path: fmt.Sprintf("docs/f%d.md", i), content: fmt.Sprintf("file %d of {{ .AgentName }}\n", i)})
	}
	files = append(files,
		fakeFile{path: "bin/start.sh", content: "#!/bin/sh\necho {{ .AgentName }}\n", mode: "100755"},
		fakeFile{path: "LICENSE", content: "untouched"},
	)
	s := newScaffoldServer(files...)

	if _, err := scaffold(t, s, githubapp.ScaffoldParams{AgentName: "newbot"}); err != nil {
		t.Fatalf("CreateFromTemplate: %v", err)
	}
	if len(s.commits) != 1 || s.refUpdates != 1 {
		t.Fatalf("commits = %d %q, ref updates = %d; want exactly one of each", len(s.commits), s.commits, s.refUpdates)
	}
	if s.commits[0] != "Scaffold identity for newbot" {
		t.Errorf("commit message = %q", s.commits[0])
	}
	if s.blobCreates != 10 {
		t.Errorf("blob creates = %d, want 10 (one per templated file)", s.blobCreates)
	}
	for i := 0; i < 9; i++ {
		if got, _ := s.fileAtHead(fmt.Sprintf("docs/f%d.md", i)); got != fmt.Sprintf("file %d of newbot\n", i) {
			t.Errorf("docs/f%d.md = %q", i, got)
		}
	}
	if got, _ := s.fileAtHead("LICENSE"); got != "untouched" {
		t.Errorf("an unchanged file was lost from the base tree: %q", got)
	}
	if mode := s.modeAtHead("bin/start.sh"); mode != "100755" {
		t.Errorf("bin/start.sh mode = %q, want the executable bit kept", mode)
	}
}

func TestCreateFromTemplate_TemplateMissing(t *testing.T) {
	s := newScaffoldServer()
	s.generateStatus = http.StatusNotFound
	_, err := scaffold(t, s, githubapp.ScaffoldParams{AgentName: "newbot"})
	if err == nil {
		t.Fatal("expected error for missing template, got nil")
	}
	if !strings.Contains(err.Error(), "template not found") {
		t.Errorf("error should mention template not found: %v", err)
	}
}

func TestCreateFromTemplate_RepoAlreadyExists_ContinuesToSubstitution(t *testing.T) {
	// 422 "Name already exists" → not an error; proceeds to substitution.
	s := newScaffoldServer(fakeFile{path: "CLAUDE.md", content: "# {{ .AgentName }}\n"})
	s.generateStatus = http.StatusUnprocessableEntity
	s.generateBody = `{"message":"Repository creation failed: Name already exists on this account"}`

	fullName, err := scaffold(t, s, githubapp.ScaffoldParams{AgentName: "newbot", Description: "retry test"})
	if err != nil {
		t.Fatalf("CreateFromTemplate: %v (expected idempotent success)", err)
	}
	if fullName != "matty-v/newbot-agent" {
		t.Errorf("fullName: got %q, want matty-v/newbot-agent", fullName)
	}
	if got, _ := s.fileAtHead("CLAUDE.md"); got != "# newbot\n" {
		t.Errorf("substitution did not run: CLAUDE.md = %q", got)
	}
}

func TestCreateFromTemplate_SubstitutionIdempotency(t *testing.T) {
	// Files that have already been substituted (no {{ . }} left) must not
	// produce a commit, so a retry after success changes nothing.
	s := newScaffoldServer(
		fakeFile{path: "CLAUDE.md", content: "# newbot\nMy new bot\n"},
		fakeFile{path: "README.md", content: "No placeholders here."},
	)
	if _, err := scaffold(t, s, githubapp.ScaffoldParams{AgentName: "newbot", Description: "My new bot"}); err != nil {
		t.Fatalf("CreateFromTemplate: %v", err)
	}
	if len(s.commits) != 0 || s.refUpdates != 0 || s.blobCreates != 0 {
		t.Errorf("commits=%v refUpdates=%d blobs=%d, want nothing written", s.commits, s.refUpdates, s.blobCreates)
	}
}

// TestCreateFromTemplate_EmptyRepoRetriedUntilPopulated covers GitHub's
// async template generation: POST /generate returns 201 + GET /repos
// returns 200 before the tree is populated, so GET /git/trees returns
// 409 "Git Repository is empty". listTree must retry rather than bail.
func TestCreateFromTemplate_EmptyRepoRetriedUntilPopulated(t *testing.T) {
	s := newScaffoldServer(fakeFile{path: "CLAUDE.md", content: "# {{ .AgentName }}\n"})
	// Simulate the real-world race: first 3 tree calls land 409, then
	// GitHub finishes populating and the 4th succeeds.
	s.treeEmptyResponses = 3

	full, err := scaffold(t, s, githubapp.ScaffoldParams{AgentName: "newbot", Description: "retry test"})
	if err != nil {
		t.Fatalf("CreateFromTemplate: %v", err)
	}
	if full != "matty-v/newbot-agent" {
		t.Errorf("fullName: got %q, want matty-v/newbot-agent", full)
	}
	if s.treeCalls < 4 {
		t.Errorf("treeCalls: got %d, want >= 4 (3 empty + 1 populated)", s.treeCalls)
	}
	if got, _ := s.fileAtHead("CLAUDE.md"); got != "# newbot\n" {
		t.Errorf("CLAUDE.md = %q", got)
	}
}

// Seen live on kyber-dev: the tree of a just-generated repo is served while
// its blobs still 404 for a moment. The scaffold waits them out instead of
// failing the attempt.
func TestCreateFromTemplate_BlobNotYetReplicatedRetried(t *testing.T) {
	s := newScaffoldServer(fakeFile{path: ".mcp.json", content: `{"name":"{{ .AgentName }}"}`})
	s.blobMissingResponses = 3
	if _, err := scaffold(t, s, githubapp.ScaffoldParams{AgentName: "newbot"}); err != nil {
		t.Fatalf("CreateFromTemplate: %v", err)
	}
	if got, _ := s.fileAtHead(".mcp.json"); got != `{"name":"newbot"}` {
		t.Errorf(".mcp.json = %q", got)
	}
}

func TestCreateFromTemplate_BinaryFileSkipped(t *testing.T) {
	s := newScaffoldServer(
		fakeFile{path: "CLAUDE.md", content: "# {{ .AgentName }}\n"},
		// Binary extensions — isBinaryPath should return true, so the bytes
		// are never read or rewritten even though they look like a placeholder.
		fakeFile{path: "avatar.png", content: "{{ .AgentName }}binarydata"},
		fakeFile{path: "doc.pdf", content: "{{ .AgentName }}binarydata"},
	)
	if _, err := scaffold(t, s, githubapp.ScaffoldParams{AgentName: "newbot", Description: "binary test"}); err != nil {
		t.Fatalf("CreateFromTemplate: %v", err)
	}
	for _, p := range []string{"avatar.png", "doc.pdf"} {
		if got, _ := s.fileAtHead(p); got != "{{ .AgentName }}binarydata" {
			t.Errorf("binary file %s was rewritten: %q", p, got)
		}
	}
	if got, _ := s.fileAtHead("CLAUDE.md"); got != "# newbot\n" {
		t.Errorf("CLAUDE.md = %q", got)
	}
}

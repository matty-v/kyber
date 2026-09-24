package githubapp

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// binaryExtensions is the set of file extensions CreateFromTemplate skips
// during placeholder substitution. Content files with these extensions are
// assumed to be binary and not safe to base64-decode-then-re-encode through
// a text substitution pass.
var binaryExtensions = map[string]bool{
	".jpg":  true,
	".jpeg": true,
	".png":  true,
	".gif":  true,
	".pdf":  true,
	".bin":  true,
	".ico":  true,
	".svg":  false, // SVG is XML — could contain placeholders, treat as text
	".woff": true,
	".woff2": true,
	".ttf":  true,
	".otf":  true,
	".eot":  true,
	".mp4":  true,
	".mp3":  true,
	".zip":  true,
	".gz":   true,
	".tar":  true,
}

// ScaffoldParams carries the template substitution inputs for CreateFromTemplate.
type ScaffoldParams struct {
	AgentName   string
	Description string
}

// generateBody is the request body for POST /repos/{owner}/{repo}/generate.
type generateBody struct {
	Owner            string `json:"owner"`
	Name             string `json:"name"`
	Description      string `json:"description"`
	Private          bool   `json:"private"`
	IncludeAllBranches bool  `json:"include_all_branches"`
}

// treeResponse is a subset of the GitHub git/trees API response.
type treeResponse struct {
	SHA  string     `json:"sha"`
	Tree []treeItem `json:"tree"`
}

type treeItem struct {
	Path string `json:"path"`
	Mode string `json:"mode,omitempty"` // e.g. "100644", "100755"
	Type string `json:"type"`           // "blob" or "tree"
	SHA  string `json:"sha"`
}

// blobResponse is a subset of GET /repos/{owner}/{repo}/git/blobs/{sha}.
type blobResponse struct {
	Content  string `json:"content"`  // base64-encoded, may include newlines
	Encoding string `json:"encoding"` // "base64"
}

// refResponse is a subset of GET /repos/{owner}/{repo}/git/ref/{ref}.
type refResponse struct {
	Object struct {
		SHA string `json:"sha"`
	} `json:"object"`
}

// shaResponse is the part of a Git Data create response the scaffold uses.
type shaResponse struct {
	SHA string `json:"sha"`
}

// repoResponse is a subset of GET /repos/{owner}/{repo}.
type repoResponse struct {
	DefaultBranch string `json:"default_branch"`
	FullName      string `json:"full_name"`
}

// CreateFromTemplate creates a new private repo from templateOwner/templateRepo
// under newOwner/newRepoName, then substitutes {{ .AgentName }} and
// {{ .Description }} in all text files as ONE commit through the Git Data API.
// Returns the full name "owner/repo" of the new repo.
//
// One commit, not one per file: the Contents API commits every PUT, so a
// template of nine files left nine "Scaffold identity" commits at the base of
// every identity repo (MAT-90 G15).
//
// Idempotent: if the target repo already exists (HTTP 422 "name already exists"),
// the function skips creation and proceeds to the substitution step — this allows
// safe retry after partial failure. Substitution is also idempotent: files
// without placeholders are left alone, and when no file needs a change no
// commit is made. Nothing is visible until the final ref update, so a failure
// part-way leaves the repo as generated.
func (c *Client) CreateFromTemplate(
	ctx context.Context,
	installationToken string,
	templateOwner, templateRepo string,
	newOwner, newRepoName string,
	params ScaffoldParams,
) (fullName string, err error) {
	// Step 1: POST /repos/{templateOwner}/{templateRepo}/generate
	if err := c.generateFromTemplate(ctx, installationToken, templateOwner, templateRepo, newOwner, newRepoName, params.Description); err != nil {
		return "", err
	}

	// Step 2: Poll until the repo is visible (generation is async on GitHub's side).
	if err := c.waitForRepo(ctx, installationToken, newOwner, newRepoName); err != nil {
		return "", fmt.Errorf("githubapp: waiting for generated repo %s/%s: %w", newOwner, newRepoName, err)
	}

	// Step 3: Get default branch.
	branch, err := c.getDefaultBranch(ctx, installationToken, newOwner, newRepoName)
	if err != nil {
		return "", fmt.Errorf("githubapp: getting default branch of %s/%s: %w", newOwner, newRepoName, err)
	}

	// Step 4: Wait for the tree to be populated, then pin the head commit and
	// read the tree AT that commit, so the new commit's base tree and parent
	// are the same snapshot.
	if _, _, err := c.listTree(ctx, installationToken, newOwner, newRepoName, branch); err != nil {
		return "", fmt.Errorf("githubapp: listing tree for %s/%s: %w", newOwner, newRepoName, err)
	}
	head, err := c.getRefSHA(ctx, installationToken, newOwner, newRepoName, branch)
	if err != nil {
		return "", fmt.Errorf("githubapp: reading %s head of %s/%s: %w", branch, newOwner, newRepoName, err)
	}
	baseTree, tree, err := c.listTree(ctx, installationToken, newOwner, newRepoName, head)
	if err != nil {
		return "", fmt.Errorf("githubapp: listing tree for %s/%s: %w", newOwner, newRepoName, err)
	}

	// Step 5: Substitute placeholders in each text blob, collecting the
	// changed files as new blobs.
	var changed []treeItem
	for _, item := range tree {
		if item.Type != "blob" || isBinaryPath(item.Path) {
			continue
		}
		newSHA, subErr := c.substituteBlob(ctx, installationToken, newOwner, newRepoName, item, params)
		if subErr != nil {
			return "", fmt.Errorf("githubapp: substituting %s in %s/%s: %w", item.Path, newOwner, newRepoName, subErr)
		}
		if newSHA != "" {
			changed = append(changed, treeItem{Path: item.Path, Mode: item.Mode, Type: "blob", SHA: newSHA})
		}
	}
	if len(changed) == 0 {
		return newOwner + "/" + newRepoName, nil
	}

	// Step 6: One tree, one commit, one fast-forward ref update.
	if err := c.commitChanges(ctx, installationToken, newOwner, newRepoName, branch, head, baseTree, changed,
		"Scaffold identity for "+params.AgentName); err != nil {
		return "", fmt.Errorf("githubapp: committing scaffold to %s/%s: %w", newOwner, newRepoName, err)
	}
	return newOwner + "/" + newRepoName, nil
}

// generateFromTemplate calls POST /repos/{tOwner}/{tRepo}/generate. Returns nil on
// success (201 Created) or if the repo already exists (422 "name already exists").
func (c *Client) generateFromTemplate(
	ctx context.Context,
	token string,
	templateOwner, templateRepo string,
	newOwner, newRepoName string,
	description string,
) error {
	body := generateBody{
		Owner:              newOwner,
		Name:               newRepoName,
		Description:        description,
		Private:            true,
		IncludeAllBranches: false,
	}
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("githubapp: marshaling generate request: %w", err)
	}

	url := fmt.Sprintf("%s/repos/%s/%s/generate", c.baseURL, templateOwner, templateRepo)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyBytes))
	if err != nil {
		return fmt.Errorf("githubapp: building generate request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("githubapp: POST generate: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))

	switch resp.StatusCode {
	case http.StatusCreated:
		return nil
	case http.StatusNotFound:
		return fmt.Errorf("githubapp: template not found: %s/%s (HTTP 404)", templateOwner, templateRepo)
	case http.StatusUnprocessableEntity:
		// 422 may mean "name already exists" — treat that as idempotent success.
		if strings.Contains(string(respBody), "Name already exists") ||
			strings.Contains(string(respBody), "name already exists") {
			return nil
		}
		return fmt.Errorf("githubapp: POST generate: HTTP 422: %s", bytes.TrimSpace(respBody))
	default:
		return fmt.Errorf("githubapp: POST generate: HTTP %d: %s", resp.StatusCode, bytes.TrimSpace(respBody))
	}
}

// waitForRepo polls GET /repos/{owner}/{repo} until 200 or context deadline.
// GitHub's generate API is async — the repo may not exist for a few seconds.
func (c *Client) waitForRepo(ctx context.Context, token, owner, repo string) error {
	const maxAttempts = 10
	const backoff = 500 * time.Millisecond

	url := fmt.Sprintf("%s/repos/%s/%s", c.baseURL, owner, repo)
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return fmt.Errorf("building request: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

		resp, err := c.http.Do(req)
		if err != nil {
			// Network error — retry.
			continue
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()

		if resp.StatusCode == http.StatusOK {
			return nil
		}
	}
	return fmt.Errorf("repo %s/%s not ready after %d attempts", owner, repo, maxAttempts)
}

// getDefaultBranch returns the default branch name for the given repo.
func (c *Client) getDefaultBranch(ctx context.Context, token, owner, repo string) (string, error) {
	url := fmt.Sprintf("%s/repos/%s/%s", c.baseURL, owner, repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("GET /repos: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET /repos: HTTP %d: %s", resp.StatusCode, bytes.TrimSpace(body))
	}

	var r repoResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return "", fmt.Errorf("decoding repo response: %w", err)
	}
	if r.DefaultBranch == "" {
		return "main", nil
	}
	return r.DefaultBranch, nil
}

// listTree returns the root tree SHA and the recursive file tree for the repo
// at the given ref (a branch name or a commit SHA).
//
// Retries on HTTP 409 "Git Repository is empty" — GitHub's template-generate
// API returns from POST /generate before the tree is populated. The repo is
// observable via GET /repos (which waitForRepo polled) before the tree is
// ready, so a naive fetch here races the async population and fails the
// whole scaffold. We back off and retry up to listTreeMaxAttempts.
func (c *Client) listTree(ctx context.Context, token, owner, repo, ref string) (string, []treeItem, error) {
	const (
		maxAttempts = 12
		backoff     = 500 * time.Millisecond
	)

	url := fmt.Sprintf("%s/repos/%s/%s/git/trees/%s?recursive=1", c.baseURL, owner, repo, ref)
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return "", nil, ctx.Err()
			case <-time.After(backoff):
			}
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return "", nil, fmt.Errorf("building request: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

		resp, err := c.http.Do(req)
		if err != nil {
			return "", nil, fmt.Errorf("GET /git/trees: %w", err)
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
		resp.Body.Close()

		switch resp.StatusCode {
		case http.StatusOK:
			var t treeResponse
			if err := json.Unmarshal(body, &t); err != nil {
				return "", nil, fmt.Errorf("decoding tree response: %w", err)
			}
			return t.SHA, t.Tree, nil
		case http.StatusNotFound:
			return "", nil, fmt.Errorf("template not found: %s/%s (HTTP 404)", owner, repo)
		case http.StatusConflict:
			// GitHub's "Git Repository is empty" — generate is still populating.
			// Retry after the backoff.
			continue
		default:
			return "", nil, fmt.Errorf("GET /git/trees: HTTP %d: %s", resp.StatusCode, bytes.TrimSpace(body))
		}
	}
	return "", nil, fmt.Errorf("GET /git/trees: tree not populated after %d attempts — template generate may have stalled", maxAttempts)
}

// gitData makes one Git Data API call, JSON in and out, and fails unless the
// response status is want.
func (c *Client) gitData(ctx context.Context, token, method, url string, in, out any, want int) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("marshaling %s body: %w", method, err)
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return fmt.Errorf("building %s request: %w", method, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, strings.TrimPrefix(url, c.baseURL), err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if resp.StatusCode != want {
		return &gitDataError{status: resp.StatusCode,
			msg: fmt.Sprintf("%s %s: HTTP %d: %s", method, strings.TrimPrefix(url, c.baseURL), resp.StatusCode, bytes.TrimSpace(respBody))}
	}
	if out != nil {
		if err := json.Unmarshal(respBody, out); err != nil {
			return fmt.Errorf("decoding %s %s response: %w", method, strings.TrimPrefix(url, c.baseURL), err)
		}
	}
	return nil
}

// gitDataError is a Git Data API call answered with an unexpected status.
type gitDataError struct {
	status int
	msg    string
}

func (e *gitDataError) Error() string { return e.msg }

// getRefSHA returns the commit SHA the branch points at.
func (c *Client) getRefSHA(ctx context.Context, token, owner, repo, branch string) (string, error) {
	var ref refResponse
	url := fmt.Sprintf("%s/repos/%s/%s/git/ref/heads/%s", c.baseURL, owner, repo, branch)
	if err := c.gitData(ctx, token, http.MethodGet, url, nil, &ref, http.StatusOK); err != nil {
		return "", err
	}
	if ref.Object.SHA == "" {
		return "", fmt.Errorf("ref heads/%s has no commit", branch)
	}
	return ref.Object.SHA, nil
}

// substituteBlob reads a blob, substitutes placeholders, and stores the result
// as a new blob. It returns the new blob's SHA, or "" when the file needs no
// change (no placeholders, already substituted, or not decodable text).
func (c *Client) substituteBlob(ctx context.Context, token, owner, repo string, item treeItem, params ScaffoldParams) (string, error) {
	var blob blobResponse
	url := fmt.Sprintf("%s/repos/%s/%s/git/blobs/%s", c.baseURL, owner, repo, item.SHA)
	// A just-generated repo serves its tree before every replica can serve
	// its blobs, so a blob the tree names may 404 for a moment.
	const (
		maxAttempts = 12
		backoff     = 500 * time.Millisecond
	)
	for attempt := 1; ; attempt++ {
		err := c.gitData(ctx, token, http.MethodGet, url, nil, &blob, http.StatusOK)
		var gerr *gitDataError
		if err == nil {
			break
		}
		if !errors.As(err, &gerr) || gerr.status != http.StatusNotFound || attempt == maxAttempts {
			return "", err
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(backoff):
		}
	}
	if blob.Encoding != "" && blob.Encoding != "base64" {
		return "", nil
	}
	// GitHub returns base64 with embedded newlines.
	raw, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(blob.Content, "\n", ""))
	if err != nil {
		// Probably a truly binary file even without a binary extension; skip safely.
		return "", nil
	}

	text := string(raw)
	// Skip if no placeholders present — already substituted or plain file.
	if !strings.Contains(text, "{{ .") {
		return "", nil
	}
	// Perform the literal substitutions (not text/template — keep it simple).
	text = strings.ReplaceAll(text, "{{ .AgentName }}", params.AgentName)
	text = strings.ReplaceAll(text, "{{ .Description }}", params.Description)

	var created shaResponse
	createURL := fmt.Sprintf("%s/repos/%s/%s/git/blobs", c.baseURL, owner, repo)
	in := map[string]string{"content": base64.StdEncoding.EncodeToString([]byte(text)), "encoding": "base64"}
	if err := c.gitData(ctx, token, http.MethodPost, createURL, in, &created, http.StatusCreated); err != nil {
		return "", err
	}
	return created.SHA, nil
}

// commitChanges writes changed files as one commit on top of parent and
// fast-forwards the branch to it. The ref update is not forced: if the branch
// moved since parent was read, GitHub refuses it and nothing is lost.
func (c *Client) commitChanges(ctx context.Context, token, owner, repo, branch, parent, baseTree string, changed []treeItem, message string) error {
	for i := range changed {
		if changed[i].Mode == "" {
			changed[i].Mode = "100644"
		}
	}
	base := fmt.Sprintf("%s/repos/%s/%s/git", c.baseURL, owner, repo)

	var tree shaResponse
	if err := c.gitData(ctx, token, http.MethodPost, base+"/trees",
		map[string]any{"base_tree": baseTree, "tree": changed}, &tree, http.StatusCreated); err != nil {
		return err
	}
	var commit shaResponse
	if err := c.gitData(ctx, token, http.MethodPost, base+"/commits",
		map[string]any{"message": message, "tree": tree.SHA, "parents": []string{parent}}, &commit, http.StatusCreated); err != nil {
		return err
	}
	return c.gitData(ctx, token, http.MethodPatch, base+"/refs/heads/"+branch,
		map[string]any{"sha": commit.SHA, "force": false}, nil, http.StatusOK)
}

// isBinaryPath returns true when the file path's extension is in binaryExtensions.
func isBinaryPath(path string) bool {
	idx := strings.LastIndex(path, ".")
	if idx < 0 {
		return false // no extension — assume text
	}
	ext := strings.ToLower(path[idx:])
	return binaryExtensions[ext]
}

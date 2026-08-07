package githubapp

import (
	"bytes"
	"context"
	"encoding/base64"
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
	Type string `json:"type"` // "blob" or "tree"
	SHA  string `json:"sha"`
}

// contentsResponse is a subset of the GitHub contents API response.
type contentsResponse struct {
	Content  string `json:"content"`  // base64-encoded, may include newlines
	SHA      string `json:"sha"`      // blob SHA — required for PUT
	Encoding string `json:"encoding"` // "base64"
}

// contentsUpdateBody is the request body for PUT /repos/{owner}/{repo}/contents/{path}.
type contentsUpdateBody struct {
	Message string `json:"message"`
	Content string `json:"content"` // base64-encoded
	SHA     string `json:"sha"`     // current blob SHA — required
}

// repoResponse is a subset of GET /repos/{owner}/{repo}.
type repoResponse struct {
	DefaultBranch string `json:"default_branch"`
	FullName      string `json:"full_name"`
}

// CreateFromTemplate creates a new private repo from templateOwner/templateRepo
// under newOwner/newRepoName, then substitutes {{ .AgentName }} and
// {{ .Description }} in all text files via the GitHub Contents API.
// Returns the full name "owner/repo" of the new repo.
//
// Idempotent: if the target repo already exists (HTTP 422 "name already exists"),
// the function skips creation and proceeds to the substitution step — this allows
// safe retry after partial failure. Substitution is also idempotent (files without
// placeholders are skipped; files already substituted contain no placeholders and
// are skipped too).
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

	// Step 4: List all blobs recursively.
	tree, err := c.listTree(ctx, installationToken, newOwner, newRepoName, branch)
	if err != nil {
		return "", fmt.Errorf("githubapp: listing tree for %s/%s: %w", newOwner, newRepoName, err)
	}

	// Step 5: Substitute placeholders in each text blob.
	for _, item := range tree {
		if item.Type != "blob" {
			continue
		}
		if isBinaryPath(item.Path) {
			continue
		}
		if subErr := c.substituteFile(ctx, installationToken, newOwner, newRepoName, item.Path, params); subErr != nil {
			return "", fmt.Errorf("githubapp: substituting %s in %s/%s: %w", item.Path, newOwner, newRepoName, subErr)
		}
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

// listTree returns the recursive file tree for the repo at the given branch ref.
//
// Retries on HTTP 409 "Git Repository is empty" — GitHub's template-generate
// API returns from POST /generate before the tree is populated. The repo is
// observable via GET /repos (which waitForRepo polled) before the tree is
// ready, so a naive fetch here races the async population and fails the
// whole scaffold. We back off and retry up to listTreeMaxAttempts.
func (c *Client) listTree(ctx context.Context, token, owner, repo, branch string) ([]treeItem, error) {
	const (
		maxAttempts = 12
		backoff     = 500 * time.Millisecond
	)

	url := fmt.Sprintf("%s/repos/%s/%s/git/trees/%s?recursive=1", c.baseURL, owner, repo, branch)
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff):
			}
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, fmt.Errorf("building request: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

		resp, err := c.http.Do(req)
		if err != nil {
			return nil, fmt.Errorf("GET /git/trees: %w", err)
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
		resp.Body.Close()

		switch resp.StatusCode {
		case http.StatusOK:
			var t treeResponse
			if err := json.Unmarshal(body, &t); err != nil {
				return nil, fmt.Errorf("decoding tree response: %w", err)
			}
			return t.Tree, nil
		case http.StatusNotFound:
			return nil, fmt.Errorf("template not found: %s/%s (HTTP 404)", owner, repo)
		case http.StatusConflict:
			// GitHub's "Git Repository is empty" — generate is still populating.
			// Retry after the backoff.
			continue
		default:
			return nil, fmt.Errorf("GET /git/trees: HTTP %d: %s", resp.StatusCode, bytes.TrimSpace(body))
		}
	}
	return nil, fmt.Errorf("GET /git/trees: tree not populated after %d attempts — template generate may have stalled", maxAttempts)
}

// substituteFile fetches a file's contents, substitutes placeholders if any
// are present, and PUTs the updated content back. Skips files with no placeholders
// (idempotent — already-substituted files have no placeholders left).
func (c *Client) substituteFile(ctx context.Context, token, owner, repo, path string, params ScaffoldParams) error {
	// GET the current contents.
	url := fmt.Sprintf("%s/repos/%s/%s/contents/%s", c.baseURL, owner, repo, path)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("building GET request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("GET /contents/%s: %w", path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET /contents/%s: HTTP %d: %s", path, resp.StatusCode, bytes.TrimSpace(body))
	}

	var contents contentsResponse
	if err := json.Unmarshal(body, &contents); err != nil {
		return fmt.Errorf("decoding contents response for %s: %w", path, err)
	}

	// Decode — GitHub returns base64 with possible embedded newlines.
	rawContent, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(contents.Content, "\n", ""))
	if err != nil {
		// If standard decoding fails, try without the trimming (some rare files).
		rawContent, err = base64.StdEncoding.DecodeString(contents.Content)
		if err != nil {
			// Probably a truly binary file even without a binary extension; skip safely.
			return nil
		}
	}

	text := string(rawContent)

	// Skip if no placeholders present — already substituted or plain file.
	if !strings.Contains(text, "{{ .") {
		return nil
	}

	// Perform the literal substitutions (not text/template — keep it simple).
	text = strings.ReplaceAll(text, "{{ .AgentName }}", params.AgentName)
	text = strings.ReplaceAll(text, "{{ .Description }}", params.Description)

	// PUT the updated content.
	newContent := base64.StdEncoding.EncodeToString([]byte(text))
	updateBody := contentsUpdateBody{
		Message: "Scaffold identity for " + params.AgentName,
		Content: newContent,
		SHA:     contents.SHA,
	}
	updateBytes, err := json.Marshal(updateBody)
	if err != nil {
		return fmt.Errorf("marshaling PUT body for %s: %w", path, err)
	}

	putReq, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(updateBytes))
	if err != nil {
		return fmt.Errorf("building PUT request for %s: %w", path, err)
	}
	putReq.Header.Set("Authorization", "Bearer "+token)
	putReq.Header.Set("Accept", "application/vnd.github+json")
	putReq.Header.Set("Content-Type", "application/json")
	putReq.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	putResp, err := c.http.Do(putReq)
	if err != nil {
		return fmt.Errorf("PUT /contents/%s: %w", path, err)
	}
	defer putResp.Body.Close()
	putBody, _ := io.ReadAll(io.LimitReader(putResp.Body, maxResponseBytes))
	if putResp.StatusCode != http.StatusOK && putResp.StatusCode != http.StatusCreated {
		return fmt.Errorf("PUT /contents/%s: HTTP %d: %s", path, putResp.StatusCode, bytes.TrimSpace(putBody))
	}

	return nil
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

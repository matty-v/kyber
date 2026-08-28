//go:build integration

// Package startclaudeshell invokes start-claude.sh from Go to verify boot-time
// behavior. Uses the oauth mockserver and a temp HOME. The //go:build tag keeps
// it out of the default `go test ./...` run; run with `-tags=integration` or
// via `go test ./images/claude-code/ -tags=integration`.
package startclaudeshell_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/matty-v/kyber/pkg/oauth/mockserver"
)

// testPATH returns a PATH suitable for running the script in tests.
// It inherits the current process PATH so jq/curl/date resolve correctly
// regardless of which machine or CI environment is running the tests.
func testPATH() string {
	existing := os.Getenv("PATH")
	if existing == "" {
		return "/usr/local/bin:/usr/bin:/bin"
	}
	return existing
}

// scriptPath returns the absolute path to start-claude.sh.
// The test file lives in the same directory as the script.
func scriptPath(t *testing.T) string {
	t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(cwd, "start-claude.sh")
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("script not found at %s: %v", p, err)
	}
	return p
}

// pkcePair returns a (verifier, challenge) pair for PKCE.
func pkcePair() (verifier, challenge string) {
	verifier = "test-verifier-with-sufficient-entropy-abcdef0123"
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return
}

// seedRefreshToken issues a code, exchanges it for tokens, and returns the refresh token.
func seedRefreshToken(t *testing.T, mock *mockserver.Server, baseURL string) string {
	t.Helper()
	verifier, challenge := pkcePair()
	code := mock.IssueCode(challenge)

	body, err := json.Marshal(map[string]string{
		"grant_type":    "authorization_code",
		"code":          code,
		"client_id":     "9d1c250a-e61b-44d9-88ed-5944d1962f5e",
		"redirect_uri":  "urn:ietf:wg:oauth:2.0:oob",
		"code_verifier": verifier,
	})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(baseURL+"/v1/oauth/token", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("code exchange failed: status %d", resp.StatusCode)
	}
	var out struct {
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.RefreshToken == "" {
		t.Fatal("code exchange returned empty refresh_token")
	}
	return out.RefreshToken
}

// runScript invokes start-claude.sh with the given environment and returns
// the combined output and any exit error.
func runScript(t *testing.T, env []string) ([]byte, error) {
	t.Helper()
	cmd := exec.Command("/bin/bash", scriptPath(t))
	cmd.Env = append(env, "KYBER_SKIP_RUNTIME_PROBE=1")
	return cmd.CombinedOutput()
}

func TestStartClaudeRegistersDiscordMCP(t *testing.T) {
	script, err := os.ReadFile(scriptPath(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`claude mcp remove kyber-discord --scope user`,
		`claude mcp add kyber-discord "$KYBER_DISCORD_MCP_URL"`,
		`--transport http --scope user`,
	} {
		if !strings.Contains(string(script), want) {
			t.Fatalf("start-claude.sh missing Discord MCP registration %q", want)
		}
	}
}

func TestStartClaudeBrokenHarnessReportsBeforeAuthentication(t *testing.T) {
	dir := t.TempDir()
	report := filepath.Join(dir, "report")
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/usr/bin/env bash\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// A Node syntax failure may include its own semver. A parsed version is not
	// success unless the harness command itself also exits zero.
	write("claude", `printf 'SyntaxError: invalid token\nNode.js v26.7.0\n' >&2; exit 1`)
	write("curl", `printf '%s\n' "$*" > "$REPORT_FILE"`)
	cmd := exec.Command("/bin/bash", scriptPath(t))
	cmd.Env = []string{
		"HOME=" + t.TempDir(),
		"PATH=" + dir + ":" + testPATH(),
		"REPORT_FILE=" + report,
	}
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(err.Error(), "exit status 43") {
		t.Fatalf("broken harness exit = %v\n%s", err, out)
	}
	body, readErr := os.ReadFile(report)
	if readErr != nil || !strings.Contains(string(body), `"usable":false`) || !strings.Contains(string(body), `"runtime":"claude-code"`) {
		t.Fatalf("runtime failure report = %q, err=%v", body, readErr)
	}
	if !strings.Contains(string(body), `Node.js v26.7.0`) {
		t.Fatalf("runtime failure report lost nonzero semver output: %q", body)
	}
	if strings.Contains(string(out), "refreshing access token") {
		t.Fatalf("authentication ran after failed runtime probe:\n%s", out)
	}
}

func TestStartClaudeRegistersRequestReplyMCP(t *testing.T) {
	script, err := os.ReadFile(scriptPath(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`claude mcp remove kyber-request-reply --scope user`,
		`claude mcp add kyber-request-reply "$KYBER_REQUEST_MCP_URL"`,
		`--transport http --scope user`,
	} {
		if !strings.Contains(string(script), want) {
			t.Fatalf("start-claude.sh missing request MCP registration %q", want)
		}
	}
}

func TestStartClaudeTrustsResolvedLaunchDirectoryBeforeClaudeStarts(t *testing.T) {
	script, err := os.ReadFile(scriptPath(t))
	if err != nil {
		t.Fatal(err)
	}
	s := string(script)
	resolve := strings.Index(s, `LAUNCH_DIR="$REPO_DIR"`)
	trust := strings.Index(s, `.projects[$launch_dir]`)
	launch := strings.LastIndex(s, `tmux new-session -d -s agent -c "$LAUNCH_DIR" "$BOOT_LAUNCH_CMD"`)
	if resolve < 0 || trust < 0 || launch < 0 {
		t.Fatalf("missing launch-dir resolution, trust merge, or Claude launch")
	}
	if !(resolve < trust && trust < launch) {
		t.Fatalf("workspace trust must be written after resolving LAUNCH_DIR and before Claude starts")
	}
	if strings.Contains(s[:trust], `"${WORK_DIR}": {`) {
		t.Fatal("startup still trusts the pre-identity-repo working directory")
	}
}

func TestStartClaudeRecoversCorruptClaudeStateBeforeTrustMerge(t *testing.T) {
	script, err := os.ReadFile(scriptPath(t))
	if err != nil {
		t.Fatal(err)
	}
	s := string(script)
	validate := strings.Index(s, `if ! jq empty "$CLAUDE_STATE"`)
	recover := strings.Index(s, `printf '{}\n' > "$CLAUDE_STATE"`)
	merge := strings.Index(s, `if ! jq --arg launch_dir "$LAUNCH_DIR"`)
	if validate < 0 || recover < 0 || merge < 0 || !(validate < recover && recover < merge) {
		t.Fatal("corrupt Claude state must be rebuilt before the workspace-trust merge")
	}
}

func TestStartClaude_RefreshOnBoot_WritesCredentialsJSON(t *testing.T) {
	mock := mockserver.New()
	ts := httptest.NewServer(mock)
	defer ts.Close()

	cpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(204)
	}))
	defer cpServer.Close()

	refreshToken := seedRefreshToken(t, mock, ts.URL)
	tmpHome := t.TempDir()

	out, err := runScript(t, []string{
		"HOME=" + tmpHome,
		"PATH=" + testPATH(),
		"CLAUDE_REFRESH_TOKEN=" + refreshToken,
		"AGENT_NAME=unit-test",
		"ANTHROPIC_TOKEN_URL=" + ts.URL + "/v1/oauth/token",
		"KYBER_REFRESH_TOKEN_URL=" + cpServer.URL + "/internal/agents/unit-test/refresh-token",
		"SKIP_CLAUDE_LAUNCH=1",
	})
	if err != nil {
		t.Fatalf("script failed: %v\noutput:\n%s", err, out)
	}

	credsPath := filepath.Join(tmpHome, ".claude", ".credentials.json")
	data, err := os.ReadFile(credsPath)
	if err != nil {
		t.Fatalf("credentials.json not written: %v\nscript output:\n%s", err, out)
	}

	var parsed struct {
		ClaudeAiOauth struct {
			AccessToken  string   `json:"accessToken"`
			RefreshToken string   `json:"refreshToken"`
			ExpiresAt    int64    `json:"expiresAt"`
			Scopes       []string `json:"scopes"`
		} `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("invalid JSON in credentials.json: %v — raw:\n%s", err, data)
	}

	o := parsed.ClaudeAiOauth
	if o.AccessToken == "" {
		t.Errorf("accessToken is empty")
	}
	if o.RefreshToken == "" {
		t.Errorf("refreshToken is empty")
	}
	if o.ExpiresAt == 0 {
		t.Errorf("expiresAt is zero")
	}
	if len(o.Scopes) == 0 {
		t.Errorf("scopes is empty")
	}
	if !containsScope(o.Scopes, "user:sessions:claude_code") {
		t.Errorf("missing required scope user:sessions:claude_code; got: %v", o.Scopes)
	}
}

func TestStartClaude_RefreshFailure_ExitsTwo(t *testing.T) {
	mock := mockserver.New()
	ts := httptest.NewServer(mock)
	defer ts.Close()

	tmpHome := t.TempDir()

	out, err := runScript(t, []string{
		"HOME=" + tmpHome,
		"PATH=" + testPATH(),
		"CLAUDE_REFRESH_TOKEN=rt-bogus-never-registered",
		"AGENT_NAME=unit-test",
		"ANTHROPIC_TOKEN_URL=" + ts.URL + "/v1/oauth/token",
		"KYBER_REFRESH_TOKEN_URL=http://127.0.0.1:1/unused",
		"SKIP_CLAUDE_LAUNCH=1",
	})
	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("expected exit error, got: %v\noutput:\n%s", err, out)
	}
	if exitErr.ExitCode() != 2 {
		t.Fatalf("expected exit code 2, got %d\noutput:\n%s", exitErr.ExitCode(), out)
	}
}

func TestStartClaude_MissingOAuthCredential_ExitsTwo(t *testing.T) {
	tmpHome := t.TempDir()
	out, err := runScript(t, []string{
		"HOME=" + tmpHome,
		"PATH=" + testPATH(),
		"AGENT_NAME=unit-test",
		"KYBER_REFRESH_TOKEN_URL=http://127.0.0.1:1/unused",
		"SKIP_CLAUDE_LAUNCH=1",
	})
	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("expected exit error, got: %v\noutput:\n%s", err, out)
	}
	if exitErr.ExitCode() != 2 {
		t.Fatalf("expected exit code 2 (NeedsAuth), got %d\noutput:\n%s", exitErr.ExitCode(), out)
	}
	if !strings.Contains(string(out), "OAuth credential is missing") {
		t.Fatalf("missing credential must be explicit in boot output:\n%s", out)
	}
}

// --- kyber#509 — git auth via the generic PAT (not the in-platform App token) -
//
// Stage 2 of the decouple-#508 cutover migrated the git credential helper off
// the in-platform per-agent GitHub App token (mounted at
// KYBER_IDENTITY_TOKEN_PATH / /var/run/secrets/kyber-github/token) onto the
// generic PAT user-secret ($GH_TOKEN / $USER_GITHUB_TOKEN) — the same
// credential the `gh` CLI uses. The token-file mount and its first-boot poll
// are gone; the helper now emits the PAT from the environment on every call.
// These tests pin the new contract.

// runCredentialHelper invokes the installed git-credential helper as git would
// (`<helper> get` with a request on stdin) under the given env, returning the
// helper's stdout. This is how we assert which credential the helper actually
// emits.
func runCredentialHelper(t *testing.T, helperPath string, env []string) string {
	t.Helper()
	cmd := exec.Command(helperPath, "get")
	cmd.Env = env
	cmd.Stdin = strings.NewReader("protocol=https\nhost=github.com\n\n")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("credential helper %s failed: %v\noutput:\n%s", helperPath, err, out)
	}
	return string(out)
}

// runCredentialHelperReq invokes the installed git-credential helper with a
// caller-supplied request on stdin and returns stdout and stderr SEPARATELY.
// The reworked helper (kyber#508 Stage 3/4) always exits 0: on the identity-repo
// fail-loud path it writes nothing to stdout and a diagnostic to stderr, so a
// merged stream can't distinguish "emitted the token" from "emitted the
// diagnostic". Keeping the streams apart lets a test assert an empty stdout
// while the stderr carries the fail-loud message.
func runCredentialHelperReq(t *testing.T, helperPath string, env []string, request string) (stdout, stderr string) {
	t.Helper()
	cmd := exec.Command(helperPath, "get")
	cmd.Env = env
	cmd.Stdin = strings.NewReader(request)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		t.Fatalf("credential helper %s exited non-zero (must always exit 0): %v\nstdout:\n%s\nstderr:\n%s",
			helperPath, err, outBuf.String(), errBuf.String())
	}
	return outBuf.String(), errBuf.String()
}

// bootIdentityHelper boots start-claude.sh with an identity repo configured so
// the git credential helper gets installed under $tmpHome/.local/bin, and
// returns the boot output. The clone target defaults to a nonexistent repo, so
// the in-boot clone soft-fails harmlessly; callers then invoke the installed
// helper directly to assert which credential it emits. extraEnv entries are
// appended (e.g. GH_TOKEN / USER_GITHUB_TOKEN) in KEY=value form.
func bootIdentityHelper(t *testing.T, tmpHome, identityRepo string, extraEnv ...string) string {
	t.Helper()
	mock := mockserver.New()
	ts := httptest.NewServer(mock)
	t.Cleanup(ts.Close)
	cpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(204)
	}))
	t.Cleanup(cpServer.Close)
	refreshToken := seedRefreshToken(t, mock, ts.URL)

	env := []string{
		"HOME=" + tmpHome,
		"PATH=" + testPATH(),
		"CLAUDE_REFRESH_TOKEN=" + refreshToken,
		"AGENT_NAME=unit-test",
		"ANTHROPIC_TOKEN_URL=" + ts.URL + "/v1/oauth/token",
		"KYBER_REFRESH_TOKEN_URL=" + cpServer.URL + "/internal/agents/unit-test/refresh-token",
		"KYBER_IDENTITY_REPO=" + identityRepo,
		"SKIP_CLAUDE_LAUNCH=1",
		"KYBER_SKIP_RUNTIME_PROBE=1",
	}
	env = append(env, extraEnv...)
	out, err := runScript(t, env)
	if err != nil {
		t.Fatalf("boot failed: %v\noutput:\n%s", err, out)
	}
	return string(out)
}

// TestStartClaude_NonIdentityRepo_CredentialHelperEmitsPAT pins the kyber#508
// Stage 3/4 contract for NON-identity repos: the generic PAT ($GH_TOKEN) is only
// for repos OTHER than the agent's own identity repo. Boot installs the helper;
// invoking it with a path that does NOT match KYBER_IDENTITY_REPO must emit the
// PAT as the git password. (The identity repo itself goes through the Kyber
// Platform App flow — covered by the mint/fail-loud tests below.)
func TestStartClaude_NonIdentityRepo_CredentialHelperEmitsPAT(t *testing.T) {
	tmpHome := t.TempDir()
	identityRepo := "matty-v/does-not-exist-" + fmt.Sprint(time.Now().UnixNano())
	const pat = "ghp_kyber508_nonidentity_pat"

	bootIdentityHelper(t, tmpHome, identityRepo, "GH_TOKEN="+pat)

	helper := wantHelperPath(tmpHome)
	if _, err := os.Stat(helper); err != nil {
		t.Fatalf("credential helper not installed at %s: %v", helper, err)
	}
	// path != KYBER_IDENTITY_REPO → non-identity → generic PAT.
	stdout, stderr := runCredentialHelperReq(t, helper, []string{
		"GH_TOKEN=" + pat,
		"PATH=" + testPATH(),
		"KYBER_IDENTITY_REPO=" + identityRepo,
	}, "protocol=https\nhost=github.com\npath=matty-v/some-other-repo.git\n\n")
	if !strings.Contains(stdout, "password="+pat) {
		t.Errorf("helper did not emit the PAT as password for a non-identity repo; got stdout:\n%s\nstderr:\n%s", stdout, stderr)
	}
}

// TestStartClaude_NonIdentityRepo_HelperFallsBackToUserGithubToken verifies the
// non-identity emit_pat path reads $USER_GITHUB_TOKEN when $GH_TOKEN isn't set.
// Same repo-routing rule (path != identity → PAT), exercising the fallback var.
func TestStartClaude_NonIdentityRepo_HelperFallsBackToUserGithubToken(t *testing.T) {
	tmpHome := t.TempDir()
	identityRepo := "matty-v/does-not-exist-" + fmt.Sprint(time.Now().UnixNano())
	const pat = "ghp_kyber508_user_secret_pat"

	bootIdentityHelper(t, tmpHome, identityRepo, "USER_GITHUB_TOKEN="+pat) // GH_TOKEN deliberately unset

	helper := wantHelperPath(tmpHome)
	stdout, stderr := runCredentialHelperReq(t, helper, []string{
		"USER_GITHUB_TOKEN=" + pat, // GH_TOKEN deliberately unset
		"PATH=" + testPATH(),
		"KYBER_IDENTITY_REPO=" + identityRepo,
	}, "protocol=https\nhost=github.com\npath=matty-v/some-other-repo.git\n\n")
	if !strings.Contains(stdout, "password="+pat) {
		t.Errorf("helper did not fall back to USER_GITHUB_TOKEN; got stdout:\n%s\nstderr:\n%s", stdout, stderr)
	}
}

// TestStartClaude_IdentityRepo_NoPATStillInstallsHelperAndClones pins that the
// identity-repo setup is NOT PAT-gated (kyber#508): with KYBER_IDENTITY_REPO set
// but NO PAT in the environment, boot still installs the credential helper and
// reaches the clone attempt — a PAT-less (e.g. freshly scaffolded) agent clones
// its identity repo through the Kyber Platform App flow, not a PAT. The old
// premise (no PAT ⇒ skip clone) is obsolete.
func TestStartClaude_IdentityRepo_NoPATStillInstallsHelperAndClones(t *testing.T) {
	tmpHome := t.TempDir()
	identityRepo := "matty-v/does-not-exist-" + fmt.Sprint(time.Now().UnixNano())

	// No GH_TOKEN, no USER_GITHUB_TOKEN. bootIdentityHelper t.Fatalf's on a
	// non-zero exit, so reaching here already proves boot did not crash-loop.
	s := bootIdentityHelper(t, tmpHome, identityRepo)

	helper := wantHelperPath(tmpHome)
	if _, err := os.Stat(helper); err != nil {
		t.Fatalf("credential helper must be installed even without a PAT (identity setup is not PAT-gated): %v", err)
	}
	if !strings.Contains(s, "cloning identity repo") {
		t.Errorf("expected clone attempt even without a PAT (identity setup not PAT-gated); got:\n%s", s)
	}
	// NOTE: no assertion that the output lacks "not readable" — the reworked
	// helper legitimately writes that to stderr when the App flow can't run.
}

// TestStartClaude_IdentityRepo_HelperMintsAppToken is the kyber#508 Stage 3/4
// happy path: for the agent's OWN identity repo the helper mints a short-lived
// token from the control plane (authenticated with the pod-token) and emits it —
// NOT the generic PAT, even when a PAT is present in the environment.
func TestStartClaude_IdentityRepo_HelperMintsAppToken(t *testing.T) {
	tmpHome := t.TempDir()
	identityRepo := "matty-v/does-not-exist-" + fmt.Sprint(time.Now().UnixNano())
	const pat = "ghp_should_not_be_used_for_identity"

	bootIdentityHelper(t, tmpHome, identityRepo, "GH_TOKEN="+pat)
	helper := wantHelperPath(tmpHome)

	// Stub control plane that mints the scoped identity-repo token.
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/agents/unit-test/identity-repo-token" {
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"token":"ghs_scoped_test","expires_at":"2027-01-01T00:00:00Z"}`)
	}))
	defer cp.Close()

	podTok := filepath.Join(t.TempDir(), "pod-token")
	if err := os.WriteFile(podTok, []byte("pod-token-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	stdout, stderr := runCredentialHelperReq(t, helper, []string{
		"HOME=" + tmpHome,
		"PATH=" + testPATH(),
		"GH_TOKEN=" + pat,
		"KYBER_IDENTITY_REPO=" + identityRepo,
		"AGENT_NAME=unit-test",
		"KYBER_CONTROL_PLANE_INTERNAL_URL=" + cp.URL,
		"KYBER_POD_TOKEN_PATH=" + podTok,
	}, "protocol=https\nhost=github.com\npath="+identityRepo+".git\n\n")

	if !strings.Contains(stdout, "password=ghs_scoped_test") {
		t.Errorf("helper did not emit the minted App token as password; got stdout:\n%s\nstderr:\n%s", stdout, stderr)
	}
	if strings.Contains(stdout, pat) {
		t.Errorf("identity-repo credential must be the minted App token, NOT the generic PAT; got stdout:\n%s", stdout)
	}
}

// TestStartClaude_IdentityRepo_HelperFailsLoudNoPATFallback proves there is NO
// PAT fallback on the identity-repo path (kyber#508 Stage 3/4): when the App
// mint can't run (here, an unreadable pod-token) the helper emits NOTHING on
// stdout and a diagnostic on stderr so git fails loudly — even though a valid
// PAT is present in the environment.
func TestStartClaude_IdentityRepo_HelperFailsLoudNoPATFallback(t *testing.T) {
	tmpHome := t.TempDir()
	identityRepo := "matty-v/does-not-exist-" + fmt.Sprint(time.Now().UnixNano())
	const pat = "ghp_present_but_must_not_be_emitted"

	bootIdentityHelper(t, tmpHome, identityRepo, "GH_TOKEN="+pat)
	helper := wantHelperPath(tmpHome)

	stdout, stderr := runCredentialHelperReq(t, helper, []string{
		"HOME=" + tmpHome,
		"PATH=" + testPATH(),
		"GH_TOKEN=" + pat,
		"KYBER_IDENTITY_REPO=" + identityRepo,
		"AGENT_NAME=unit-test",
		"KYBER_CONTROL_PLANE_INTERNAL_URL=http://127.0.0.1:1/unused",
		"KYBER_POD_TOKEN_PATH=" + filepath.Join(t.TempDir(), "does-not-exist"),
	}, "protocol=https\nhost=github.com\npath="+identityRepo+".git\n\n")

	if strings.Contains(stdout, "password=") {
		t.Errorf("identity-repo credential must NOT fall back to the PAT on failure; got stdout:\n%s", stdout)
	}
	if !strings.Contains(stderr, "Kyber Platform App failed") {
		t.Errorf("expected fail-loud diagnostic on stderr; got stderr:\n%s", stderr)
	}
}

// TestStartClaude_IdentityRepo_SetsUseHttpPath pins that boot enables
// credential.useHttpPath for https://github.com. It is load-bearing for the
// no-fallback design: git only passes path=owner/repo to the helper when this
// is on, so without it the helper can't tell the identity repo from any other
// and would mask the App flow with the PAT.
func TestStartClaude_IdentityRepo_SetsUseHttpPath(t *testing.T) {
	tmpHome := t.TempDir()
	identityRepo := "matty-v/does-not-exist-" + fmt.Sprint(time.Now().UnixNano())

	bootIdentityHelper(t, tmpHome, identityRepo, "GH_TOKEN=ghp_fake_pat_for_helper_install")

	gc := exec.Command("git", "config", "--global", "--get", "credential.https://github.com.useHttpPath")
	gc.Env = []string{"HOME=" + tmpHome, "PATH=" + testPATH()}
	out, err := gc.CombinedOutput()
	if err != nil {
		t.Fatalf("git config --get useHttpPath failed: %v\n%s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != "true" {
		t.Errorf("credential.https://github.com.useHttpPath = %q, want \"true\"", got)
	}
}

// TestStartClaude_IdentityRepo_SyncsDefaultBranchNotParkedBranch is the kyber#542
// regression: on restart the boot sync must move the identity repo to the DEFAULT
// branch before pulling. Agents routinely end a task parked on a feature branch;
// the old code `git pull --ff-only`'d whatever branch was checked out and never
// touched main, so merged skill/contract updates never arrived — silently. This
// test parks a clone on a feature branch, advances the remote's main, boots, and
// asserts the repo ends up on main at the new commit (not the stale parked branch).
func TestStartClaude_IdentityRepo_SyncsDefaultBranchNotParkedBranch(t *testing.T) {
	mock := mockserver.New()
	ts := httptest.NewServer(mock)
	defer ts.Close()
	// The boot sync fetches with a short-lived Kyber Platform App token minted from
	// the control plane (kyber#508 Stage 3/4 — no PAT fallback). Serve a real token
	// on that route so the fetch path actually executes; everything else (the
	// refresh-token rotation push) still wants a 204.
	const appToken = "test-app-token"
	cpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/identity-repo-token") {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"token":%q}`, appToken)
			return
		}
		w.WriteHeader(204)
	}))
	defer cpServer.Close()
	refreshToken := seedRefreshToken(t, mock, ts.URL)

	tmpHome := t.TempDir()
	podTokenPath := filepath.Join(tmpHome, "pod-token")
	if err := os.WriteFile(podTokenPath, []byte("pod-token-value"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Isolated git identity/config for the test's own setup commits (start-claude
	// uses its own $HOME/.gitconfig; the file:// remote needs no credential helper).
	gitEnv := []string{
		"HOME=" + tmpHome, "PATH=" + testPATH(),
		"GIT_CONFIG_GLOBAL=" + filepath.Join(tmpHome, ".gitconfig-setup"),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
	}
	git := func(dir string, args ...string) {
		t.Helper()
		c := exec.Command("git", args...)
		c.Dir = dir
		c.Env = gitEnv
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("setup git %v in %q: %v\n%s", args, dir, err, out)
		}
	}
	gitOut := func(dir string, args ...string) string {
		t.Helper()
		c := exec.Command("git", args...)
		c.Dir = dir
		c.Env = gitEnv
		out, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("setup git %v in %q: %v\n%s", args, dir, err, out)
		}
		return strings.TrimSpace(string(out))
	}

	// Bare "remote" with one commit on main (the default branch).
	work := t.TempDir()
	remote := filepath.Join(work, "remote.git")
	git(work, "init", "--bare", "-b", "main", remote)
	seed := filepath.Join(work, "seed")
	git(work, "clone", remote, seed)
	if err := os.WriteFile(filepath.Join(seed, "marker"), []byte("A"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(seed, "add", "-A")
	git(seed, "commit", "-m", "A")
	git(seed, "push", "origin", "main")

	// Pre-create REPO_DIR = $HOME/dev/<name> as a clone PARKED on a feature branch.
	repoName := "test-ident-" + fmt.Sprint(time.Now().UnixNano())
	repoDir := filepath.Join(tmpHome, "dev", repoName)
	if err := os.MkdirAll(filepath.Dir(repoDir), 0o755); err != nil {
		t.Fatal(err)
	}
	git(work, "clone", remote, repoDir)
	git(repoDir, "checkout", "-b", "feat/parked")

	// The sync fetches from https://x-access-token:<minted>@github.com/<slug>.git.
	// Rewrite exactly that URL to the local bare remote so the PRODUCTION code path
	// runs unmodified — mint, build the URL, fetch — with no test-only seam in
	// start-claude.sh and no network. The token is the one cpServer mints above.
	git(repoDir, "config",
		fmt.Sprintf("url.%s.insteadOf", remote),
		fmt.Sprintf("https://x-access-token:%s@github.com/matty-v/%s.git", appToken, repoName))

	// Advance the remote's main AFTER the agent parked (the "merged update").
	if err := os.WriteFile(filepath.Join(seed, "marker"), []byte("B"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(seed, "commit", "-am", "B")
	git(seed, "push", "origin", "main")

	out, err := runScript(t, []string{
		"HOME=" + tmpHome,
		"PATH=" + testPATH(),
		"CLAUDE_REFRESH_TOKEN=" + refreshToken,
		"AGENT_NAME=unit-test",
		"ANTHROPIC_TOKEN_URL=" + ts.URL + "/v1/oauth/token",
		"KYBER_REFRESH_TOKEN_URL=" + cpServer.URL + "/internal/agents/unit-test/refresh-token",
		"KYBER_CONTROL_PLANE_INTERNAL_URL=" + cpServer.URL,
		"KYBER_POD_TOKEN_PATH=" + podTokenPath,
		"KYBER_IDENTITY_REPO=matty-v/" + repoName,
		"GH_TOKEN=ghp_fake_pat_for_helper_install",
		"SKIP_CLAUDE_LAUNCH=1",
	})
	if err != nil {
		t.Fatalf("script failed: %v\noutput:\n%s", err, out)
	}

	if cur := gitOut(repoDir, "rev-parse", "--abbrev-ref", "HEAD"); cur != "main" {
		t.Errorf("repo should be on 'main' after boot, got %q\noutput:\n%s", cur, out)
	}
	if b, _ := os.ReadFile(filepath.Join(repoDir, "marker")); string(b) != "B" {
		t.Errorf("repo should have pulled the merged update (marker 'B'), got %q — boot synced the parked branch, not main\noutput:\n%s", b, out)
	}
	if !strings.Contains(string(out), "was on 'feat/parked'") {
		t.Errorf("expected off-default-branch WARNING, got:\n%s", out)
	}
}

// TestStartClaude_IdentityRepo_SyncWithoutAppTokenSkipsFetch pins the other half of
// the kyber#508 Stage 3/4 contract: with no mintable App token the sync must NOT
// fall back to the PAT, must say so, and must still do the work that needs no
// network — the kyber#542 default-branch checkout. Without this, "the fetch didn't
// happen" and "the branch protection didn't happen" look identical in the logs.
func TestStartClaude_IdentityRepo_SyncWithoutAppTokenSkipsFetch(t *testing.T) {
	mock := mockserver.New()
	ts := httptest.NewServer(mock)
	defer ts.Close()
	// 204 everywhere, including the mint route — an unconfigured/broken App path.
	cpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(204)
	}))
	defer cpServer.Close()
	refreshToken := seedRefreshToken(t, mock, ts.URL)

	tmpHome := t.TempDir()
	gitEnv := []string{
		"HOME=" + tmpHome, "PATH=" + testPATH(),
		"GIT_CONFIG_GLOBAL=" + filepath.Join(tmpHome, ".gitconfig-setup"),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
	}
	git := func(dir string, args ...string) {
		t.Helper()
		c := exec.Command("git", args...)
		c.Dir = dir
		c.Env = gitEnv
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("setup git %v in %q: %v\n%s", args, dir, err, out)
		}
	}

	work := t.TempDir()
	remote := filepath.Join(work, "remote.git")
	git(work, "init", "--bare", "-b", "main", remote)
	seed := filepath.Join(work, "seed")
	git(work, "clone", remote, seed)
	if err := os.WriteFile(filepath.Join(seed, "marker"), []byte("A"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(seed, "add", "-A")
	git(seed, "commit", "-m", "A")
	git(seed, "push", "origin", "main")

	repoName := "test-noapp-" + fmt.Sprint(time.Now().UnixNano())
	repoDir := filepath.Join(tmpHome, "dev", repoName)
	if err := os.MkdirAll(filepath.Dir(repoDir), 0o755); err != nil {
		t.Fatal(err)
	}
	git(work, "clone", remote, repoDir)
	git(repoDir, "checkout", "-b", "feat/parked")

	out, err := runScript(t, []string{
		"HOME=" + tmpHome,
		"PATH=" + testPATH(),
		"CLAUDE_REFRESH_TOKEN=" + refreshToken,
		"AGENT_NAME=unit-test",
		"ANTHROPIC_TOKEN_URL=" + ts.URL + "/v1/oauth/token",
		"KYBER_REFRESH_TOKEN_URL=" + cpServer.URL + "/internal/agents/unit-test/refresh-token",
		"KYBER_CONTROL_PLANE_INTERNAL_URL=" + cpServer.URL,
		"KYBER_IDENTITY_REPO=matty-v/" + repoName,
		"GH_TOKEN=ghp_fake_pat_for_helper_install",
		"SKIP_CLAUDE_LAUNCH=1",
	})
	if err != nil {
		t.Fatalf("script failed: %v\noutput:\n%s", err, out)
	}

	if !strings.Contains(string(out), "could not obtain Kyber Platform App token") {
		t.Errorf("expected the no-App-token skip to be logged loudly, got:\n%s", out)
	}
	if strings.Contains(string(out), "synced") {
		t.Errorf("sync reported success with no App token — the PAT fallback must stay dead:\n%s", out)
	}
	// The network-free half still has to run.
	c := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD")
	c.Dir = repoDir
	c.Env = gitEnv
	head, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("read HEAD: %v\n%s", err, head)
	}
	if got := strings.TrimSpace(string(head)); got != "main" {
		t.Errorf("kyber#542 checkout must happen even without a token, got %q\noutput:\n%s", got, out)
	}
}

// TestStartClaude_IdentityRepo_WiresMemorySymlinkIntoRepo pins kyber#625: the
// boot sync migrates any pre-existing native Claude Code memory
// (~/.claude/projects/<cwd-slug>/memory) into the identity repo's memory/ and
// replaces the native dir with a symlink to it, so the auto-memory hook +
// /compact-memory persist curated memory instead of leaving it untracked on the
// pod's disk.
func TestStartClaude_IdentityRepo_WiresMemorySymlinkIntoRepo(t *testing.T) {
	mock := mockserver.New()
	ts := httptest.NewServer(mock)
	defer ts.Close()
	cpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(204)
	}))
	defer cpServer.Close()
	refreshToken := seedRefreshToken(t, mock, ts.URL)

	tmpHome := t.TempDir()
	gitEnv := []string{
		"HOME=" + tmpHome, "PATH=" + testPATH(),
		"GIT_CONFIG_GLOBAL=" + filepath.Join(tmpHome, ".gitconfig-setup"),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
	}
	git := func(dir string, args ...string) {
		t.Helper()
		c := exec.Command("git", args...)
		c.Dir = dir
		c.Env = gitEnv
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("setup git %v in %q: %v\n%s", args, dir, err, out)
		}
	}

	// Bare remote with one commit on main, cloned to REPO_DIR = $HOME/dev/<name>.
	work := t.TempDir()
	remote := filepath.Join(work, "remote.git")
	git(work, "init", "--bare", "-b", "main", remote)
	seed := filepath.Join(work, "seed")
	git(work, "clone", remote, seed)
	if err := os.WriteFile(filepath.Join(seed, "marker"), []byte("A"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(seed, "add", "-A")
	git(seed, "commit", "-m", "A")
	git(seed, "push", "origin", "main")

	repoName := "test-mem-" + fmt.Sprint(time.Now().UnixNano())
	repoDir := filepath.Join(tmpHome, "dev", repoName)
	if err := os.MkdirAll(filepath.Dir(repoDir), 0o755); err != nil {
		t.Fatal(err)
	}
	git(work, "clone", remote, repoDir)

	// Pre-seed native Claude Code memory at ~/.claude/projects/<cwd-slug>/memory,
	// where cwd-slug is REPO_DIR with '/'→'-' (matches start-claude.sh).
	slug := strings.ReplaceAll(repoDir, "/", "-")
	nativeMem := filepath.Join(tmpHome, ".claude", "projects", slug, "memory")
	if err := os.MkdirAll(nativeMem, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nativeMem, "MEMORY.md"), []byte("# index\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nativeMem, "project_alpha.md"), []byte("alpha\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := runScript(t, []string{
		"HOME=" + tmpHome,
		"PATH=" + testPATH(),
		"CLAUDE_REFRESH_TOKEN=" + refreshToken,
		"AGENT_NAME=unit-test",
		"ANTHROPIC_TOKEN_URL=" + ts.URL + "/v1/oauth/token",
		"KYBER_REFRESH_TOKEN_URL=" + cpServer.URL + "/internal/agents/unit-test/refresh-token",
		"KYBER_IDENTITY_REPO=matty-v/" + repoName,
		"GH_TOKEN=ghp_fake_pat_for_helper_install",
		"SKIP_CLAUDE_LAUNCH=1",
	})
	if err != nil {
		t.Fatalf("script failed: %v\noutput:\n%s", err, out)
	}

	// Native memory dir must now be a symlink → repoDir/memory.
	fi, err := os.Lstat(nativeMem)
	if err != nil {
		t.Fatalf("lstat native memory: %v\noutput:\n%s", err, out)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("native memory should be a symlink, got mode %v\noutput:\n%s", fi.Mode(), out)
	}
	target, err := os.Readlink(nativeMem)
	if err != nil {
		t.Fatalf("readlink: %v", err)
	}
	if target != filepath.Join(repoDir, "memory") {
		t.Errorf("symlink target: got %q, want %q", target, filepath.Join(repoDir, "memory"))
	}

	// Migrated files must exist in the repo's memory/.
	for _, f := range []string{"MEMORY.md", "project_alpha.md"} {
		if _, err := os.Stat(filepath.Join(repoDir, "memory", f)); err != nil {
			t.Errorf("migrated file %s missing from repo memory/: %v", f, err)
		}
	}
	// And the native path must resolve (through the symlink) to the same content.
	if b, err := os.ReadFile(filepath.Join(nativeMem, "project_alpha.md")); err != nil || string(b) != "alpha\n" {
		t.Errorf("native path should resolve migrated file through symlink, got %q err=%v", b, err)
	}
}

// --- kyber#418 — idempotent git credential-helper setup -----------------
//
// Regression for the boot crash where an agent whose PERSISTED ~/.gitconfig
// (on the PVC) already held MULTIPLE values for
// credential.https://github.com.helper crashed boot. The single-value set at
// start-claude.sh:152 failed with "cannot overwrite multiple values with a
// single value" under `set -euo pipefail` (exit 5 → agent container exit 5 →
// stuck Failed, no auto-recovery because the bad config is persisted). The
// duplicate arose from an earlier boot revision that used `gh auth setup-git`
// (writes an empty `helper =` reset + the gh helper). These tests pin the fix:
// the credential-helper step must tolerate 0, 1, or N pre-existing values,
// exit 0, and leave exactly one effective value.

// runIdentityBoot runs start-claude.sh through the identity-repo block (which
// is where the credential-helper set lives) against the OAuth + CP mock
// servers, with HOME = home and a GitHub PAT in $GH_TOKEN so the credential-
// helper set executes. The clone target is a nonexistent repo so the clone
// soft-fails harmlessly — we only care that the credential-helper set runs.
// Returns the combined output and the script's exit error (nil on clean boot).
//
// Post-kyber#509: git auth no longer comes from an in-platform App-token mount
// (KYBER_IDENTITY_TOKEN_PATH); it rides the generic PAT user-secret, so the
// gate that drives the helper-install branch is $GH_TOKEN being non-empty.
func runIdentityBoot(t *testing.T, home string) ([]byte, error) {
	t.Helper()
	mock := mockserver.New()
	ts := httptest.NewServer(mock)
	defer ts.Close()

	cpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(204)
	}))
	defer cpServer.Close()

	refreshToken := seedRefreshToken(t, mock, ts.URL)

	return runScript(t, []string{
		"HOME=" + home,
		"PATH=" + testPATH(),
		"CLAUDE_REFRESH_TOKEN=" + refreshToken,
		"AGENT_NAME=unit-test",
		"ANTHROPIC_TOKEN_URL=" + ts.URL + "/v1/oauth/token",
		"KYBER_REFRESH_TOKEN_URL=" + cpServer.URL + "/internal/agents/unit-test/refresh-token",
		"KYBER_IDENTITY_REPO=matty-v/does-not-exist-" + fmt.Sprint(time.Now().UnixNano()),
		"GH_TOKEN=ghp_fake_pat_for_helper_install",
		"SKIP_CLAUDE_LAUNCH=1",
	})
}

// effectiveHelpers returns all values of credential.https://github.com.helper
// from the gitconfig under home.
func effectiveHelpers(t *testing.T, home string) []string {
	t.Helper()
	gc := exec.Command("git", "config", "--global", "--get-all", "credential.https://github.com.helper")
	gc.Env = []string{"HOME=" + home, "PATH=" + testPATH()}
	out, err := gc.CombinedOutput()
	if err != nil {
		// exit status 1 with empty output = key absent; any other failure is real.
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 && len(out) == 0 {
			return nil
		}
		t.Fatalf("git config --get-all failed: %v\n%s", err, out)
	}
	trimmed := strings.TrimRight(string(out), "\n")
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "\n")
}

func wantHelperPath(home string) string {
	return filepath.Join(home, ".local", "bin", "git-credential-kyber-github")
}

// TestStartClaude_CredentialHelper_ToleratesPreexistingMultipleValues is the
// core kyber#418 regression: boot must not crash when the persisted gitconfig
// already has multiple helper values, and must converge to a single effective
// value equal to the freshly-written helper path.
func TestStartClaude_CredentialHelper_ToleratesPreexistingMultipleValues(t *testing.T) {
	home := t.TempDir()
	// Exactly the state `gh auth setup-git` left on older agents: an empty
	// reset + the gh helper. This is what wedged Yoda for ~3.5h.
	gitconfig := "[credential \"https://github.com\"]\n\thelper = \n\thelper = !/usr/bin/gh auth git-credential\n"
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte(gitconfig), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := runIdentityBoot(t, home)
	if err != nil {
		t.Fatalf("boot crashed on pre-existing multiple credential.helper values (kyber#418 regression): %v\noutput:\n%s", err, out)
	}

	got := effectiveHelpers(t, home)
	if len(got) != 1 {
		t.Fatalf("expected exactly one effective helper value, got %d: %q", len(got), got)
	}
	if got[0] != wantHelperPath(home) {
		t.Errorf("effective helper = %q, want %q", got[0], wantHelperPath(home))
	}
}

// TestStartClaude_CredentialHelper_IdempotentAcrossRestarts pins the
// idempotency AC: repeated boots from the same PVC (same HOME) neither
// accumulate helper values nor crash.
func TestStartClaude_CredentialHelper_IdempotentAcrossRestarts(t *testing.T) {
	home := t.TempDir()
	for i := 1; i <= 3; i++ {
		out, err := runIdentityBoot(t, home)
		if err != nil {
			t.Fatalf("boot %d crashed: %v\noutput:\n%s", i, err, out)
		}
		got := effectiveHelpers(t, home)
		if len(got) != 1 {
			t.Fatalf("after boot %d: expected exactly one helper value, got %d: %q", i, len(got), got)
		}
		if got[0] != wantHelperPath(home) {
			t.Errorf("after boot %d: effective helper = %q, want %q", i, got[0], wantHelperPath(home))
		}
	}
}

// TestStartClaude_CredentialHelper_FirstBootNoPreexisting is the
// non-regression AC: an agent with NO pre-existing credential config still
// boots and ends with a single working helper (first-boot path unchanged).
func TestStartClaude_CredentialHelper_FirstBootNoPreexisting(t *testing.T) {
	home := t.TempDir()
	out, err := runIdentityBoot(t, home)
	if err != nil {
		t.Fatalf("first boot (no pre-existing config) crashed: %v\noutput:\n%s", err, out)
	}
	got := effectiveHelpers(t, home)
	if len(got) != 1 {
		t.Fatalf("expected exactly one helper value on first boot, got %d: %q", len(got), got)
	}
	if got[0] != wantHelperPath(home) {
		t.Errorf("effective helper = %q, want %q", got[0], wantHelperPath(home))
	}
}

func containsScope(scopes []string, target string) bool {
	for _, s := range scopes {
		if s == target {
			return true
		}
	}
	return false
}

// TestStartClaude_AccessTokenStillValid_SkipsRefresh verifies that when the
// access_token has > 5min remaining, the script writes credentials.json without
// calling the Anthropic refresh endpoint at all.
func TestStartClaude_AccessTokenStillValid_SkipsRefresh(t *testing.T) {
	calls := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		http.Error(w, "should not be called when access_token is fresh", 500)
	}))
	defer ts.Close()

	tmp := t.TempDir()
	futureMs := time.Now().Add(30 * time.Minute).UnixMilli()

	cmd := exec.Command("/bin/bash", scriptPath(t))
	// Hermetic env (kyber#551): no host-environment inheritance — on an agent pod the real
	// CLAUDE_*/KYBER_* vars leak into the script and flip its code paths.
	cmd.Env = []string{
		"HOME=" + tmp,
		"PATH=" + testPATH(),
		"AGENT_NAME=unit-test",
		"ANTHROPIC_TOKEN_URL=" + ts.URL,
		"CLAUDE_ACCESS_TOKEN=cached-access-token",
		"CLAUDE_REFRESH_TOKEN=any-refresh",
		fmt.Sprintf("CLAUDE_ACCESS_TOKEN_EXPIRES_AT=%d", futureMs),
		"KYBER_REFRESH_TOKEN_URL=http://127.0.0.1:1/should-not-be-called",
		"SKIP_CLAUDE_LAUNCH=1",
		"KYBER_SKIP_RUNTIME_PROBE=1",
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("script failed: %v\n%s", err, out)
	}
	if calls != 0 {
		t.Errorf("Anthropic was called %d times — expected 0 (token still valid)", calls)
	}

	credsPath := filepath.Join(tmp, ".claude", ".credentials.json")
	data, err := os.ReadFile(credsPath)
	if err != nil {
		t.Fatalf("read creds: %v", err)
	}
	if !strings.Contains(string(data), "cached-access-token") {
		t.Errorf("creds.json doesn't contain cached access_token: %s", data)
	}
}

// TestStartClaude_BootAfterRotation_UsesNewToken simulates three consecutive
// boots with rotating mock Anthropic + recording mock control-plane:
//  1. Fresh: refresh runs, rotation push records new tokens, secret updated.
//  2. Cached: access_token still valid, Anthropic NOT called.
//  3. Expired: refresh runs again with the rotated refresh_token from boot 1.
func TestStartClaude_BootAfterRotation_UsesNewToken(t *testing.T) {
	type creds struct {
		Access, Refresh string
		ExpiresAt       int64
	}
	current := creds{Access: "A0", Refresh: "R0", ExpiresAt: 0}

	cpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			ExpiresAt    int64  `json:"expires_at"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.AccessToken != "" {
			current.Access = body.AccessToken
		}
		if body.RefreshToken != "" {
			current.Refresh = body.RefreshToken
		}
		if body.ExpiresAt != 0 {
			current.ExpiresAt = body.ExpiresAt
		}
		w.WriteHeader(204)
	}))
	defer cpServer.Close()

	rotN := 0
	anthServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rotN++
		fmt.Fprintf(w, `{"access_token":"A%d","refresh_token":"R%d","expires_in":3600}`, rotN, rotN)
	}))
	defer anthServer.Close()

	runBoot := func(t *testing.T) {
		t.Helper()
		tmp := t.TempDir()
		cmd := exec.Command("/bin/bash", scriptPath(t))
		// Hermetic env (kyber#551): no host-environment inheritance — see the rotation test below.
		cmd.Env = []string{
			"HOME=" + tmp,
			"PATH=" + testPATH(),
			"AGENT_NAME=unit-test",
			"ANTHROPIC_TOKEN_URL=" + anthServer.URL,
			"KYBER_REFRESH_TOKEN_URL=" + cpServer.URL + "/internal/agents/unit-test/refresh-token",
			"CLAUDE_ACCESS_TOKEN=" + current.Access,
			"CLAUDE_REFRESH_TOKEN=" + current.Refresh,
			fmt.Sprintf("CLAUDE_ACCESS_TOKEN_EXPIRES_AT=%d", current.ExpiresAt),
			"SKIP_CLAUDE_LAUNCH=1",
			"KYBER_SKIP_RUNTIME_PROBE=1",
		}
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("boot failed: %v\n%s", err, out)
		}
	}

	// Boot 1: fresh — no expires_at → refreshes, gets A1/R1, pushes to CP.
	runBoot(t)
	if current.Access != "A1" || current.Refresh != "R1" {
		t.Errorf("after boot 1: got access=%s refresh=%s, want A1/R1", current.Access, current.Refresh)
	}
	if current.ExpiresAt == 0 {
		t.Errorf("after boot 1: expires_at not persisted")
	}

	// Boot 2: secret has A1/R1 + future expires_at → SKIP refresh entirely.
	rotBefore := rotN
	runBoot(t)
	if rotN != rotBefore {
		t.Errorf("boot 2 called Anthropic %d times — expected 0 (access_token still valid)", rotN-rotBefore)
	}

	// Boot 3: simulate expired access_token → refresh runs with R1.
	current.ExpiresAt = time.Now().Add(-1 * time.Hour).UnixMilli()
	runBoot(t)
	if current.Access != "A2" || current.Refresh != "R2" {
		t.Errorf("after boot 3: got access=%s refresh=%s, want A2/R2", current.Access, current.Refresh)
	}
}

// TestStartClaude_RotationPushFails_ExitsTwo verifies that when the rotation
// push fails (control-plane returns 500), the script exits 2 with FATAL —
// it does NOT silently continue with a stale secret.
func TestStartClaude_RotationPushFails_ExitsTwo(t *testing.T) {
	cpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "simulated cp outage", 500)
	}))
	defer cpServer.Close()

	anthServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"access_token":"A1","refresh_token":"R1","expires_in":3600}`)
	}))
	defer anthServer.Close()

	tmp := t.TempDir()
	cmd := exec.Command("/bin/bash", scriptPath(t))
	// Hermetic env (kyber#551): no host-environment inheritance. On an agent pod the inherited
	// CLAUDE_ACCESS_TOKEN(+_EXPIRES_AT) made the script skip the refresh —
	// so the FATAL-on-rotation-push-failure path under test never ran and the
	// expected exit 2 never happened (fails on freshly-rolled pods, passes on
	// pods whose token happens to be expired; CI was green by absence of the
	// vars). Explicit allowlist of exactly what the scenario needs.
	cmd.Env = []string{
		"HOME=" + tmp,
		"PATH=" + testPATH(),
		"AGENT_NAME=unit-test",
		"ANTHROPIC_TOKEN_URL=" + anthServer.URL,
		"KYBER_REFRESH_TOKEN_URL=" + cpServer.URL + "/internal/agents/unit-test/refresh-token",
		"CLAUDE_REFRESH_TOKEN=R0",
		"SKIP_CLAUDE_LAUNCH=1",
		"KYBER_SKIP_RUNTIME_PROBE=1",
	}
	out, err := cmd.CombinedOutput()
	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("expected exit error, got %v\n%s", err, out)
	}
	if exitErr.ExitCode() != 2 {
		t.Errorf("expected exit code 2, got %d\n%s", exitErr.ExitCode(), out)
	}
	if !strings.Contains(string(out), "FATAL") {
		t.Errorf("expected FATAL message in output, got: %s", out)
	}
}

// --- kyber#377 (PR-C) — boot-time CC install + data-driven [1m] ----------
//
// These tests exercise the new charset guard, install branch, and [1m]
// arithmetic gate added in /home/kyber/dev/kyber/images/claude-code/start-claude.sh.
// All of them set KYBER_BOOTPREP_DRY_RUN=1 so the script exits right
// after the boot-prep block — no tmux, no real claude, no real npm
// (npm is stubbed via PATH for tests that exercise the install branch).
//
// The charset guard is security-critical: it sits between an operator-
// writable CRD field (spec.runtimeVersion, reachable from the PWA in
// PR-D) and a `npm install` shell interpolation on a pod that holds
// OAuth tokens. Reject malformed input loud and continue with the
// baked-in version; never let it reach `npm`.

// stubNPMDir creates a tempdir with a stub `npm` script whose behavior
// is controlled by the script body the caller passes in. PATH-prepend
// the returned dir to make the stub win against any system npm.
func stubNPMDir(t *testing.T, scriptBody string) string {
	t.Helper()
	dir := t.TempDir()
	body := "#!/bin/bash\n" + scriptBody + "\n"
	npm := filepath.Join(dir, "npm")
	if err := os.WriteFile(npm, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// bootPrepRun runs start-claude.sh with KYBER_BOOTPREP_DRY_RUN=1 and a
// minimal env, returning the combined output for assertion. extraEnv
// entries override the defaults; pass them in `KEY=value` form.
func bootPrepRun(t *testing.T, extraEnv ...string) string {
	t.Helper()
	tmp := t.TempDir()
	// ANTHROPIC_API_KEY+no CLAUDE_ACCESS_TOKEN short-circuits the upstream
	// OAuth refresh block (which would otherwise FATAL on missing
	// KYBER_REFRESH_TOKEN_URL). We're not exercising auth here — just the
	// new boot-prep block — so the cheapest skip is the api-key path.
	env := []string{
		"HOME=" + tmp,
		"PATH=" + testPATH(),
		"KYBER_BOOTPREP_DRY_RUN=1",
		"ANTHROPIC_API_KEY=test-key",
	}
	env = append(env, extraEnv...)
	out, err := runScript(t, env)
	if err != nil {
		t.Fatalf("script failed unexpectedly under dry-run: %v\n%s", err, out)
	}
	return string(out)
}

func TestStartClaude_PRC_CharsetGuard_RejectsSemicolonInjection(t *testing.T) {
	out := bootPrepRun(t,
		"KYBER_REQUESTED_CC_VERSION=2.1.119;rm -rf /",
		"KYBER_RUNTIME_DEFAULT_VERSION=2.0.99",
	)
	if !strings.Contains(out, "rejected by charset guard") {
		t.Errorf("expected charset rejection log line; got: %s", out)
	}
	if strings.Contains(out, "installing @anthropic-ai/claude-code") {
		t.Errorf("install branch must NOT fire on rejected input; got: %s", out)
	}
	if !strings.Contains(out, "CC_INSTALL_OUTCOME=rejected-charset") {
		t.Errorf("expected outcome=rejected-charset; got: %s", out)
	}
}

func TestStartClaude_PRC_CharsetGuard_RejectsCommandSubstitution(t *testing.T) {
	out := bootPrepRun(t,
		`KYBER_REQUESTED_CC_VERSION=$(whoami)`,
		"KYBER_RUNTIME_DEFAULT_VERSION=2.0.99",
	)
	if !strings.Contains(out, "rejected by charset guard") {
		t.Errorf("expected charset rejection; got: %s", out)
	}
	if strings.Contains(out, "installing @anthropic-ai/claude-code") {
		t.Errorf("install must not fire; got: %s", out)
	}
}

func TestStartClaude_PRC_CharsetGuard_RejectsBacktickInjection(t *testing.T) {
	out := bootPrepRun(t,
		"KYBER_REQUESTED_CC_VERSION=`id`",
		"KYBER_RUNTIME_DEFAULT_VERSION=2.0.99",
	)
	if !strings.Contains(out, "rejected by charset guard") {
		t.Errorf("expected charset rejection; got: %s", out)
	}
}

func TestStartClaude_PRC_CharsetGuard_RejectsSpaces(t *testing.T) {
	out := bootPrepRun(t,
		"KYBER_REQUESTED_CC_VERSION=2.1.119 plus",
		"KYBER_RUNTIME_DEFAULT_VERSION=2.0.99",
	)
	if !strings.Contains(out, "rejected by charset guard") {
		t.Errorf("expected charset rejection; got: %s", out)
	}
}

func TestStartClaude_PRC_LengthCap_RejectsOverlongString(t *testing.T) {
	// 65 chars of digits/dots — passes charset, fails length cap (64).
	long := strings.Repeat("1", 65)
	out := bootPrepRun(t,
		"KYBER_REQUESTED_CC_VERSION="+long,
		"KYBER_RUNTIME_DEFAULT_VERSION=2.0.99",
	)
	if !strings.Contains(out, "exceeds 64 chars") {
		t.Errorf("expected length-cap rejection; got: %s", out)
	}
	if !strings.Contains(out, "CC_INSTALL_OUTCOME=rejected-length") {
		t.Errorf("expected outcome=rejected-length; got: %s", out)
	}
}

func TestStartClaude_PRC_RequestEmpty_NoInstall(t *testing.T) {
	out := bootPrepRun(t,
		"KYBER_REQUESTED_CC_VERSION=",
		"KYBER_RUNTIME_DEFAULT_VERSION=2.1.119",
	)
	if strings.Contains(out, "installing @anthropic-ai/claude-code") {
		t.Errorf("install must not fire when request is empty; got: %s", out)
	}
	if !strings.Contains(out, "CC_INSTALL_OUTCOME=not-requested") {
		t.Errorf("expected outcome=not-requested; got: %s", out)
	}
}

func TestStartClaude_PRC_RequestEqualsDefault_NoInstall(t *testing.T) {
	npmLog := filepath.Join(t.TempDir(), "npm.log")
	sudoLog := filepath.Join(t.TempDir(), "sudo.log")
	stub := stubInstallEnvDir(t, npmLog, 0, sudoLog, "2.0.99")
	out := bootPrepRun(t,
		"PATH="+stub+":"+testPATH(),
		"KYBER_REQUESTED_CC_VERSION=2.0.99",
		"KYBER_RUNTIME_DEFAULT_VERSION=2.0.99",
	)
	if strings.Contains(out, "installing @anthropic-ai/claude-code") {
		t.Errorf("install must not fire when request equals default; got: %s", out)
	}
	if !strings.Contains(out, "requested version already installed") {
		t.Errorf("expected installed-version skip log line; got: %s", out)
	}
	if !strings.Contains(out, "CC_INSTALL_OUTCOME=skipped-equal") {
		t.Errorf("expected outcome=skipped-equal; got: %s", out)
	}
}

// stubInstallEnvDir creates a tempdir with stub `npm`, `sudo`, and `claude`
// for the boot-time CC-install tests. The boot install runs as root
// (`sudo npm install -g ...` — the baked-in global claude is root-owned) and
// determines success by verifying `claude --version`, not npm's exit code.
//   - sudo:   records that it was invoked (to sudoLog), then exec's its args,
//     so `sudo <npm> install ...` actually runs the npm stub.
//   - npm:    records its args (to npmLog) and exits npmExit.
//   - claude: prints "<claudeVersion> (Claude Code)" for `--version`; exits 0
//     otherwise (e.g. the PR-E model probe).
func stubInstallEnvDir(t *testing.T, npmLog string, npmExit int, sudoLog, claudeVersion string) string {
	t.Helper()
	dir := t.TempDir()
	installedMarker := filepath.Join(dir, "installed")
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/bash\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write("sudo", fmt.Sprintf(`echo called > %q; if [[ "$1" == /usr/local/bin/kyber-harness-install ]]; then shift; exec "$(dirname "$0")/kyber-harness-install" "$@"; fi; exec "$@"`, sudoLog))
	// The shared installer owns npm staging and activation; this stub verifies
	// that the boot script resolves the requested version and delegates to it.
	write("npm", fmt.Sprintf(`if [[ "$1" == "view" ]]; then echo "2.9.1"; exit 0; fi; echo "$@" > %q; exit %d`, npmLog, npmExit))
	write("kyber-harness-install", fmt.Sprintf(`echo "$@" > %q; if [[ "$2" == %q ]] && [[ %d == 0 ]]; then touch %q; exit 0; fi; exit 1`, npmLog, claudeVersion, npmExit, installedMarker))
	write("claude", fmt.Sprintf(`if [[ "$*" == "--version"* ]]; then if [[ -f %q ]]; then echo %q; else echo '2.0.99 (Claude Code)'; fi; exit 0; fi; exit 0`, installedMarker, claudeVersion+" (Claude Code)"))
	return dir
}

func TestStartClaude_PRC_RequestDiffersFromDefault_NpmInstallFires(t *testing.T) {
	npmLog := filepath.Join(t.TempDir(), "npm.log")
	sudoLog := filepath.Join(t.TempDir(), "sudo.log")
	// npm succeeds AND claude --version reports the requested version → installed.
	stub := stubInstallEnvDir(t, npmLog, 0, sudoLog, "2.1.200")
	out := bootPrepRun(t,
		"PATH="+stub+":"+testPATH(),
		"KYBER_HARNESS_INSTALLER="+filepath.Join(stub, "kyber-harness-install"),
		"KYBER_REQUESTED_CC_VERSION=2.1.200",
		"KYBER_RUNTIME_DEFAULT_VERSION=2.0.99",
	)
	if !strings.Contains(out, "installing @anthropic-ai/claude-code@2.1.200") {
		t.Errorf("expected install log line for 2.1.200; got: %s", out)
	}
	if !strings.Contains(out, "CC install: succeeded (2.1.200)") {
		t.Errorf("expected success log line; got: %s", out)
	}
	if !strings.Contains(out, "CC_INSTALL_OUTCOME=installed") {
		t.Errorf("expected outcome=installed; got: %s", out)
	}
	// The install must run as ROOT — the global claude is root-owned, so a
	// non-root install silently no-ops (the bug this fixes).
	if _, err := os.Stat(sudoLog); err != nil {
		t.Errorf("expected the install to run via sudo (root); sudo stub was not invoked: %v", err)
	}
	// And with the expected package@version arg.
	npmArgs, err := os.ReadFile(npmLog)
	if err != nil {
		t.Fatalf("npm log file: %v", err)
	}
	if !strings.Contains(string(npmArgs), "@anthropic-ai/claude-code 2.1.200 claude") {
		t.Errorf("installer args: got %q, want atomic installer package/version/binary", string(npmArgs))
	}
}

func TestStartClaude_PRC_LatestAcceptsResolvedVersion(t *testing.T) {
	npmLog := filepath.Join(t.TempDir(), "npm.log")
	sudoLog := filepath.Join(t.TempDir(), "sudo.log")
	stub := stubInstallEnvDir(t, npmLog, 0, sudoLog, "2.9.1")
	out := bootPrepRun(t,
		"PATH="+stub+":"+testPATH(),
		"KYBER_HARNESS_INSTALLER="+filepath.Join(stub, "kyber-harness-install"),
		"KYBER_REQUESTED_CC_VERSION=latest",
		"KYBER_RUNTIME_DEFAULT_VERSION=2.0.99",
	)
	if !strings.Contains(out, "CC install: succeeded (2.9.1)") {
		t.Errorf("latest install was not accepted: %s", out)
	}
	if !strings.Contains(out, "CC_INSTALL_OUTCOME=installed") {
		t.Errorf("expected latest install outcome=installed: %s", out)
	}
}

func TestStartClaude_PRC_UsesSharedAtomicInstaller(t *testing.T) {
	// Regression for kyber#483: an interrupted prior `npm install -g` leaves a
	// hidden staging dir (.claude-code-<hash>) in the global node_modules. On
	// whole-disk-persistence agents that dir is saved to the PVC and survives
	// every reboot, so the next install fails forever with
	// `ENOTEMPTY: rename ... .claude-code-<hash>` and the agent is pinned to the
	// last good version, ignoring requestedVersion. The boot script must clear
	// stale staging dirs before installing.
	npmLog := filepath.Join(t.TempDir(), "npm.log")
	sudoLog := filepath.Join(t.TempDir(), "sudo.log")
	stub := stubInstallEnvDir(t, npmLog, 0, sudoLog, "2.1.200")

	out := bootPrepRun(t,
		"PATH="+stub+":"+testPATH(),
		"KYBER_HARNESS_INSTALLER="+filepath.Join(stub, "kyber-harness-install"),
		"KYBER_REQUESTED_CC_VERSION=2.1.200",
		"KYBER_RUNTIME_DEFAULT_VERSION=2.0.99",
	)
	if !strings.Contains(out, "CC install: succeeded (2.1.200)") {
		t.Errorf("shared atomic installer was not used successfully: %s", out)
	}
}

func TestStartClaude_PRC_InstallReportsOkButVersionUnchanged_Fails(t *testing.T) {
	// Regression for the broken-upgrade root cause: npm exits 0 but
	// `claude --version` still reports the OLD version (the global install
	// didn't take — e.g. EACCES against the root-owned prefix). The outcome
	// MUST be "failed", not a false "succeeded", so requestedSatisfied can't lie.
	npmLog := filepath.Join(t.TempDir(), "npm.log")
	sudoLog := filepath.Join(t.TempDir(), "sudo.log")
	stub := stubInstallEnvDir(t, npmLog, 0, sudoLog, "2.0.99") // claude still reports baked-in
	out := bootPrepRun(t,
		"PATH="+stub+":"+testPATH(),
		"KYBER_HARNESS_INSTALLER="+filepath.Join(stub, "kyber-harness-install"),
		"KYBER_REQUESTED_CC_VERSION=2.1.200",
		"KYBER_RUNTIME_DEFAULT_VERSION=2.0.99",
	)
	if !strings.Contains(out, "CC install: FAILED") {
		t.Errorf("expected install-FAILED log when version didn't change; got: %s", out)
	}
	if !strings.Contains(out, "CC_INSTALL_OUTCOME=failed") {
		t.Errorf("expected outcome=failed; got: %s", out)
	}
	// Non-fatal: the dry-run must still exit 0 (no crash-loop).
}

func TestStartClaude_PRC_NpmInstallFailure_NonFatal(t *testing.T) {
	// npm itself fails (registry outage / version-not-found); the version check
	// then sees the unchanged baked-in version → failed, but non-fatal.
	npmLog := filepath.Join(t.TempDir(), "npm.log")
	sudoLog := filepath.Join(t.TempDir(), "sudo.log")
	stub := stubInstallEnvDir(t, npmLog, 1, sudoLog, "2.0.99")
	out := bootPrepRun(t,
		"PATH="+stub+":"+testPATH(),
		"KYBER_HARNESS_INSTALLER="+filepath.Join(stub, "kyber-harness-install"),
		"KYBER_REQUESTED_CC_VERSION=2.1.200",
		"KYBER_RUNTIME_DEFAULT_VERSION=2.0.99",
	)
	if !strings.Contains(out, "CC install: FAILED") {
		t.Errorf("expected install-failure log; got: %s", out)
	}
	if !strings.Contains(out, "falling back to the previous verified install") {
		t.Errorf("expected fallback log; got: %s", out)
	}
	if !strings.Contains(out, "CC_INSTALL_OUTCOME=failed") {
		t.Errorf("expected outcome=failed; got: %s", out)
	}
	// The dry-run exits 0 — install failure must NOT crash-loop the pod.
	// (bootPrepRun would t.Fatalf if the script exited non-zero.)
}

func TestStartClaude_PRC_OneM_AppliedWhenWindowGTE1M(t *testing.T) {
	out := bootPrepRun(t,
		"CLAUDE_MODEL=claude-opus-4-7",
		"KYBER_MODEL_CONTEXT_WINDOW=1000000",
	)
	if !strings.Contains(out, "CLAUDE_MODEL=claude-opus-4-7[1m]") {
		t.Errorf("expected [1m] suffix at exactly 1M boundary; got: %s", out)
	}
}

func TestStartClaude_PRC_OneM_NotAppliedWhenWindowBelow1M(t *testing.T) {
	out := bootPrepRun(t,
		"CLAUDE_MODEL=claude-sonnet-4-5",
		"KYBER_MODEL_CONTEXT_WINDOW=200000",
	)
	// claude-sonnet-4-5 maps to "sonnet" via the family alias, then no [1m].
	if !strings.Contains(out, "CLAUDE_MODEL=sonnet ") {
		t.Errorf("expected family-alias 'sonnet' without [1m]; got: %s", out)
	}
	if strings.Contains(out, "[1m]") {
		t.Errorf("[1m] must NOT be applied for 200K window; got: %s", out)
	}
}

func TestStartClaude_PRC_OneM_NotAppliedToFamilyAlias(t *testing.T) {
	// claude-haiku-4-5 maps to "haiku" alias. Even if KYBER_MODEL_CONTEXT_WINDOW
	// reports 1M (Claude Code itself maps haiku alias to its highest concrete
	// model), the alias forms must NOT receive a [1m] suffix — only concrete
	// claude-* IDs do.
	out := bootPrepRun(t,
		"CLAUDE_MODEL=claude-haiku-4-5",
		"KYBER_MODEL_CONTEXT_WINDOW=1000000",
	)
	if !strings.Contains(out, "CLAUDE_MODEL=haiku ") {
		t.Errorf("expected alias 'haiku' without [1m]; got: %s", out)
	}
	if strings.Contains(out, "haiku[1m]") {
		t.Errorf("[1m] must NOT be applied to alias forms; got: %s", out)
	}
}

func TestStartClaude_PRC_OneM_NoHardcodedModelIDArmRemains(t *testing.T) {
	// Hard-pin the kyber#374 contract: no concrete model-id case arm
	// (e.g., `claude-opus-4-9)`) is allowed back into the script. A future
	// contributor adding such an arm would mean "to support a new 1M
	// model, edit this script and rebuild," exactly what PR-C removed.
	// The generic `claude-*)` glob arm is the allowed shape and is
	// excluded from the match.
	body, err := os.ReadFile(scriptPath(t))
	if err != nil {
		t.Fatal(err)
	}
	src := string(body)
	// Match `claude-opus-...)`, `claude-sonnet-...)`, `claude-haiku-...)`
	// followed by trailing case-arm syntax including [1m]. The
	// pre-PR-C arms looked like:
	//   claude-opus-4-7|claude-opus-4-6|claude-sonnet-4-6) CLAUDE_MODEL="${CLAUDE_MODEL}[1m]" ;;
	// Future-contributor copy-paste of that shape lights up here.
	bad := regexp.MustCompile(`claude-(opus|sonnet|haiku)-[0-9][^*\n]*\)\s*CLAUDE_MODEL=.*\[1m\]`)
	if bad.MatchString(src) {
		t.Errorf("found suspected hardcoded `[1m]` case arm on a concrete model ID — PR-C removed these; reintroducing them would re-break the kyber#374 contract")
	}
	// The family-alias arms (claude-sonnet-4 → "sonnet", etc.) WITHOUT a
	// [1m] suffix are fine — they're a Max-subscription compatibility
	// shim, not a 1M-context decision. The arithmetic gate stays
	// data-driven.
}

// --- kyber#379 (PR-E) — pre-flight probe + extended report body --------
//
// These tests exercise the probe block + the report-body extension that
// PR-E added to start-claude.sh. The probe and the install-outcome
// derivation both happen AFTER the boot-prep block, so we can't use
// KYBER_BOOTPREP_DRY_RUN=1 here — instead we exercise the script with a
// stubbed `claude` (via PATH) and a stubbed control-plane HTTP server
// (via KYBER_CONTROL_PLANE_INTERNAL_URL) so we can observe what the
// script reports without spinning up tmux.

// stubClaudeDir creates a tempdir with stub `claude` + `tmux` scripts.
// Test controls the probe outcome via scriptBody for `claude` (called as
// `claude --version` and `claude --model X --print 'ping'`); the tmux
// stub is a no-op so the launch line doesn't error.
func stubClaudeDir(t *testing.T, claudeBody string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte("#!/bin/bash\n"+claudeBody+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tmux"), []byte("#!/bin/bash\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Note: we deliberately do NOT stub `timeout` — the real coreutils
	// binary on PATH wraps our stub `claude` correctly because PATH is
	// inherited by the subprocess spawn.
	return dir
}

// probeRun runs start-claude.sh against stub binaries and a stub CP
// receiver. Returns (script combined output, parsed report body, error).
// The CP server records the last received body so tests can assert on
// the exact JSON the script POSTed.
func probeRun(t *testing.T, claudeStub string, extraEnv ...string) (string, []byte) {
	t.Helper()
	bin := stubClaudeDir(t, claudeStub)
	receivedBody := make(chan []byte, 1)
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, r.ContentLength)
		if r.ContentLength > 0 {
			_, _ = r.Body.Read(buf)
		}
		select {
		case receivedBody <- buf:
		default:
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(cp.Close)

	tmp := t.TempDir()
	env := []string{
		"HOME=" + tmp,
		"PATH=" + bin + ":" + testPATH(),
		"ANTHROPIC_API_KEY=test-key", // skip OAuth block
		"AGENT_NAME=test-agent",
		"KYBER_CONTROL_PLANE_INTERNAL_URL=" + cp.URL,
		// Don't run tmux launch — exit after the report POST. The
		// SKIP_CLAUDE_LAUNCH env var is the existing early-exit, but it
		// exits BEFORE the model+probe block. We need to exit AFTER. Use
		// KYBER_SKIP_LAUNCH_AFTER_REPORT=1, added in PR-E for this purpose.
		"KYBER_SKIP_LAUNCH_AFTER_REPORT=1",
	}
	env = append(env, extraEnv...)
	out, err := runScript(t, env)
	if err != nil {
		t.Fatalf("script failed: %v\n%s", err, out)
	}

	var got []byte
	select {
	case got = <-receivedBody:
	default:
		t.Fatal("no report POST received by stub CP")
	}
	return string(out), got
}

func TestStartClaude_PRE_Probe_Succeeds_ReportsModelSupportedTrue(t *testing.T) {
	// claude stub: print non-empty for --version; exit 0 for --print probe.
	claude := `if [[ "$*" == "--version" ]]; then echo "2.1.119 (Claude Code)"; exit 0; fi
if [[ "$*" == *"--print"* ]]; then exit 0; fi
exit 0`
	out, body := probeRun(t, claude, "CLAUDE_MODEL=claude-sonnet-4-5")
	if !strings.Contains(out, "pre-flight model probe: ok") {
		t.Errorf("expected probe-ok log; got: %s", out)
	}
	if !strings.Contains(string(body), `"modelSupported":true`) {
		t.Errorf("report body missing modelSupported:true; got: %s", body)
	}
}

func TestStartClaude_PRE_Probe_ModelRejected_ReportsModelSupportedFalse(t *testing.T) {
	// claude stub: --version ok; --print exits non-zero with model-rejection stderr.
	claude := `if [[ "$*" == "--version" ]]; then echo "2.1.119 (Claude Code)"; exit 0; fi
if [[ "$*" == *"--print"* ]]; then echo "error: unsupported model claude-fictional-9" >&2; exit 1; fi
exit 1`
	out, body := probeRun(t, claude, "CLAUDE_MODEL=claude-fictional-9")
	if !strings.Contains(out, "model rejected") {
		t.Errorf("expected model-rejected log; got: %s", out)
	}
	if !strings.Contains(string(body), `"modelSupported":false`) {
		t.Errorf("report body missing modelSupported:false; got: %s", body)
	}
}

func TestStartClaude_PRE_Probe_Timeout_ReportsModelSupportedAbsent(t *testing.T) {
	// claude stub: --version ok; --print sleeps past the timeout. A network
	// blip → unknown, NOT false. We must NOT flip the badge on a transient
	// timeout — the field is OMITTED from the body (== nil server-side).
	claude := `if [[ "$*" == "--version" ]]; then echo "2.1.119 (Claude Code)"; exit 0; fi
if [[ "$*" == *"--print"* ]]; then sleep 5; exit 0; fi
exit 0`
	out, body := probeRun(t, claude, "CLAUDE_MODEL=claude-sonnet-4-5", "KYBER_PROBE_TIMEOUT_SECONDS=1")
	if !strings.Contains(out, "timed out") {
		t.Errorf("expected timeout log; got: %s", out)
	}
	if strings.Contains(string(body), `"modelSupported"`) {
		t.Errorf("modelSupported must be ABSENT on timeout (not false); got: %s", body)
	}
}

func TestStartClaude_PRE_Probe_NonModelError_ReportsModelSupportedAbsent(t *testing.T) {
	// claude stub: --version ok; --print fails with a non-model-rejection
	// error (network/auth/etc). Field must be absent — not false.
	claude := `if [[ "$*" == "--version" ]]; then echo "2.1.119 (Claude Code)"; exit 0; fi
if [[ "$*" == *"--print"* ]]; then echo "network error: connection refused" >&2; exit 1; fi
exit 1`
	_, body := probeRun(t, claude, "CLAUDE_MODEL=claude-sonnet-4-5")
	if strings.Contains(string(body), `"modelSupported"`) {
		t.Errorf("modelSupported must be ABSENT on non-model error; got: %s", body)
	}
}

func TestStartClaude_PRE_ReportBody_IncludesRequestedVersionWhenSet(t *testing.T) {
	claude := `if [[ "$*" == "--version" ]]; then echo "2.0.99 (Claude Code)"; exit 0; fi; exit 0`
	_, body := probeRun(t, claude,
		"CLAUDE_MODEL=claude-sonnet-4-5",
		"KYBER_REQUESTED_CC_VERSION=2.1.119",
	)
	if !strings.Contains(string(body), `"requestedVersion":"2.1.119"`) {
		t.Errorf("report body missing requestedVersion; got: %s", body)
	}
}

func TestStartClaude_PRE_ReportBody_RequestedSatisfiedDerivedFromInstallOutcome(t *testing.T) {
	cases := []struct {
		name             string
		requested        string
		bakedIn          string
		wantSatisfiedKey string // "true", "false", or "" (absent)
	}{
		{"not-requested → absent", "", "2.1.119", ""},
		{"matches-baked-in → true", "2.1.119", "2.1.119", `"requestedSatisfied":true`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			claude := `if [[ "$*" == "--version" ]]; then echo "2.1.119 (Claude Code)"; exit 0; fi; exit 0`
			_, body := probeRun(t, claude,
				"CLAUDE_MODEL=claude-sonnet-4-5",
				"KYBER_REQUESTED_CC_VERSION="+tc.requested,
				"KYBER_RUNTIME_DEFAULT_VERSION="+tc.bakedIn,
			)
			if tc.wantSatisfiedKey == "" {
				if strings.Contains(string(body), `"requestedSatisfied"`) {
					t.Errorf("requestedSatisfied must be absent for not-requested; got: %s", body)
				}
			} else {
				if !strings.Contains(string(body), tc.wantSatisfiedKey) {
					t.Errorf("body missing %q; got: %s", tc.wantSatisfiedKey, body)
				}
			}
		})
	}
}

func TestStartClaude_PRE_Probe_DoesNotCrashLoopOnFailure(t *testing.T) {
	// Probe failure must NOT exit the script non-zero. The pod boots,
	// the failure is surfaced via the report.
	claude := `if [[ "$*" == "--version" ]]; then echo "2.1.119 (Claude Code)"; exit 0; fi
if [[ "$*" == *"--print"* ]]; then exit 99; fi
exit 0`
	out, _ := probeRun(t, claude, "CLAUDE_MODEL=claude-sonnet-4-5")
	if !strings.Contains(out, "[kyber] runtime version reported to control plane") {
		t.Errorf("script should reach the report POST after probe failure; got: %s", out)
	}
}

// --- kyber#678 — the probe must not spawn channel-plugin MCP servers ----

// TestStartClaude_Probe_RunsWithStrictMCPConfig pins the flag that keeps the
// pre-flight probe from loading MCP config. Channel plugins are MCP stdio
// servers, so a probe without this flag spawns a SECOND Telegram bot, then
// exits without reaping it — the orphan holds bot.pid and races the real
// session's poller, leaving the agent randomly deaf on Telegram (kyber#678).
//
// The stub fails the probe when the flag is absent, so a regression surfaces
// as a rejected model rather than a passing test.
func TestStartClaude_Probe_RunsWithStrictMCPConfig(t *testing.T) {
	claude := `if [[ "$*" == "--version" ]]; then echo "2.1.119 (Claude Code)"; exit 0; fi
if [[ "$*" == *"--print"* ]]; then
  if [[ "$*" != *"--strict-mcp-config"* ]]; then
    echo "error: probe invoked without --strict-mcp-config (kyber#678)" >&2
    exit 1
  fi
  exit 0
fi
exit 0`
	out, body := probeRun(t, claude, "CLAUDE_MODEL=claude-sonnet-4-5")
	if !strings.Contains(out, "pre-flight model probe: ok") {
		t.Errorf("probe should run with --strict-mcp-config; got: %s", out)
	}
	if !strings.Contains(string(body), `"modelSupported":true`) {
		t.Errorf("report body missing modelSupported:true; got: %s", body)
	}
	// The logged command must match what actually runs — operators diagnose
	// from this line.
	if !strings.Contains(out, "--strict-mcp-config --print 'ping'") {
		t.Errorf("probe log line should show the flag; got: %s", out)
	}
}

// TestStartClaude_BootSweepsStalePollerBeforeLaunch pins the ORDER of the
// leftover-poller sweep. The equivalent clear at the top of the script runs
// ~1000 lines earlier, so it can only ever catch a previous pod's pid — never
// one created later in the same boot. This is a source-order assertion
// deliberately: the defect being guarded is positional, and the boot path
// between the report POST and the tmux launch has no observable hook to
// exercise behaviourally without stubbing tmux's relaunch loop.
func TestStartClaude_BootSweepsStalePollerBeforeLaunch(t *testing.T) {
	src, err := os.ReadFile(scriptPath(t))
	if err != nil {
		t.Fatalf("read start-claude.sh: %v", err)
	}
	s := string(src)

	launch := strings.Index(s, `tmux new-session -d -s agent -c "$LAUNCH_DIR" "$BOOT_LAUNCH_CMD"`+"\n\necho \"[kyber] tmux session 'agent' started")
	if launch < 0 {
		t.Fatal("could not locate the boot tmux launch")
	}
	sweep := strings.LastIndex(s[:launch], `pkill -f "bun.*server\.ts"`)
	if sweep < 0 {
		t.Fatal("no bun-poller sweep before the boot tmux launch (kyber#678)")
	}
	clear := strings.LastIndex(s[:launch], `rm -f "${HOME:-/home/kyber}/.claude/channels/telegram/bot.pid"`)
	if clear < 0 || clear < sweep {
		t.Fatal("bot.pid must be cleared after the sweep and before the boot launch (kyber#678)")
	}
}

// TestStartClaude_PRC_OneM_BoundaryBelowAndAbove pins the >= 1M
// comparison. At exactly 999_999 → no [1m]; at exactly 1_000_000 →
// [1m]. Future contributors who change the operator (>=, >, ==) would
// trip this.
func TestStartClaude_PRC_OneM_BoundaryBelowAndAbove(t *testing.T) {
	cases := []struct {
		window string
		want1m bool
	}{
		{"999999", false},
		{"1000000", true},
		{"2000000", true},
	}
	for _, tc := range cases {
		t.Run("window="+tc.window, func(t *testing.T) {
			out := bootPrepRun(t,
				"CLAUDE_MODEL=claude-opus-4-7",
				"KYBER_MODEL_CONTEXT_WINDOW="+tc.window,
			)
			has1m := strings.Contains(out, "claude-opus-4-7[1m]")
			if has1m != tc.want1m {
				t.Errorf("window=%s: got [1m]=%v, want %v\n%s", tc.window, has1m, tc.want1m, out)
			}
		})
	}
}

// --- kyber#548 — boot-sync substitutions must not kill boot on edge states ---
//
// The #546 sync block added two unguarded command substitutions; under the
// script's `set -euo pipefail`, a failing substitution in a plain assignment
// exits the shell. Two real states trip it: origin/HEAD unset on the clone
// (symbolic-ref fails → the pipeline fails despite sed's 0), and a clone of
// an EMPTY identity repo on its second boot (no origin/HEAD; rev-parse on the
// unborn HEAD also fails). Both previously fell into guarded paths and booted;
// after #546 they crashloop the pod. These tests pin the guarded behavior:
// boot completes and the sync degrades to WARNINGs, alongside the existing
// normal-clone fixture (TestStartClaude_IdentityRepo_SyncsDefaultBranchNotParkedBranch).

// bootSyncEdgeEnv builds the boot env for the kyber#548 edge-state tests.
func bootSyncEdgeEnv(tmpHome, repoName, anthURL, cpURL, refreshToken string) []string {
	return []string{
		"HOME=" + tmpHome,
		"PATH=" + testPATH(),
		"CLAUDE_REFRESH_TOKEN=" + refreshToken,
		"AGENT_NAME=unit-test",
		"ANTHROPIC_TOKEN_URL=" + anthURL + "/v1/oauth/token",
		"KYBER_REFRESH_TOKEN_URL=" + cpURL + "/internal/agents/unit-test/refresh-token",
		"KYBER_IDENTITY_REPO=matty-v/" + repoName,
		"GH_TOKEN=ghp_fake_pat_for_helper_install",
		"SKIP_CLAUDE_LAUNCH=1",
	}
}

func TestStartClaude_IdentityRepo_OriginHeadUnset_BootSurvives(t *testing.T) {
	mock := mockserver.New()
	ts := httptest.NewServer(mock)
	defer ts.Close()
	cpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(204)
	}))
	defer cpServer.Close()
	refreshToken := seedRefreshToken(t, mock, ts.URL)

	tmpHome := t.TempDir()
	gitEnv := []string{
		"HOME=" + tmpHome, "PATH=" + testPATH(),
		"GIT_CONFIG_GLOBAL=" + filepath.Join(tmpHome, ".gitconfig-setup"),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
	}
	git := func(dir string, args ...string) {
		t.Helper()
		c := exec.Command("git", args...)
		c.Dir = dir
		c.Env = gitEnv
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("setup git %v in %q: %v\n%s", args, dir, err, out)
		}
	}

	// Normal remote with one commit on main; clone it, then DELETE origin/HEAD —
	// the state symbolic-ref fails on (older tooling / manual surgery leaves
	// clones like this; the ${DEFAULT_BRANCH:-main} fallback was written for it).
	work := t.TempDir()
	remote := filepath.Join(work, "remote.git")
	git(work, "init", "--bare", "-b", "main", remote)
	seed := filepath.Join(work, "seed")
	git(work, "clone", remote, seed)
	if err := os.WriteFile(filepath.Join(seed, "marker"), []byte("A"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(seed, "add", "-A")
	git(seed, "commit", "-m", "A")
	git(seed, "push", "origin", "main")

	repoName := "test-ident-" + fmt.Sprint(time.Now().UnixNano())
	repoDir := filepath.Join(tmpHome, "dev", repoName)
	if err := os.MkdirAll(filepath.Dir(repoDir), 0o755); err != nil {
		t.Fatal(err)
	}
	git(work, "clone", remote, repoDir)
	git(repoDir, "symbolic-ref", "--delete", "refs/remotes/origin/HEAD")

	// Seed a skill in the identity-repo convention layout — skills/<name>/SKILL.md
	// (a subdir, not a flat file). The linker must pick this up; a regression to a
	// flat `skills/*.md` glob would silently link nothing.
	skillDir := filepath.Join(repoDir, "skills", "demo")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\nname: demo\n---\ndemo\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := runScript(t, bootSyncEdgeEnv(tmpHome, repoName, ts.URL, cpServer.URL, refreshToken))
	if err != nil {
		t.Fatalf("boot must survive an unset origin/HEAD (kyber#548): %v\noutput:\n%s", err, out)
	}
	s := string(out)
	// The main fallback worked: the DEFAULT_BRANCH:-main checkout did not error.
	if strings.Contains(s, "checkout main failed") {
		t.Errorf("expected the sync to fall back to 'main' cleanly, got:\n%s", s)
	}
	// Boot ran the sync to completion — the skills-relink is its final step, so
	// this line is reached only if nothing aborted the sync block.
	if !strings.Contains(s, "skills re-linked") {
		t.Errorf("boot did not complete the identity-repo sync, got:\n%s", s)
	}
	// The subdir-layout skill was actually linked into ~/.claude/skills/<name>/SKILL.md
	// (os.Stat follows the directory symlink to the real file).
	linked := filepath.Join(tmpHome, ".claude", "skills", "demo", "SKILL.md")
	if _, err := os.Stat(linked); err != nil {
		t.Errorf("subdir skill 'demo' was not linked into ~/.claude/skills: %v\noutput:\n%s", err, s)
	}
	codexLinked := filepath.Join(tmpHome, ".codex", "skills", "demo", "SKILL.md")
	if _, err := os.Stat(codexLinked); err != nil {
		t.Errorf("subdir skill 'demo' was not linked into ~/.codex/skills: %v\noutput:\n%s", err, s)
	}
}

func TestStartClaude_IdentityRepo_EmptyRemoteSecondBoot_BootSurvives(t *testing.T) {
	mock := mockserver.New()
	ts := httptest.NewServer(mock)
	defer ts.Close()
	cpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(204)
	}))
	defer cpServer.Close()
	refreshToken := seedRefreshToken(t, mock, ts.URL)

	tmpHome := t.TempDir()
	gitEnv := []string{
		"HOME=" + tmpHome, "PATH=" + testPATH(),
		"GIT_CONFIG_GLOBAL=" + filepath.Join(tmpHome, ".gitconfig-setup"),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
	}
	git := func(dir string, args ...string) {
		t.Helper()
		c := exec.Command("git", args...)
		c.Dir = dir
		c.Env = gitEnv
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("setup git %v in %q: %v\n%s", args, dir, err, out)
		}
	}

	// EMPTY remote (no commits — a brand-new agent whose identity repo hasn't
	// been populated yet). The pre-existing clone is what the agent's FIRST
	// boot left on disk; this run is the second boot: [ -d .git ] → sync path,
	// where no origin/HEAD exists and HEAD is unborn (both substitutions fail).
	work := t.TempDir()
	remote := filepath.Join(work, "remote.git")
	git(work, "init", "--bare", "-b", "main", remote)

	repoName := "test-ident-" + fmt.Sprint(time.Now().UnixNano())
	repoDir := filepath.Join(tmpHome, "dev", repoName)
	if err := os.MkdirAll(filepath.Dir(repoDir), 0o755); err != nil {
		t.Fatal(err)
	}
	git(work, "clone", remote, repoDir)

	out, err := runScript(t, bootSyncEdgeEnv(tmpHome, repoName, ts.URL, cpServer.URL, refreshToken))
	if err != nil {
		t.Fatalf("boot must survive an empty identity repo on second boot (kyber#548): %v\noutput:\n%s", err, out)
	}
	// The sync degrades to WARNINGs (checkout/pull fail on an unborn HEAD) and
	// boot reaches the skills-relink step (final sync step) instead of crashlooping.
	if !strings.Contains(string(out), "skills re-linked") {
		t.Errorf("boot did not reach the skills-relink step, got:\n%s", out)
	}
}

// bootWithSessionState boots start-claude.sh through a successful token refresh
// (so it reaches the post-auth session-recall block) with KYBER_SESSION_STATE_FILE
// pointed at stateJSON (empty string = don't set it). Returns the temp HOME (=
// LAUNCH_DIR, since no identity repo) and the combined script output.
func bootWithSessionState(t *testing.T, stateJSON string) (home string, out []byte) {
	t.Helper()
	mock := mockserver.New()
	ts := httptest.NewServer(mock)
	t.Cleanup(ts.Close)
	cpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(204)
	}))
	t.Cleanup(cpServer.Close)

	refreshToken := seedRefreshToken(t, mock, ts.URL)
	tmpHome := t.TempDir()

	env := []string{
		"HOME=" + tmpHome,
		"PATH=" + testPATH(),
		"CLAUDE_REFRESH_TOKEN=" + refreshToken,
		"AGENT_NAME=unit-test",
		"ANTHROPIC_TOKEN_URL=" + ts.URL + "/v1/oauth/token",
		"KYBER_REFRESH_TOKEN_URL=" + cpServer.URL + "/internal/agents/unit-test/refresh-token",
		"SKIP_CLAUDE_LAUNCH=1",
	}
	// Always pin KYBER_SESSION_STATE_FILE, including for the no-state case: left
	// unset, STATE_FILE falls back to the real /persist/session-state.json, so the
	// absent-state tests pick up the live state of whatever agent pod is running
	// the suite and assert against its session instead of an empty one.
	statePath := filepath.Join(tmpHome, "session-state.json")
	if stateJSON != "" {
		if err := os.WriteFile(statePath, []byte(stateJSON), 0o644); err != nil {
			t.Fatalf("write state fixture: %v", err)
		}
	}
	env = append(env, "KYBER_SESSION_STATE_FILE="+statePath)

	out, err := runScript(t, env)
	if err != nil {
		t.Fatalf("script failed: %v\noutput:\n%s", err, out)
	}
	return tmpHome, out
}

// TestStartClaude_SessionRecall_WrittenOnBoot: with a session-state.json present,
// the boot renders .runtime/session-recall.md (the recall the agent's CLAUDE.md
// reads) with the last activity and recent turns, before claude launches.
func TestStartClaude_SessionRecall_WrittenOnBoot(t *testing.T) {
	state := `{"version":1,"agent_name":"unit-test","updated_at":"2026-07-06T18:00:05Z",` +
		`"last_activity":"Doing the thing.","recent_exchanges":[` +
		`{"role":"user","content":"hi there","timestamp":"2026-07-06T18:00:00Z"},` +
		`{"role":"assistant","content":"Doing the thing.","timestamp":"2026-07-06T18:00:05Z"}]}`

	home, out := bootWithSessionState(t, state)

	if !strings.Contains(string(out), "Session recall written") {
		t.Fatalf("boot did not report writing the recall, got:\n%s", out)
	}
	recall, err := os.ReadFile(filepath.Join(home, ".runtime", "session-recall.md"))
	if err != nil {
		t.Fatalf("session-recall.md not written: %v\noutput:\n%s", err, out)
	}
	got := string(recall)
	for _, want := range []string{
		"**Last activity:** Doing the thing.",
		"**As of:** 2026-07-06T18:00:05Z",
		"### User · 2026-07-06T18:00:00Z",
		"hi there",
		"### Assistant · 2026-07-06T18:00:05Z",
		"Doing the thing.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("recall missing %q; full recall:\n%s", want, got)
		}
	}
}

// TestStartClaude_SessionRecall_AbsentIsNoOp: first boot (no state file) writes no
// recall file and does not error.
func TestStartClaude_SessionRecall_AbsentIsNoOp(t *testing.T) {
	home, out := bootWithSessionState(t, "")
	if _, err := os.Stat(filepath.Join(home, ".runtime", "session-recall.md")); !os.IsNotExist(err) {
		t.Errorf("recall file should not exist on first boot (no state); stat err = %v", err)
	}
	if strings.Contains(string(out), "Session recall written") {
		t.Errorf("boot reported writing a recall with no state file present:\n%s", out)
	}
}

// TestStartClaude_SessionRecall_EmptyStateIsNoOp: a state file with no activity and
// no exchanges (e.g. a brand-new session that only ran tools) writes no recall.
func TestStartClaude_SessionRecall_EmptyStateIsNoOp(t *testing.T) {
	home, out := bootWithSessionState(t, `{"version":1,"agent_name":"unit-test","last_activity":"","recent_exchanges":[]}`)
	if _, err := os.Stat(filepath.Join(home, ".runtime", "session-recall.md")); !os.IsNotExist(err) {
		t.Errorf("recall file should not exist for empty state; stat err = %v", err)
	}
	if strings.Contains(string(out), "Session recall written") {
		t.Errorf("boot reported writing a recall for empty state:\n%s", out)
	}
}

// TestStartClaude_SessionRecall_ToleratesMissingExchanges locks the boot-render
// hardening: a state file with last_activity but no recent_exchanges (e.g. shape
// drift across a rolling upgrade) still renders a valid recall — the jq render is
// null/type-safe and must not abort into a 0-byte clobber.
func TestStartClaude_SessionRecall_ToleratesMissingExchanges(t *testing.T) {
	state := `{"version":1,"agent_name":"unit-test","updated_at":"2026-07-06T18:00:05Z","last_activity":"Did stuff"}`
	home, out := bootWithSessionState(t, state)

	if !strings.Contains(string(out), "Session recall written") {
		t.Fatalf("boot did not write recall for a no-exchanges state, got:\n%s", out)
	}
	recall, err := os.ReadFile(filepath.Join(home, ".runtime", "session-recall.md"))
	if err != nil {
		t.Fatalf("session-recall.md not written: %v\noutput:\n%s", err, out)
	}
	got := string(recall)
	if len(strings.TrimSpace(got)) == 0 {
		t.Fatalf("recall rendered empty (jq aborted into a clobber); output:\n%s", out)
	}
	if !strings.Contains(got, "**Last activity:** Did stuff") {
		t.Errorf("recall missing last activity; full recall:\n%s", got)
	}
	if !strings.Contains(got, "## Recent turns") {
		t.Errorf("recall missing the turns header; full recall:\n%s", got)
	}
}

// bootWithAgentManual boots start-claude.sh through a successful token refresh (so
// it reaches the post-auth agent-manual block) with KYBER_AGENT_MANUAL_PATH pointed
// at a fixture holding manualBody. An empty manualBody leaves the env var pointed at
// a path that does not exist, exercising the missing-source path. Returns the temp
// HOME (= LAUNCH_DIR, since no identity repo) and the combined script output.
func bootWithAgentManual(t *testing.T, manualBody string) (home string, out []byte) {
	t.Helper()
	return bootWithAgentManualInHome(t, manualBody, t.TempDir())
}

// bootWithAgentManualInHome is bootWithAgentManual against a caller-supplied HOME,
// so a test can boot twice into the same launch dir and assert what the second boot
// does to what the first one left behind.
func bootWithAgentManualInHome(t *testing.T, manualBody, tmpHome string) (home string, out []byte) {
	t.Helper()
	manualPath := filepath.Join(t.TempDir(), "KYBER.md")
	if manualBody != "" {
		if err := os.WriteFile(manualPath, []byte(manualBody), 0o644); err != nil {
			t.Fatalf("write manual fixture: %v", err)
		}
	}
	return bootWithAgentManualPath(t, manualPath, tmpHome)
}

// bootWithAgentManualPath is the same boot with KYBER_AGENT_MANUAL_PATH pointed at
// a caller-built path, so a test can control the source file's *surroundings* (its
// parent directory's mode) and not just its contents.
func bootWithAgentManualPath(t *testing.T, manualPath, tmpHome string) (home string, out []byte) {
	t.Helper()
	mock := mockserver.New()
	ts := httptest.NewServer(mock)
	t.Cleanup(ts.Close)
	cpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(204)
	}))
	t.Cleanup(cpServer.Close)

	refreshToken := seedRefreshToken(t, mock, ts.URL)

	env := []string{
		"HOME=" + tmpHome,
		"PATH=" + testPATH(),
		"CLAUDE_REFRESH_TOKEN=" + refreshToken,
		"AGENT_NAME=unit-test",
		"ANTHROPIC_TOKEN_URL=" + ts.URL + "/v1/oauth/token",
		"KYBER_REFRESH_TOKEN_URL=" + cpServer.URL + "/internal/agents/unit-test/refresh-token",
		"KYBER_AGENT_MANUAL_PATH=" + manualPath,
		"SKIP_CLAUDE_LAUNCH=1",
	}

	out, err := runScript(t, env)
	if err != nil {
		t.Fatalf("script failed: %v\noutput:\n%s", err, out)
	}
	return tmpHome, out
}

// TestStartClaude_AgentManual_WrittenOnBoot: the manual baked into the image is
// rendered verbatim into .runtime/KYBER.md before claude launches, so the agent's
// CLAUDE.md walk-up reads a manual matching the running platform version.
func TestStartClaude_AgentManual_WrittenOnBoot(t *testing.T) {
	body := "# Kyber — the platform you run on\n\nOnly git is durable.\n"
	home, out := bootWithAgentManual(t, body)

	if !strings.Contains(string(out), "Agent manual written") {
		t.Fatalf("boot did not report writing the manual, got:\n%s", out)
	}
	got, err := os.ReadFile(filepath.Join(home, ".runtime", "KYBER.md"))
	if err != nil {
		t.Fatalf(".runtime/KYBER.md not written: %v\noutput:\n%s", err, out)
	}
	if string(got) != body {
		t.Errorf("manual not copied verbatim:\ngot:\n%s\nwant:\n%s", got, body)
	}
}

// TestStartClaude_AgentManual_AbsentIsNoOp: an image without the baked manual (an
// older base, or a runtime that doesn't ship one) must boot cleanly, write nothing,
// and not report a write.
func TestStartClaude_AgentManual_AbsentIsNoOp(t *testing.T) {
	home, out := bootWithAgentManual(t, "")
	if _, err := os.Stat(filepath.Join(home, ".runtime", "KYBER.md")); !os.IsNotExist(err) {
		t.Errorf("manual should not exist when the source is absent; stat err = %v", err)
	}
	if strings.Contains(string(out), "Agent manual written") {
		t.Errorf("boot reported writing a manual with no source present:\n%s", out)
	}
	// Absent is fine, but never silent — see the unreadable-source test below for
	// why "said nothing" is the failure mode that actually shipped.
	if !strings.Contains(string(out), "no agent manual baked at") {
		t.Errorf("boot skipped the manual without saying so:\n%s", out)
	}
}

// TestStartClaude_AgentManual_UnreadableSourceWarns pins the OBSERVABILITY half of
// the kyber#653 regression. `[ -s "$MANUAL_SRC" ]` is false both when the manual is
// absent and when it is present but unreachable, and the first cut of the block
// treated both as a silent no-op. The real failure was the second case — the image
// shipped /opt/kyber at mode 0644, so the unprivileged `kyber` user the boot script
// runs as could not traverse into it — and it produced not one line in any boot log
// on any pod. Reproduced here by stripping the execute bit off the parent directory,
// which is exactly what `COPY --chmod=0644` did.
//
// Skipped as root: root bypasses directory permission checks entirely, which is the
// same reason every existing test passed while the fleet was broken. The uid-proof
// half of this regression lives in TestClaudeCodeDockerfile_ManualDirIsTraversable.
func TestStartClaude_AgentManual_UnreadableSourceWarns(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: directory permission checks are bypassed, so the failure cannot be reproduced")
	}
	optKyber := filepath.Join(t.TempDir(), "kyber")
	if err := os.Mkdir(optKyber, 0o755); err != nil {
		t.Fatal(err)
	}
	manualPath := filepath.Join(optKyber, "KYBER.md")
	if err := os.WriteFile(manualPath, []byte("# manual\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(optKyber, 0o644); err != nil { // no +x → not traversable
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(optKyber, 0o755) }) // so TempDir cleanup can recurse

	home, out := bootWithAgentManualPath(t, manualPath, t.TempDir())

	if _, err := os.Stat(filepath.Join(home, ".runtime", "KYBER.md")); !os.IsNotExist(err) {
		t.Errorf("manual should not have been written from an unreachable source; stat err = %v", err)
	}
	if !strings.Contains(string(out), "not traversable") {
		t.Errorf("boot did not warn that the manual source was unreachable — this is the exact silence that let kyber#653 ship:\n%s", out)
	}
}

// TestClaudeCodeDockerfile_ManualDirIsTraversable is the uid-independent pin on the
// kyber#653 packaging defect: `COPY --chmod=0644 docs/agent-manual.md
// /opt/kyber/KYBER.md` applies 0644 to the /opt/kyber directory it implicitly
// creates too, leaving it without an execute bit and therefore invisible to every
// non-root process in the pod. No runtime test catches this — the boot suite runs
// against a fixture path, and the container tests run as root — so the assertion has
// to be made against the Dockerfile itself.
func TestClaudeCodeDockerfile_ManualDirIsTraversable(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(cwd, "Dockerfile"))
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	src := string(raw)

	copyIdx := strings.Index(src, "/opt/kyber/KYBER.md")
	if copyIdx < 0 {
		t.Fatal("the Dockerfile no longer bakes the agent manual to /opt/kyber/KYBER.md — " +
			"if it moved, move this guard with it")
	}

	// The directory must be created with an explicit traversable mode BEFORE the
	// COPY; a mode set afterwards would still leave the implicit-creation bug in
	// place for any other COPY landing there first.
	mkdir := regexp.MustCompile(`(?m)^RUN\s+install\s+-d\s+-m\s+([0-7]{3,4})\s+/opt/kyber\s*$`)
	m := mkdir.FindStringSubmatch(src[:copyIdx])
	if m == nil {
		t.Fatal("/opt/kyber is not created before the manual COPY. `COPY --chmod=0644` " +
			"applies 0644 to the directories it implicitly creates, which makes the manual " +
			"unreadable to the unprivileged `kyber` user and silently breaks the render on " +
			"every pod (kyber#653). Add `RUN install -d -m 0755 /opt/kyber` before the COPY.")
	}
	mode, err := strconv.ParseUint(m[1], 8, 32)
	if err != nil {
		t.Fatalf("unparseable mode %q: %v", m[1], err)
	}
	if mode&0o111 != 0o111 {
		t.Errorf("/opt/kyber is created with mode %s — every class needs the execute bit to "+
			"traverse into it; the pod's boot script runs as `kyber`, not root", m[1])
	}
}

// TestStartClaude_AgentManual_RendersIntoIdentityRepo: the production path. Real
// agents have an identity repo, and their CLAUDE.md walk-up resolves from the repo
// clone — not $HOME. If the manual renders to $HOME instead of <repo>/.runtime/ the
// agent never reads it, so pin the base-dir resolution against a real clone.
func TestStartClaude_AgentManual_RendersIntoIdentityRepo(t *testing.T) {
	mock := mockserver.New()
	ts := httptest.NewServer(mock)
	t.Cleanup(ts.Close)
	cpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(204)
	}))
	t.Cleanup(cpServer.Close)

	refreshToken := seedRefreshToken(t, mock, ts.URL)
	tmpHome := t.TempDir()

	git := func(dir string, args ...string) {
		t.Helper()
		c := exec.Command("git", args...)
		c.Dir = dir
		c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("setup git %v in %q: %v\n%s", args, dir, err, out)
		}
	}

	// Bare "remote" with one commit on main, pre-cloned to REPO_DIR = $HOME/dev/<name>.
	work := t.TempDir()
	remote := filepath.Join(work, "remote.git")
	git(work, "init", "--bare", "-b", "main", remote)
	seed := filepath.Join(work, "seed")
	git(work, "clone", remote, seed)
	if err := os.WriteFile(filepath.Join(seed, "marker"), []byte("A"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(seed, "add", "-A")
	git(seed, "commit", "-m", "A")
	git(seed, "push", "origin", "main")

	repoName := "test-manual-" + fmt.Sprint(time.Now().UnixNano())
	repoDir := filepath.Join(tmpHome, "dev", repoName)
	if err := os.MkdirAll(filepath.Dir(repoDir), 0o755); err != nil {
		t.Fatal(err)
	}
	git(work, "clone", remote, repoDir)

	body := "# Kyber — the platform you run on\n\nOnly git is durable.\n"
	manualPath := filepath.Join(t.TempDir(), "KYBER.md")
	if err := os.WriteFile(manualPath, []byte(body), 0o644); err != nil {
		t.Fatalf("write manual fixture: %v", err)
	}

	out, err := runScript(t, []string{
		"HOME=" + tmpHome,
		"PATH=" + testPATH(),
		"CLAUDE_REFRESH_TOKEN=" + refreshToken,
		"AGENT_NAME=unit-test",
		"ANTHROPIC_TOKEN_URL=" + ts.URL + "/v1/oauth/token",
		"KYBER_REFRESH_TOKEN_URL=" + cpServer.URL + "/internal/agents/unit-test/refresh-token",
		"KYBER_IDENTITY_REPO=matty-v/" + repoName,
		"KYBER_AGENT_MANUAL_PATH=" + manualPath,
		"GH_TOKEN=ghp_fake_pat_for_helper_install",
		"SKIP_CLAUDE_LAUNCH=1",
	})
	if err != nil {
		t.Fatalf("script failed: %v\noutput:\n%s", err, out)
	}

	got, err := os.ReadFile(filepath.Join(repoDir, ".runtime", "KYBER.md"))
	if err != nil {
		t.Fatalf("manual not rendered into the identity repo: %v\noutput:\n%s", err, out)
	}
	if string(got) != body {
		t.Errorf("manual not copied verbatim into the repo:\ngot:\n%s\nwant:\n%s", got, body)
	}
	if _, err := os.Stat(filepath.Join(tmpHome, ".runtime", "KYBER.md")); !os.IsNotExist(err) {
		t.Errorf("manual also written to $HOME/.runtime — base dir must resolve to the repo; stat err = %v", err)
	}

	// The manual is platform-owned and identical to the image's copy, so it must
	// not end up in the agent's git history — an agent running a `git add -A` save
	// would otherwise commit it, and every image bump would churn every identity
	// repo. .git/info/exclude covers existing repos with no .gitignore entry.
	excl, err := os.ReadFile(filepath.Join(repoDir, ".git", "info", "exclude"))
	if err != nil {
		t.Fatalf("read .git/info/exclude: %v", err)
	}
	if !strings.Contains(string(excl), ".runtime/KYBER.md") {
		t.Errorf(".runtime/KYBER.md not excluded from git; a `git add -A` save would commit it:\n%s", excl)
	}
	// git must actually agree — the line being present is not the same as it working.
	st := exec.Command("git", "status", "--porcelain", "--untracked-files=all")
	st.Dir = repoDir
	st.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+filepath.Join(tmpHome, ".gitconfig-none"))
	stOut, err := st.CombinedOutput()
	if err != nil {
		t.Fatalf("git status: %v\n%s", err, stOut)
	}
	if strings.Contains(string(stOut), ".runtime/KYBER.md") {
		t.Errorf("git still reports .runtime/KYBER.md as untracked:\n%s", stOut)
	}
}

// TestStartClaude_AgentManual_ExcludeNotDuplicated: the exclude entry is appended
// once, not on every boot — otherwise .git/info/exclude grows a line per restart
// for the life of the agent.
func TestStartClaude_AgentManual_ExcludeNotDuplicated(t *testing.T) {
	body := "# Kyber — the platform you run on\n"
	home := t.TempDir()

	// No identity repo in this harness, so the launch dir IS $HOME — make it a git
	// repo so the exclude branch runs at all.
	init := exec.Command("git", "init", "-q", "-b", "main", home)
	init.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+filepath.Join(t.TempDir(), "gitconfig"))
	if out, err := init.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}

	for i := 0; i < 3; i++ {
		bootWithAgentManualInHome(t, body, home)
	}

	excl, err := os.ReadFile(filepath.Join(home, ".git", "info", "exclude"))
	if err != nil {
		t.Fatalf("read .git/info/exclude: %v", err)
	}
	if n := strings.Count(string(excl), ".runtime/KYBER.md"); n != 1 {
		t.Errorf("exclude entry written %d times across 3 boots, want 1:\n%s", n, excl)
	}
}

// TestStartClaude_AgentManual_OverwrittenEachBoot: the manual is platform-owned, so
// a stale copy left in .runtime/ from a previous image version must be replaced —
// not merged, not preserved. This is the whole point of rendering it at boot.
func TestStartClaude_AgentManual_OverwrittenEachBoot(t *testing.T) {
	body := "# Kyber — the platform you run on\n\nCurrent text.\n"
	home := t.TempDir()
	bootWithAgentManualInHome(t, body, home)

	stale := filepath.Join(home, ".runtime", "KYBER.md")
	if err := os.WriteFile(stale, []byte("stale text from an older image\n"), 0o644); err != nil {
		t.Fatalf("seed stale manual: %v", err)
	}
	// Second boot into the SAME launch dir — the stale copy must be replaced.
	_, out := bootWithAgentManualInHome(t, body, home)
	got, err := os.ReadFile(stale)
	if err != nil {
		t.Fatalf(".runtime/KYBER.md not written on re-boot: %v\noutput:\n%s", err, out)
	}
	if string(got) != body {
		t.Errorf("re-boot did not render the current manual:\ngot:\n%s\nwant:\n%s", got, body)
	}
}

// TestStartClaude_LaunchHeredoc_NoUnescapedLocals guards a generation-time hazard
// that no runtime test in this file can reach: the restart-session block lives
// inside the UNQUOTED `<<LAUNCH_SH` heredoc, so every unescaped $var there is
// expanded at boot and baked into the generated script — not evaluated at restart.
//
// A local variable assigned inside the generated script therefore expands to the
// EMPTY string, silently turning `[ -x "$_sync_script" ]` into `[ -x "" ]` and
// disabling the kyber#596 restart-session identity sync with no error anywhere.
// That regression shipped in this PR's first draft and was caught in review, not
// by tests: the SKIP_CLAUDE_LAUNCH short-circuit returns long before this heredoc
// is ever written, and the generator hardcodes /persist paths that don't exist off
// a pod.
//
// So this asserts on the script SOURCE. It is a narrow guard, deliberately: it
// pins the one shape that broke and explains why the obvious runtime test can't.
func TestStartClaude_LaunchHeredoc_NoUnescapedLocals(t *testing.T) {
	src, err := os.ReadFile(scriptPath(t))
	if err != nil {
		t.Fatalf("read start-claude.sh: %v", err)
	}
	body := string(src)

	start := strings.Index(body, "<<LAUNCH_SH")
	if start < 0 {
		t.Fatal("could not find the <<LAUNCH_SH heredoc — did the generator move?")
	}
	end := strings.Index(body[start:], "\nLAUNCH_SH\n")
	if end < 0 {
		t.Fatal("could not find the LAUNCH_SH terminator")
	}
	heredoc := body[start : start+end]

	if strings.Contains(heredoc, "_sync_script") {
		t.Error("the LAUNCH_SH heredoc references a script-local variable (_sync_script); " +
			"unescaped, it expands at boot to the empty string and silently disables the " +
			"restart-session identity sync. Bake the boot-resolved literal in instead.")
	}
	if !strings.Contains(heredoc, "kyber-sync-identity.sh") {
		t.Error("the restart-session identity sync (kyber#596) disappeared from the generated " +
			"launch script — a restart would no longer pick up new skills/contract")
	}
}

// --- raw model-probe report (canary regression 2026-08-22) ----------------
//
// The CLI prints its model-rejection message to STDOUT, which the old
// probe discarded (it captured stderr only), and the current phrasing
// matched none of the old grep patterns — so an invalid model reported
// "unknown" and the platform showed green. The script now captures BOTH
// streams, recognizes the current phrasing, and always ships the raw
// exit+output for the control plane's authoritative classification
// (pkg/modelprobe).

func TestStartClaude_Probe_StdoutRejectionCurrentPhrasing_ReportsFalseAndRaw(t *testing.T) {
	// Reproduces the live failure byte-for-byte: rejection on STDOUT,
	// deprecation noise on stderr, exit 1.
	claude := `if [[ "$*" == "--version" ]]; then echo "2.1.240 (Claude Code)"; exit 0; fi
if [[ "$*" == *"--print"* ]]; then
  echo "There's an issue with the selected model (claude-opus-4-canary-marker). It may not exist or you may not have access to it."
  echo "warning: something unrelated" >&2
  exit 1
fi
exit 1`
	out, body := probeRun(t, claude, "CLAUDE_MODEL=claude-opus-4-canary-marker")
	if !strings.Contains(out, "model rejected") {
		t.Errorf("expected model-rejected log for the current CLI phrasing; got: %s", out)
	}
	if !strings.Contains(string(body), `"modelSupported":false`) {
		t.Errorf("legacy field: report body missing modelSupported:false; got: %s", body)
	}
	if !strings.Contains(string(body), `"modelProbeExit":1`) {
		t.Errorf("report body missing modelProbeExit:1; got: %s", body)
	}
	if !strings.Contains(string(body), "issue with the selected model") {
		t.Errorf("report body missing the probe output; got: %s", body)
	}
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("report body is not valid JSON: %v\n%s", err, body)
	}
}

func TestStartClaude_Probe_RawFieldsPresentOnTimeout(t *testing.T) {
	// Legacy modelSupported stays absent on timeout, but the raw exit
	// (124) must ship so the control plane can distinguish "timed out"
	// from "never probed".
	claude := `if [[ "$*" == "--version" ]]; then echo "2.1.240 (Claude Code)"; exit 0; fi
if [[ "$*" == *"--print"* ]]; then sleep 5; exit 0; fi
exit 0`
	_, body := probeRun(t, claude, "CLAUDE_MODEL=claude-sonnet-5", "KYBER_PROBE_TIMEOUT_SECONDS=1")
	if strings.Contains(string(body), `"modelSupported"`) {
		t.Errorf("modelSupported must stay ABSENT on timeout; got: %s", body)
	}
	if !strings.Contains(string(body), `"modelProbeExit":124`) {
		t.Errorf("report body missing modelProbeExit:124 on timeout; got: %s", body)
	}
}

func TestStartClaude_Probe_OutputSanitizedForJSON(t *testing.T) {
	// Quotes, backslashes and newlines in probe output must not corrupt
	// the hand-assembled JSON body.
	claude := `if [[ "$*" == "--version" ]]; then echo "2.1.240 (Claude Code)"; exit 0; fi
if [[ "$*" == *"--print"* ]]; then
  printf 'line "one" with \\ slash\nline two\n'
  exit 1
fi
exit 1`
	_, body := probeRun(t, claude, "CLAUDE_MODEL=claude-sonnet-5")
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("report body is not valid JSON after hostile probe output: %v\n%s", err, body)
	}
	if _, ok := parsed["modelProbeOutput"]; !ok {
		t.Errorf("sanitized output missing from body: %s", body)
	}
}

func TestStartClaude_StartupPromptIsQuotedForEveryLaunch(t *testing.T) {
	src, err := os.ReadFile(scriptPath(t))
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	if !strings.Contains(s, `CLAUDE_LAUNCH_ARGS="$CLAUDE_LAUNCH_ARGS -- $(printf '%q' "$KYBER_STARTUP_PROMPT")"`) {
		t.Fatal("startup prompt is not shell-quoted as one positional argument")
	}
	if got := strings.Count(s, `"claude $CLAUDE_LAUNCH_ARGS"`); got != 3 {
		t.Fatalf("startup-aware command used by %d launch paths, want 3", got)
	}
	if strings.Contains(s, `Starting Claude Code in tmux (cwd=$LAUNCH_DIR): claude $CLAUDE_LAUNCH_ARGS`) {
		t.Fatal("startup prompt must not be emitted in boot logs")
	}
}

// TestStartClaudeSessionResumeSourceContract pins the kyber#118 source
// invariants the rendered-script test below cannot see: boot gates resume on
// the enable flag plus a recorded transcript, resume launches use
// CLAUDE_RESUME_ARGS (--continue plus the startup prompt when configured —
// the prompt is what makes a resumed agent act on its restored context
// instead of idling), and the crash watchdog keeps its --fresh fallback for
// poison transcripts.
func TestStartClaudeSessionResumeSourceContract(t *testing.T) {
	src, err := os.ReadFile(scriptPath(t))
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	for what, want := range map[string]string{
		"resume arg set":             `CLAUDE_RESUME_ARGS="$CLAUDE_ARGS --continue"`,
		"resume arg set prompt":      `CLAUDE_RESUME_ARGS="$CLAUDE_RESUME_ARGS -- $(printf '%q' "$KYBER_STARTUP_PROMPT")"`,
		"boot resume selection":      `BOOT_LAUNCH_CMD="claude $CLAUDE_RESUME_ARGS"`,
		"boot gate":                  `if claude_has_prior_session; then`,
		"boot gate enable flag":      `if [ "$SESSION_RESUME_ENABLED" = "1" ]; then`,
		"relaunch resume selection":  `RELAUNCH_CMD="claude $CLAUDE_RESUME_ARGS"`,
		"watchdog --fresh fallback":  `/persist/last-claude-launch.sh $RELAUNCH_FLAG || echo`,
		"restart-session fresh flag": `[ "\${1:-}" = "--fresh" ] && KYBER_FRESH=1`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("%s missing from start-claude.sh (want %q)", what, want)
		}
	}
}

// TestGeneratedClaudeRelaunchScript_SessionResume renders the
// last-claude-launch.sh heredoc exactly as boot would (resume enabled) and
// executes the result in all three modes, asserting against a logging sudo
// stub:
//
//	bare + empty transcript store -> fresh launch (with startup prompt)
//	bare + prior transcript       -> `claude ... --continue` (prompt delivered
//	                                 into the resumed session)
//	--fresh + prior transcript    -> fresh launch (intentional restart wins)
func TestGeneratedClaudeRelaunchScript_SessionResume(t *testing.T) {
	src, err := os.ReadFile(scriptPath(t))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(src), "\n")
	start, end := -1, -1
	for i, l := range lines {
		if strings.HasPrefix(l, `cat > /persist/last-claude-launch.sh <<LAUNCH_SH`) {
			start = i
		} else if start >= 0 && l == "LAUNCH_SH" {
			end = i
			break
		}
	}
	if start < 0 || end < 0 {
		t.Fatalf("could not locate last-claude-launch.sh heredoc (start=%d end=%d)", start, end)
	}

	work := t.TempDir()
	gen := filepath.Join(work, "last-claude-launch.sh")
	block := strings.ReplaceAll(
		strings.Join(lines[start:end+1], "\n"),
		"/persist/last-claude-launch.sh", gen)

	store := filepath.Join(work, "store")
	if err := os.MkdirAll(store, 0o755); err != nil {
		t.Fatal(err)
	}
	lockDir := filepath.Join(work, "lock")
	if err := os.MkdirAll(lockDir, 0o755); err != nil {
		t.Fatal(err)
	}

	bin := filepath.Join(work, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	// The stub answers tmux has-session from a flag file (absent = dead
	// session, the normal watchdog case) and logs everything else.
	sudoLog := filepath.Join(work, "sudo.log")
	aliveFlag := filepath.Join(work, "session-alive")
	if err := os.WriteFile(filepath.Join(bin, "sudo"),
		[]byte("#!/usr/bin/env bash\ncase \"$*\" in *has-session*) [ -f '"+aliveFlag+"' ] && exit 0 || exit 1;; esac\necho \"sudo $*\" >> '"+sudoLog+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "pkill"),
		[]byte("#!/usr/bin/env bash\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	wrapper := strings.Join([]string{
		"set -u",
		"HOME='" + work + "'", // keeps the baked bot.pid rm inside the sandbox
		"LAUNCH_DIR='/home/kyber/dev/test-agent'",
		`CLAUDE_ARGS="--dangerously-skip-permissions --model claude-test"`,
		`CLAUDE_LAUNCH_ARGS="$CLAUDE_ARGS -- startup\ prompt"`,
		`CLAUDE_RESUME_ARGS="$CLAUDE_ARGS --continue -- startup\ prompt"`,
		"SESSION_RESUME_ENABLED=1",
		"CLAUDE_PROJECT_STORE='" + store + "'",
		"USER_PRESERVE_SUFFIX=''",
		"KYBER_SYNC_SCRIPT='" + filepath.Join(work, "no-such-sync") + "'",
		block,
		"",
	}, "\n")
	wrapperPath := filepath.Join(work, "wrapper.sh")
	if err := os.WriteFile(wrapperPath, []byte(wrapper), 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("/bin/bash", wrapperPath).CombinedOutput(); err != nil {
		t.Fatalf("rendering heredoc: %v\n%s", err, out)
	}

	// The generated script hardcodes /persist/var/lock, which doesn't exist
	// off a pod — repoint it at the sandbox before executing.
	raw, err := os.ReadFile(gen)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(gen,
		[]byte(strings.ReplaceAll(string(raw), "/persist/var/lock", lockDir)), 0o755); err != nil {
		t.Fatal(err)
	}

	run := func(arg ...string) string {
		t.Helper()
		os.Remove(sudoLog)
		cmd := exec.Command("/bin/bash", append([]string{gen}, arg...)...)
		cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("running generated script %v: %v\n%s", arg, err, out)
		}
		log, err := os.ReadFile(sudoLog)
		if err != nil {
			t.Fatalf("sudo stub never ran: %v", err)
		}
		return string(log)
	}

	if got := run(); !strings.Contains(got, "claude --dangerously-skip-permissions --model claude-test -- startup") ||
		strings.Contains(got, "--continue") {
		t.Errorf("empty store: want fresh launch with prompt, got:\n%s", got)
	}

	if err := os.WriteFile(filepath.Join(store, "session-1.jsonl"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := run(); !strings.Contains(got, "--continue") {
		t.Errorf("prior transcript: want --continue launch, got:\n%s", got)
	} else if !strings.Contains(got, "--continue -- startup") {
		t.Errorf("resume launch must deliver the startup prompt after --continue, got:\n%s", got)
	}

	if got := run("--fresh"); strings.Contains(got, "--continue") {
		t.Errorf("--fresh: intentional restart must stay fresh, got:\n%s", got)
	}

	// Race guard: a bare (watchdog) invocation that finds the session alive
	// must do nothing — a concurrent restart-session already relaunched, and
	// killing it here could resurrect the conversation that restart just
	// discarded. --fresh still kills + relaunches.
	if err := os.WriteFile(aliveFlag, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	os.Remove(sudoLog)
	cmd := exec.Command("/bin/bash", gen)
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("bare run with live session: %v\n%s", err, out)
	} else if !strings.Contains(string(out), "already alive") {
		t.Errorf("bare run with live session did not report the race skip:\n%s", out)
	}
	if _, err := os.Stat(sudoLog); err == nil {
		log, _ := os.ReadFile(sudoLog)
		t.Errorf("bare run with live session must not kill/relaunch, but ran:\n%s", log)
	}
	if got := run("--fresh"); !strings.Contains(got, "new-session") {
		t.Errorf("--fresh with live session must still relaunch, got:\n%s", got)
	}
}

//go:build integration

// Package identityreposhared pins the contract of the shared identity-repo
// script (kyber#676) independently of any one runtime.
//
// Why this file exists: the clone/sync logic used to live inline in
// start-claude.sh, so the Codex runtime shipped the CONSUMER of the identity
// repo without the producer — start-codex.sh checked for
// $HOME/dev/<repo>/.git to pick its launch dir, that check could never be
// true, and HK-47 (the first prod Codex agent) booted with no identity repo,
// no memory and no sync script while status.identityRepo reported "Ready".
//
// The 54 tests in images/claude-code/start_claude_test.go already cover the
// behaviour in depth via the Claude Code boot path and keep passing unchanged
// against the extracted script. What they CANNOT catch is the packaging
// mistake that caused this bug in the first place: a runtime that never
// invokes the shared script at all, or an image that never ships it. That is
// what is asserted here.
package identityreposhared

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func repoRoot(t *testing.T) string {
	t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	// This file lives at <root>/images/shared.
	return filepath.Dir(filepath.Dir(cwd))
}

func readFile(t *testing.T, parts ...string) string {
	t.Helper()
	p := filepath.Join(parts...)
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("reading %s: %v", p, err)
	}
	return string(b)
}

// The shared script must exist and be a no-op when no identity repo is
// configured — every runtime sources it unconditionally.
func TestSharedScript_ExistsAndGuardsOnIdentityRepoEnv(t *testing.T) {
	body := readFile(t, repoRoot(t), "images", "shared", "kyber-identity-repo.sh")
	if !strings.Contains(body, `if [ -n "${KYBER_IDENTITY_REPO:-}" ]; then`) {
		t.Error("shared script is not guarded on KYBER_IDENTITY_REPO — it would run for agents that have no identity repo")
	}
	if !strings.Contains(body, "git-credential-kyber-github") {
		t.Error("shared script does not install the git credential helper")
	}
	for _, skillsHome := range []string{`$HOME_DIR/.claude/skills`, `$HOME_DIR/.codex/skills`} {
		if !strings.Contains(body, skillsHome) {
			t.Errorf("shared script does not link identity skills into %s", skillsHome)
		}
	}
}

// EVERY runtime start script must source the shared script. A runtime that
// merely *reads* the clone (as start-codex.sh did) silently gets nothing.
func TestEveryRuntimeStartScript_SourcesTheSharedScript(t *testing.T) {
	root := repoRoot(t)
	for _, rt := range []struct{ name, script string }{
		{"claude-code", filepath.Join("images", "claude-code", "start-claude.sh")},
		{"codex", filepath.Join("images", "codex", "start-codex.sh")},
	} {
		t.Run(rt.name, func(t *testing.T) {
			body := readFile(t, root, rt.script)
			if !strings.Contains(body, "kyber-identity-repo.sh") {
				t.Fatalf("%s never references the shared identity-repo script — "+
					"this runtime's agents will boot with no identity repo, no memory and no sync script (kyber#676)", rt.script)
			}
			// Sourced, not executed: it exports REPO_DIR into the caller's shell.
			if !strings.Contains(body, `. "$_kyber_identity_script"`) {
				t.Errorf("%s does not SOURCE the shared script; running it in a subshell would discard REPO_DIR", rt.script)
			}
		})
	}
}

// Shipping the script matters as much as calling it: a runtime image that
// sources a file it never COPYs fails at boot, in production, on a path no
// unit test exercises.
func TestEveryRuntimeImage_ShipsTheSharedScript(t *testing.T) {
	root := repoRoot(t)
	for _, df := range []string{
		filepath.Join("images", "claude-code", "Dockerfile"),
		filepath.Join("images", "codex", "Dockerfile"),
	} {
		t.Run(df, func(t *testing.T) {
			body := readFile(t, root, df)
			if !strings.Contains(body, "images/shared/kyber-identity-repo.sh") {
				t.Errorf("%s does not COPY the shared identity-repo script — the image would source a file it does not ship", df)
			}
		})
	}
}

// Codex specifically: the launch-dir logic must run AFTER the clone. Getting
// this order wrong reproduces the original bug exactly — the .git check runs
// before anything created it, so it silently falls back to $HOME.
func TestCodex_ClonesBeforeChoosingLaunchDir(t *testing.T) {
	body := readFile(t, repoRoot(t), "images", "codex", "start-codex.sh")
	srcIdx := strings.Index(body, `. "$_kyber_identity_script"`)
	launchIdx := strings.Index(body, `LAUNCH_DIR="$HOME"`)
	if srcIdx < 0 || launchIdx < 0 {
		t.Fatalf("could not locate both markers (source=%d launchdir=%d)", srcIdx, launchIdx)
	}
	if srcIdx > launchIdx {
		t.Error("start-codex.sh picks LAUNCH_DIR before sourcing the identity-repo script — " +
			"the $HOME/dev/<repo>/.git check would run before the clone exists and always fall back to $HOME (kyber#676)")
	}
}

// CI must actually run the boot suite when the shared script changes. This is
// the kyber#655 lesson: that suite sat red for five weeks because the files it
// covered were outside the workflow's path filter.
func TestCI_RebuildsAndRetestsOnSharedScriptChanges(t *testing.T) {
	root := repoRoot(t)
	integration := readFile(t, root, ".github", "workflows", "integration.yml")
	if !strings.Contains(integration, "images/shared/**") {
		t.Error("integration.yml does not watch images/shared/** — the 54-test boot suite would not run against a change to the shared identity script")
	}
	build := readFile(t, root, ".github", "workflows", "build.yml")
	if strings.Count(build, "images/shared/**") < 2 {
		t.Error("build.yml must watch images/shared/** for BOTH runtime image filters, or a change ships to neither image")
	}
}

// extractCredentialHelper pulls the git credential helper out of the heredoc in
// kyber-identity-repo.sh and writes it to a runnable temp file. The helper is
// generated at boot rather than shipped as its own file, so executing it is the
// only way to test its behaviour rather than its text.
func extractCredentialHelper(t *testing.T) string {
	t.Helper()
	body := readFile(t, repoRoot(t), "images", "shared", "kyber-identity-repo.sh")
	const startMark = "<<'HELPER_EOF'\n"
	const endMark = "\nHELPER_EOF"
	i := strings.Index(body, startMark)
	if i < 0 {
		t.Fatal("could not find the credential-helper heredoc opener in the shared script")
	}
	rest := body[i+len(startMark):]
	j := strings.Index(rest, endMark)
	if j < 0 {
		t.Fatal("could not find the credential-helper heredoc terminator")
	}
	path := filepath.Join(t.TempDir(), "git-credential-kyber-github")
	if err := os.WriteFile(path, []byte(rest[:j]), 0o700); err != nil {
		t.Fatalf("writing helper: %v", err)
	}
	return path
}

// runCredentialHelper executes the helper with a controlled environment and the
// key=value request block git would send on stdin.
func runCredentialHelper(t *testing.T, helper string, env []string, stdin string) (string, string) {
	t.Helper()
	cmd := exec.Command("/bin/sh", helper, "get")
	cmd.Env = env
	cmd.Stdin = strings.NewReader(stdin)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		t.Fatalf("helper exited non-zero (it must always exit 0 so git can proceed): %v\nstderr: %s", err, errb.String())
	}
	return out.String(), errb.String()
}

// TestCredentialHelper_NamesTheReasonInsteadOfFailingSilently pins the
// diagnostic contract of the credential helper.
//
// Every failure path in this helper ends with git receiving NO credential, and
// git then retries ANONYMOUSLY. Against a private repo GitHub answers 404, so
// the operator sees "Repository not found" — which reads as "the repo is gone"
// or "the App lost access" when the truth is "this helper produced nothing".
//
// That cost real time on 2026-08-06: hk-47 hit it on kyber-lonestar, reported
// the repo as missing and the App as unauthorized, and refused to edit his
// identity until both were verified. Both were fine — the platform's own boot
// sync had pulled that same repo seconds earlier. The helper had simply exited
// silently. The script's own comment claimed it "fails LOUDLY"; it did not.
//
// So: every no-credential path must say why on stderr (git forwards it), and
// must warn that the 404 about to appear is a symptom, not the cause. The
// happy paths must stay silent — a helper that chatters on every successful
// fetch of an unrelated repo would be its own kind of noise.
func TestCredentialHelper_NamesTheReasonInsteadOfFailingSilently(t *testing.T) {
	helper := extractCredentialHelper(t)
	const req = "protocol=https\nhost=github.com\npath=matty-v/hk-47-agent\n\n"

	t.Run("identity repo unrecognised and no PAT", func(t *testing.T) {
		// hk-47's suspected case: KYBER_IDENTITY_REPO empty, so even his own
		// repo takes the PAT branch, and there is no PAT.
		stdout, stderr := runCredentialHelper(t, helper, []string{"HOME=" + t.TempDir()}, req)
		if stdout != "" {
			t.Errorf("must emit no credential, got stdout %q", stdout)
		}
		if !strings.Contains(stderr, "KYBER_IDENTITY_REPO") {
			t.Errorf("stderr must name KYBER_IDENTITY_REPO as the cause; got: %s", stderr)
		}
		if !strings.Contains(stderr, "Repository not found") {
			t.Errorf("stderr must pre-empt the misleading 404 the operator is about to see; got: %s", stderr)
		}
	})

	t.Run("useHttpPath disabled", func(t *testing.T) {
		// No path= line: git cannot tell the helper which repo it wants, so the
		// identity repo is indistinguishable from any other.
		stdout, stderr := runCredentialHelper(t, helper,
			[]string{"HOME=" + t.TempDir(), "KYBER_IDENTITY_REPO=matty-v/hk-47-agent"},
			"protocol=https\nhost=github.com\n\n")
		if stdout != "" {
			t.Errorf("must emit no credential, got stdout %q", stdout)
		}
		if !strings.Contains(stderr, "useHttpPath") {
			t.Errorf("stderr must name useHttpPath as the cause; got: %s", stderr)
		}
	})

	t.Run("app mint fails", func(t *testing.T) {
		// Correctly recognised as the identity repo, but the App flow cannot
		// run. Must report the App failure, not fall back to a PAT.
		stdout, stderr := runCredentialHelper(t, helper,
			[]string{"HOME=" + t.TempDir(), "KYBER_IDENTITY_REPO=matty-v/hk-47-agent", "AGENT_NAME=hk-47"},
			req)
		if stdout != "" {
			t.Errorf("identity repo must never fall back to a PAT; got stdout %q", stdout)
		}
		if !strings.Contains(stderr, "Kyber Platform App failed") {
			t.Errorf("stderr must report the App mint failure; got: %s", stderr)
		}
	})

	t.Run("unrelated repo with a PAT stays silent", func(t *testing.T) {
		// The common path. Must emit the PAT and say nothing — otherwise every
		// ordinary fetch of a non-identity repo becomes noisy.
		stdout, stderr := runCredentialHelper(t, helper,
			[]string{"HOME=" + t.TempDir(), "KYBER_IDENTITY_REPO=matty-v/hk-47-agent", "GH_TOKEN=ghp_example"},
			"protocol=https\nhost=github.com\npath=matty-v/some-other-repo\n\n")
		if !strings.Contains(stdout, "password=ghp_example") {
			t.Errorf("must emit the PAT for a non-identity repo; got stdout %q", stdout)
		}
		if stderr != "" {
			t.Errorf("happy path must be silent; got stderr: %s", stderr)
		}
	})
}

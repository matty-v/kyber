//go:build integration

package startclaudeshell_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/matty-v/kyber/pkg/oauth/mockserver"
)

// bootWithSkillEnv runs start-claude.sh with the minimum environment a boot
// needs (the OAuth rotation endpoint is mandatory) plus whatever the caller
// adds. Mirrors runIdentityBoot, which cannot be reused directly because these
// tests need to inject PATH and extra variables.
func bootWithSkillEnv(t *testing.T, home string, extra ...string) ([]byte, error) {
	t.Helper()
	mock := mockserver.New()
	ts := httptest.NewServer(mock)
	defer ts.Close()
	cpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(204)
	}))
	defer cpServer.Close()
	refreshToken := seedRefreshToken(t, mock, ts.URL)

	env := append([]string{
		"HOME=" + home,
		"CLAUDE_REFRESH_TOKEN=" + refreshToken,
		"AGENT_NAME=unit-test",
		"ANTHROPIC_TOKEN_URL=" + ts.URL + "/v1/oauth/token",
		"KYBER_REFRESH_TOKEN_URL=" + cpServer.URL + "/internal/agents/unit-test/refresh-token",
		"SKIP_CLAUDE_LAUNCH=1",
	}, extra...)
	return runScript(t, env)
}

// stubKyberSkills puts a fake `kyber-skills` on PATH that records the argv it
// was called with. Returns the stub's bin dir and the path of the record file.
func stubKyberSkills(t *testing.T) (binDir, record string) {
	t.Helper()
	binDir = t.TempDir()
	record = filepath.Join(binDir, "invocations.txt")
	body := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + record + "\nexit 0\n"
	stub := filepath.Join(binDir, "kyber-skills")
	if err := os.WriteFile(stub, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return binDir, record
}

// waitForFile polls until path exists and is non-empty. The boot script starts
// the reporter in the background, so the script can exit before the stub has
// written anything.
func waitForFile(t *testing.T, path string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(path); err == nil && len(b) > 0 {
			return string(b)
		}
		time.Sleep(50 * time.Millisecond)
	}
	return ""
}

// TestStartClaude_SkillReport_FiresAtBootAndCannotBreakIt covers both halves of
// the boot-time skill report. It must actually run — a reporter that silently
// never fires is how the panels stay dark — and it must not be able to take the
// boot down with it. The script runs under `set -e` and this block sits well
// before the token-reporter block that creates /persist/var/log, so an
// unguarded redirect here would kill every agent's boot on any install where
// that directory is not already writable.
func TestStartClaude_SkillReport_FiresAtBootAndCannotBreakIt(t *testing.T) {
	binDir, record := stubKyberSkills(t)
	tmpHome := t.TempDir()
	// A deliberately unwritable log directory: the block must fall back to
	// $HOME rather than fail the redirect.
	unwritable := filepath.Join(t.TempDir(), "no-such-parent", "nested", "kyber-skills.log")

	out, err := bootWithSkillEnv(t, tmpHome,
		"PATH="+binDir+":"+testPATH(),
		"KYBER_SKILLS_LOG="+unwritable,
	)
	if err != nil {
		t.Fatalf("boot must survive an unwritable skill-report log path: %v\noutput:\n%s", err, out)
	}

	got := waitForFile(t, record, 5*time.Second)
	if got == "" {
		t.Fatalf("kyber-skills was never invoked at boot\noutput:\n%s", out)
	}
	if !strings.Contains(got, "report") {
		t.Errorf("expected a `report` invocation, got %q", got)
	}
	if !strings.Contains(got, "--home "+tmpHome) {
		t.Errorf("expected --home %s in the invocation, got %q", tmpHome, got)
	}
	if !strings.Contains(string(out), "skill report started") {
		t.Errorf("expected the boot log to name the skill report; got:\n%s", out)
	}
}

// The block is guarded on the binary existing, so an older image (or a runtime
// built before this shipped) boots exactly as before.
func TestStartClaude_SkillReport_AbsentBinaryIsHarmless(t *testing.T) {
	// A PATH with no kyber-skills on it: t.TempDir() is empty.
	emptyBin := t.TempDir()
	tmpHome := t.TempDir()
	out, err := bootWithSkillEnv(t, tmpHome,
		"PATH="+emptyBin+":"+testPATH(),
		"KYBER_SKILLS_LOG="+filepath.Join(t.TempDir(), "kyber-skills.log"),
	)
	if err != nil {
		t.Fatalf("boot failed with no kyber-skills present: %v\noutput:\n%s", err, out)
	}
	if strings.Contains(string(out), "skill report started") {
		t.Errorf("skill report should not run without the binary; got:\n%s", out)
	}
}

// The generated sync script is what runs on a restart-session, under nsenter as
// root where the pod's environment is NOT visible. The report call must
// therefore carry its paths as literal arguments, not read them from the
// environment — the same trap that made KYBER_IDENTITY_RUNTIME unusable there.
func TestStartClaude_SyncScript_ReportsSkillsWithExplicitPaths(t *testing.T) {
	binDir, _ := stubKyberSkills(t)
	tmpHome := t.TempDir()
	syncScript := filepath.Join(t.TempDir(), "kyber-sync-identity.sh")

	out, err := bootWithSkillEnv(t, tmpHome,
		"PATH="+binDir+":"+testPATH(),
		"KYBER_IDENTITY_REPO=matty-v/does-not-exist-skill-sync",
		"KYBER_SYNC_SCRIPT="+syncScript,
		"KYBER_SKILLS_LOG="+filepath.Join(t.TempDir(), "kyber-skills.log"),
	)
	if err != nil {
		t.Fatalf("boot failed: %v\noutput:\n%s", err, out)
	}

	body, err := os.ReadFile(syncScript)
	if err != nil {
		t.Fatalf("sync script not generated at %s: %v\noutput:\n%s", syncScript, err, out)
	}
	src := string(body)
	if !strings.Contains(src, "kyber-skills report") {
		t.Fatalf("generated sync script does not report skills:\n%s", src)
	}
	for _, want := range []string{`--repo-dir "$REPO_DIR"`, `--home "$HOME_DIR"`} {
		if !strings.Contains(src, want) {
			t.Errorf("sync script must pass %s explicitly (PID1 env is invisible under nsenter):\n%s", want, src)
		}
	}
	// A generated script that does not parse is a restart-session that
	// silently does nothing.
	if out, err := exec.Command("bash", "-n", syncScript).CombinedOutput(); err != nil {
		t.Fatalf("generated sync script is not valid bash: %v\n%s", err, out)
	}
}

// TestStartClaude_BootLinksSkillsIntoBothRuntimeHomes exercises the real
// linking path end to end against a real clone: identity skills, vendored
// skills, and the README that is documentation rather than a skill.
//
// Nothing covered this before, which is how kyber#691 stayed invisible — the
// boot log said "skills re-linked" while linking into a path nothing read. The
// assertion here is the resulting filesystem, never the log line.
func TestStartClaude_BootLinksSkillsIntoBothRuntimeHomes(t *testing.T) {
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
	writeFile := func(path, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	work := t.TempDir()
	remote := filepath.Join(work, "remote.git")
	git(work, "init", "--bare", "-b", "main", remote)
	seed := filepath.Join(work, "seed")
	git(work, "clone", remote, seed)

	writeFile(filepath.Join(seed, "skills", "deploy", "SKILL.md"),
		"---\nname: deploy\ndescription: Ship it.\n---\nbody\n")
	writeFile(filepath.Join(seed, "skills", "deploy", "references", "notes.md"), "bundled\n")
	writeFile(filepath.Join(seed, "vendor", "shared-pkg", "skills", "triage", "SKILL.md"),
		"---\nname: triage\ndescription: Triage it.\n---\nbody\n")
	// Documentation that lives in skills/ and must NOT become a command.
	writeFile(filepath.Join(seed, "skills", "README.md"), "# Skills\n\nHow to add one.\n")
	git(seed, "add", "-A")
	git(seed, "commit", "-m", "skills")
	git(seed, "push", "origin", "main")

	repoName := "test-skills-" + fmt.Sprint(time.Now().UnixNano())
	repoDir := filepath.Join(tmpHome, "dev", repoName)
	if err := os.MkdirAll(filepath.Dir(repoDir), 0o755); err != nil {
		t.Fatal(err)
	}
	git(work, "clone", remote, repoDir)

	out, err := bootWithSkillEnv(t, tmpHome,
		"PATH="+testPATH(),
		"KYBER_IDENTITY_REPO=matty-v/"+repoName,
		"KYBER_SYNC_SCRIPT="+filepath.Join(t.TempDir(), "kyber-sync-identity.sh"),
		"KYBER_SKILLS_LOG="+filepath.Join(t.TempDir(), "kyber-skills.log"),
	)
	if err != nil {
		t.Fatalf("boot failed: %v\noutput:\n%s", err, out)
	}

	// Both runtime homes, because one identity repo has to work under either
	// runtime — a skill live in only one of them is the kyber#691 shape.
	for _, home := range []string{".claude", ".codex"} {
		for _, want := range []struct{ name, wantTarget string }{
			{"deploy", filepath.Join(repoDir, "skills", "deploy")},
			{"triage", filepath.Join(repoDir, "vendor", "shared-pkg", "skills", "triage")},
		} {
			link := filepath.Join(tmpHome, home, "skills", want.name)
			got, err := filepath.EvalSymlinks(link)
			if err != nil {
				t.Errorf("~/%s/skills/%s is not linked: %v\noutput:\n%s", home, want.name, err, out)
				continue
			}
			resolvedWant, err := filepath.EvalSymlinks(want.wantTarget)
			if err != nil {
				t.Fatal(err)
			}
			if got != resolvedWant {
				t.Errorf("~/%s/skills/%s → %s, want %s", home, want.name, got, resolvedWant)
			}
			// The whole package is linked, not just SKILL.md, so bundled
			// references resolve.
			if want.name == "deploy" {
				if _, err := os.Stat(filepath.Join(link, "references", "notes.md")); err != nil {
					t.Errorf("bundled references did not resolve through the link: %v", err)
				}
			}
		}
		if _, err := os.Lstat(filepath.Join(tmpHome, home, "skills", "README")); err == nil {
			t.Errorf("~/%s/skills/README exists — a README in skills/ is documentation, not a command", home)
		}
	}
}

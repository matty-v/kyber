package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/matty-v/kyber/pkg/skillscan"
)

// gitEnv isolates the test's git from the machine's real config and identity.
func gitEnv(home string) []string {
	return []string{
		"HOME=" + home,
		"PATH=" + os.Getenv("PATH"),
		"GIT_CONFIG_GLOBAL=" + filepath.Join(home, ".gitconfig-test"),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
	}
}

func runGit(t *testing.T, home, dir string, args ...string) string {
	t.Helper()
	c := exec.Command("git", args...)
	c.Dir = dir
	c.Env = gitEnv(home)
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// repoFixture builds a home, an identity repo clone with a bare remote behind
// it, and both runtime skill homes.
type repoFixture struct {
	home    string
	repoDir string
	remote  string
}

func newRepoFixture(t *testing.T) *repoFixture {
	t.Helper()
	home := t.TempDir()
	work := t.TempDir()
	remote := filepath.Join(work, "remote.git")
	runGit(t, home, work, "init", "--bare", "-b", "main", remote)

	seed := filepath.Join(work, "seed")
	runGit(t, home, work, "clone", remote, seed)
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, home, seed, "add", "-A")
	runGit(t, home, seed, "commit", "-m", "seed")
	runGit(t, home, seed, "push", "origin", "main")

	repoDir := filepath.Join(home, "dev", "agent-repo")
	if err := os.MkdirAll(filepath.Dir(repoDir), 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, home, work, "clone", remote, repoDir)

	for _, d := range []string{".claude/skills", ".codex/skills"} {
		if err := os.MkdirAll(filepath.Join(home, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return &repoFixture{home: home, repoDir: repoDir, remote: remote}
}

func (f *repoFixture) writeSkill(t *testing.T, name, body string) {
	t.Helper()
	dir := filepath.Join(f.repoDir, "skills", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// captureSidecar stands in for the status sidecar's localhost forwarder and
// records the reports it receives.
func captureSidecar(t *testing.T, status int) (*httptest.Server, *[]skillscan.Report) {
	t.Helper()
	var got []skillscan.Report
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/skills" {
			t.Errorf("reporter posted to %s, want /skills", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		var rep skillscan.Report
		if err := json.Unmarshal(body, &rep); err != nil {
			t.Errorf("report is not valid JSON: %v", err)
		}
		got = append(got, rep)
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv, &got
}

const skillBody = "---\nname: deploy\ndescription: Ship it.\n---\nbody\n"

// The command an agent is told to run after writing a skill. It has to do the
// whole job in one go: linking it so the skill works NOW rather than at the
// next boot, and pushing it so the skill survives a reprovision.
func TestInstall_LinksIntoBothRuntimesAndPushes(t *testing.T) {
	f := newRepoFixture(t)
	f.writeSkill(t, "deploy", skillBody)
	sidecar, reports := captureSidecar(t, http.StatusNoContent)
	t.Setenv("KYBER_SIDECAR_URL", sidecar.URL)
	t.Setenv("HOME", f.home)
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(f.home, ".gitconfig-test"))
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@t")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@t")

	if code := runInstall([]string{"--repo-dir", f.repoDir, "--home", f.home}); code != 0 {
		t.Fatalf("install exit code = %d, want 0", code)
	}

	for _, home := range []string{".claude", ".codex"} {
		link := filepath.Join(f.home, home, "skills", "deploy")
		target, err := filepath.EvalSymlinks(link)
		if err != nil {
			t.Errorf("~/%s/skills/deploy not linked: %v", home, err)
			continue
		}
		want, _ := filepath.EvalSymlinks(filepath.Join(f.repoDir, "skills", "deploy"))
		if target != want {
			t.Errorf("~/%s/skills/deploy → %s, want %s", home, target, want)
		}
	}

	// Pushed, not just committed: a skill that only exists in the pod's clone
	// dies with the pod.
	remoteFiles := runGit(t, f.home, f.repoDir, "ls-tree", "-r", "--name-only", "origin/main")
	if !strings.Contains(remoteFiles, "skills/deploy/SKILL.md") {
		t.Errorf("skill was not pushed to the remote; remote tree:\n%s", remoteFiles)
	}

	if len(*reports) != 1 {
		t.Fatalf("expected exactly one report, got %d", len(*reports))
	}
	rep := (*reports)[0]
	if len(rep.Skills) != 1 || rep.Skills[0].Name != "deploy" {
		t.Fatalf("reported skills = %+v", rep.Skills)
	}
	if len(rep.Skills[0].Linked) != 2 {
		t.Errorf("reported as linked in %v, want both runtimes", rep.Skills[0].Linked)
	}
}

// Running it twice must be safe — an agent will, and so will a retry.
func TestInstall_IsIdempotent(t *testing.T) {
	f := newRepoFixture(t)
	f.writeSkill(t, "deploy", skillBody)
	sidecar, _ := captureSidecar(t, http.StatusNoContent)
	t.Setenv("KYBER_SIDECAR_URL", sidecar.URL)
	t.Setenv("HOME", f.home)
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(f.home, ".gitconfig-test"))
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@t")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@t")

	args := []string{"--repo-dir", f.repoDir, "--home", f.home}
	for i := 0; i < 2; i++ {
		if code := runInstall(args); code != 0 {
			t.Fatalf("install run %d exit code = %d, want 0", i+1, code)
		}
	}
	// The second run has nothing to commit; it must not create an empty commit.
	log := runGit(t, f.home, f.repoDir, "log", "--oneline")
	if n := strings.Count(log, "kyber-skills"); n != 1 {
		t.Errorf("expected exactly one kyber-skills commit, got %d:\n%s", n, log)
	}
}

func TestInstall_FromDirectoryAndFile(t *testing.T) {
	tests := []struct {
		name     string
		makeSrc  func(t *testing.T, dir string) string
		extraArg []string
		wantName string
	}{
		{
			name: "directory keeps bundled files",
			makeSrc: func(t *testing.T, dir string) string {
				src := filepath.Join(dir, "downloaded-skill")
				if err := os.MkdirAll(filepath.Join(src, "references"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(src, "SKILL.md"), []byte(skillBody), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(src, "references", "notes.md"), []byte("x"), 0o644); err != nil {
					t.Fatal(err)
				}
				return src
			},
			wantName: "downloaded-skill",
		},
		{
			name: "single markdown file becomes SKILL.md",
			makeSrc: func(t *testing.T, dir string) string {
				src := filepath.Join(dir, "quickfix.md")
				if err := os.WriteFile(src, []byte(skillBody), 0o644); err != nil {
					t.Fatal(err)
				}
				return src
			},
			wantName: "quickfix",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newRepoFixture(t)
			sidecar, _ := captureSidecar(t, http.StatusNoContent)
			t.Setenv("KYBER_SIDECAR_URL", sidecar.URL)
			src := tc.makeSrc(t, t.TempDir())

			args := append([]string{"--repo-dir", f.repoDir, "--home", f.home, "--no-push", "--from", src}, tc.extraArg...)
			if code := runInstall(args); code != 0 {
				t.Fatalf("install exit code = %d, want 0", code)
			}
			dest := filepath.Join(f.repoDir, "skills", tc.wantName)
			if _, err := os.Stat(filepath.Join(dest, "SKILL.md")); err != nil {
				t.Fatalf("%s/SKILL.md missing: %v", dest, err)
			}
			if tc.wantName == "downloaded-skill" {
				if _, err := os.Stat(filepath.Join(dest, "references", "notes.md")); err != nil {
					t.Errorf("bundled references were not copied: %v", err)
				}
			}
			if _, err := os.Lstat(filepath.Join(f.home, ".codex", "skills", tc.wantName)); err != nil {
				t.Errorf("imported skill was not linked for Codex: %v", err)
			}
		})
	}
}

// A bare SKILL.md carries no name of its own, and guessing "SKILL" would
// publish a command called SKILL.
func TestInstall_BareSkillMdNeedsAnExplicitName(t *testing.T) {
	f := newRepoFixture(t)
	src := filepath.Join(t.TempDir(), "SKILL.md")
	if err := os.WriteFile(src, []byte(skillBody), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := runInstall([]string{"--repo-dir", f.repoDir, "--home", f.home, "--no-push", "--from", src}); code == 0 {
		t.Fatal("expected a non-zero exit for an underivable skill name")
	}
	if code := runInstall([]string{"--repo-dir", f.repoDir, "--home", f.home, "--no-push", "--from", src, "--name", "renamed"}); code != 0 {
		t.Fatalf("install with --name exit code = %d, want 0", code)
	}
	if _, err := os.Stat(filepath.Join(f.repoDir, "skills", "renamed", "SKILL.md")); err != nil {
		t.Errorf("skill not installed under the given name: %v", err)
	}
}

func TestValidSkillName(t *testing.T) {
	for _, ok := range []string{"deploy", "kyber-release", "w", "compact_memory", "v2"} {
		if err := validSkillName(ok); err != nil {
			t.Errorf("validSkillName(%q) = %v, want nil", ok, err)
		}
	}
	// Path separators and traversal matter most: the name becomes a directory
	// under the identity repo.
	for _, bad := range []string{"", "Deploy", "a b", "../escape", "a/b", strings.Repeat("x", 65)} {
		if err := validSkillName(bad); err == nil {
			t.Errorf("validSkillName(%q) = nil, want an error", bad)
		}
	}
}

// The retry exists for one thing: a sidecar that has not bound its listener
// yet. An actual HTTP answer will not change on the second ask, and retrying it
// would add 12 seconds to every boot on an install that has no skill store.
func TestPostReportWithRetry_DoesNotRetryAnHTTPResponse(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	t.Setenv("KYBER_SIDECAR_URL", srv.URL)

	err := postReportWithRetry(&skillscan.Report{Version: skillscan.ReportVersion})
	if err == nil {
		t.Fatal("expected an error")
	}
	var respErr *responseError
	if !errors.As(err, &respErr) {
		t.Errorf("error = %v, want a responseError", err)
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Errorf("posted %d times, want 1 — an HTTP response must not be retried", n)
	}
}

func TestReport_MissingIdentityRepoIsNotAFailure(t *testing.T) {
	t.Setenv("KYBER_IDENTITY_REPO", "")
	t.Setenv("HOME", t.TempDir())
	// An agent configured with no identity repo has no skills to report; that
	// must not surface as a boot-time failure.
	if code := runReport([]string{"--home", t.TempDir()}); code != 0 {
		t.Errorf("report exit code = %d, want 0", code)
	}
}

func TestResolve_DerivesRepoDirFromTheIdentitySlug(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KYBER_IDENTITY_REPO", "matty-v/dave-agent")
	p := paths{homeDir: home}
	if err := p.resolve(); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if want := filepath.Join(home, "dev", "dave-agent"); p.repoDir != want {
		t.Errorf("repoDir = %q, want %q", p.repoDir, want)
	}
}

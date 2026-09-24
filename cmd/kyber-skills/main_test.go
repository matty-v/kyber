package main

import (
	"bytes"
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
	"time"

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
// it, and every runtime skill home.
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

	for _, d := range []string{".claude/skills", ".codex/skills", ".hermes/skills"} {
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

func (f *repoFixture) writeFlatSkill(t *testing.T, name, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(f.repoDir, "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.repoDir, "skills", name+".md"), []byte(body), 0o644); err != nil {
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

	for _, home := range []string{".claude", ".codex", ".hermes"} {
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
	if len(rep.Skills[0].Linked) != 3 {
		t.Errorf("reported as linked in %v, want every runtime", rep.Skills[0].Linked)
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

// Both start scripts pass `--repo-dir ""` for an agent with no identity repo.
// That agent still has the image's bundled skills, so it must still POST — an
// early return here is what would leave it stuck on "No report yet" forever,
// which is the blind spot the feature exists to remove.
func TestReport_NoIdentityRepoStillPostsPlatformSkills(t *testing.T) {
	home := t.TempDir()
	platform := filepath.Join(t.TempDir(), "opt-kyber-skills")
	tg := filepath.Join(platform, "telegram-messaging")
	if err := os.MkdirAll(tg, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tg, "SKILL.md"),
		[]byte("---\nname: telegram-messaging\ndescription: Talk on Telegram.\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(home, ".claude", "skills")
	if err := os.MkdirAll(link, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(tg, filepath.Join(link, "telegram-messaging")); err != nil {
		t.Fatal(err)
	}

	sidecar, reports := captureSidecar(t, http.StatusNoContent)
	t.Setenv("KYBER_SIDECAR_URL", sidecar.URL)
	t.Setenv("KYBER_IDENTITY_REPO", "")

	// Exactly what the identity sync passes in this case.
	code := runReport([]string{"--once", "--repo-dir", "", "--home", home, "--platform-dir", platform})
	if code != 0 {
		t.Fatalf("report exit code = %d, want 0", code)
	}
	if len(*reports) != 1 {
		t.Fatalf("expected one POST, got %d — an agent with no identity repo never reports", len(*reports))
	}
	rep := (*reports)[0]
	if len(rep.Skills) != 1 || rep.Skills[0].Name != "telegram-messaging" {
		t.Fatalf("reported skills = %+v", rep.Skills)
	}
	if rep.Skills[0].Source != skillscan.SourcePlatform {
		t.Errorf("source = %q, want %q", rep.Skills[0].Source, skillscan.SourcePlatform)
	}
}

// An agent with nothing at all still reports, so the tab says "no skills"
// rather than "never reported" — those are different facts.
func TestReport_NothingInstalledStillPosts(t *testing.T) {
	sidecar, reports := captureSidecar(t, http.StatusNoContent)
	t.Setenv("KYBER_SIDECAR_URL", sidecar.URL)
	t.Setenv("KYBER_IDENTITY_REPO", "")
	if code := runReport([]string{"--once", "--repo-dir", "", "--home", t.TempDir()}); code != 0 {
		t.Fatalf("report exit code = %d, want 0", code)
	}
	if len(*reports) != 1 {
		t.Fatalf("expected one POST, got %d", len(*reports))
	}
}

// install is the one command that genuinely needs a repo: there is nowhere
// else a skill can be saved and survive a reprovision.
func TestInstall_WithoutAnIdentityRepoFailsLoudly(t *testing.T) {
	t.Setenv("KYBER_IDENTITY_REPO", "")
	if code := runInstall([]string{"--repo-dir", "", "--home", t.TempDir(), "--no-push"}); code == 0 {
		t.Error("install must fail when there is no identity repo to save into")
	}
}

func TestRequireRepo_ExplainsWhereRepoLessSkillsLive(t *testing.T) {
	err := (&paths{}).requireRepo()
	if err == nil {
		t.Fatal("requireRepo with no repo: want an error")
	}
	for _, want := range []string{"no identity repository", "stays on this agent's disk"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q: want it to mention %q", err, want)
		}
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

// This is the regression that sent the whole design back: Echo wrote
// skills/cowsay/SKILL.md, committed it, and pushed it — everything an agent
// following the identity-repo convention should do — and the skill was neither
// loadable nor visible, because linking only ever ran at boot. Convergence is
// the platform's job, not the agent's memory.
func TestConverge_PicksUpASkillWrittenAfterBoot(t *testing.T) {
	f := newRepoFixture(t)
	sidecar, reports := captureSidecar(t, http.StatusNoContent)
	t.Setenv("KYBER_SIDECAR_URL", sidecar.URL)
	p := paths{repoDir: f.repoDir, homeDir: f.home}

	// Boot: nothing to find.
	if _, err := convergeAndReport(p, postReport); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if n := len((*reports)[0].Skills); n != 0 {
		t.Fatalf("expected an empty first report, got %d skills", n)
	}

	// The agent writes a skill mid-session and commits it, exactly as Echo
	// did. It never runs `kyber-skills install`.
	f.writeSkill(t, "cowsay", "---\nname: cowsay\ndescription: Make a cow say something.\n---\nbody\n")
	runGit(t, f.home, f.repoDir, "add", "-A")
	runGit(t, f.home, f.repoDir, "commit", "-m", "skill: add cowsay skill")
	runGit(t, f.home, f.repoDir, "push", "origin", "main")

	if _, err := convergeAndReport(p, postReport); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if len(*reports) != 2 {
		t.Fatalf("expected a second report, got %d", len(*reports))
	}
	rep := (*reports)[1]
	if len(rep.Skills) != 1 || rep.Skills[0].Name != "cowsay" {
		t.Fatalf("reported skills = %+v", rep.Skills)
	}
	// Loadable, not just listed: the convergence has to actually link it.
	if len(rep.Skills[0].Linked) != 3 {
		t.Errorf("linked = %v, want every runtime", rep.Skills[0].Linked)
	}
	for _, home := range []string{".claude", ".codex", ".hermes"} {
		if _, err := os.Lstat(filepath.Join(f.home, home, "skills", "cowsay")); err != nil {
			t.Errorf("~/%s/skills/cowsay was not linked: %v", home, err)
		}
	}
	if !rep.Skills[0].Healthy() {
		t.Errorf("a committed, pushed, linked skill should be clean; got %+v", rep.Skills[0].Issues)
	}
}

func TestConverge_LegacyFlatSkillIsManagedAndStaleLinksAreRemoved(t *testing.T) {
	f := newRepoFixture(t)
	f.writeFlatSkill(t, "approve", "---\nname: approve\ndescription: Approve a plan.\n---\nbody\n")
	for _, runtime := range []string{".claude", ".codex", ".hermes"} {
		stale := filepath.Join(f.home, runtime, "skills", "approve.md")
		if err := os.Symlink(filepath.Join(f.repoDir, "skills", "deleted.md"), stale); err != nil {
			t.Fatal(err)
		}
	}

	repJSON, err := convergeAndReport(paths{repoDir: f.repoDir, homeDir: f.home}, func(*skillscan.Report) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	var rep skillscan.Report
	if err := json.Unmarshal(repJSON, &rep); err != nil {
		t.Fatal(err)
	}
	if len(rep.Skills) != 1 || rep.Skills[0].Name != "approve" || len(rep.Skills[0].Linked) != 3 {
		t.Fatalf("flat skill report = %+v", rep.Skills)
	}
	if len(rep.Issues) != 0 {
		t.Fatalf("flat compatibility wrappers must be managed; issues = %+v", rep.Issues)
	}
	for _, runtime := range []string{".claude", ".codex", ".hermes"} {
		wrapper := filepath.Join(f.home, runtime, "skills", "approve")
		if !compatWrapperPointsAt(wrapper, filepath.Join(f.repoDir, "skills", "approve.md")) {
			t.Errorf("%s is not a managed compatibility wrapper", wrapper)
		}
		if _, err := os.Lstat(filepath.Join(f.home, runtime, "skills", "approve.md")); !os.IsNotExist(err) {
			t.Errorf("stale link still exists: %v", err)
		}
	}
}

func TestConverge_CanonicalPackageWinsOverLegacyFlatSkill(t *testing.T) {
	f := newRepoFixture(t)
	f.writeSkill(t, "approve", "---\nname: approve\ndescription: Canonical package.\n---\npackage\n")
	f.writeFlatSkill(t, "approve", "---\nname: approve\ndescription: Legacy copy.\n---\nlegacy\n")
	if _, err := convergeAndReport(paths{repoDir: f.repoDir, homeDir: f.home}, func(*skillscan.Report) error { return nil }); err != nil {
		t.Fatal(err)
	}
	rep, err := skillscan.Scan(skillscan.Options{RepoDir: f.repoDir, HomeDir: f.home})
	if err != nil {
		t.Fatal(err)
	}
	var canonical, legacy *skillscan.Skill
	for i := range rep.Skills {
		switch rep.Skills[i].Path {
		case "skills/approve":
			canonical = &rep.Skills[i]
		case "skills/approve.md":
			legacy = &rep.Skills[i]
		}
	}
	if canonical == nil || len(canonical.Linked) != 3 || canonical.Description != "Canonical package." {
		t.Fatalf("canonical package was not linked/won: %+v", canonical)
	}
	if legacy != nil && len(legacy.Linked) != 0 {
		t.Fatalf("legacy flat skill unexpectedly linked: %+v", legacy)
	}
}

func TestConverge_RemovesStaleGeneratedCompatibilityWrapper(t *testing.T) {
	f := newRepoFixture(t)
	stale := filepath.Join(f.home, ".codex", "skills", "old-skill")
	if err := os.MkdirAll(stale, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(f.repoDir, "skills", "removed.md"), filepath.Join(stale, "SKILL.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := convergeAndReport(paths{repoDir: f.repoDir, homeDir: f.home}, func(*skillscan.Report) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale generated wrapper remains: %v", err)
	}
}

// Auto-linking makes a skill work before it is safe. Without this the tab would
// show a brand-new uncommitted skill as perfectly healthy, right up until the
// pod was reprovisioned and it vanished — the false-healthy state this whole
// feature exists to remove.
func TestConverge_FlagsASkillThatIsNotInGitHubYet(t *testing.T) {
	f := newRepoFixture(t)
	sidecar, reports := captureSidecar(t, http.StatusNoContent)
	t.Setenv("KYBER_SIDECAR_URL", sidecar.URL)
	p := paths{repoDir: f.repoDir, homeDir: f.home}

	// Written, never committed.
	f.writeSkill(t, "draft", "---\nname: draft\ndescription: Not saved yet.\n---\nbody\n")
	if _, err := convergeAndReport(p, postReport); err != nil {
		t.Fatal(err)
	}
	sk := (*reports)[0].Skills[0]
	if !hasIssueCode(sk.Issues, skillscan.IssueNotPushed) {
		t.Fatalf("expected %s on an uncommitted skill; got %+v", skillscan.IssueNotPushed, sk.Issues)
	}
	if sk.Broken() {
		t.Error("not being pushed yet is a warning, not a failure — the skill works right now")
	}

	// Committed but not pushed is the same durability story.
	runGit(t, f.home, f.repoDir, "add", "-A")
	runGit(t, f.home, f.repoDir, "commit", "-m", "wip")
	if _, err := convergeAndReport(p, postReport); err != nil {
		t.Fatal(err)
	}
	sk = (*reports)[1].Skills[0]
	if !hasIssueCode(sk.Issues, skillscan.IssueNotPushed) {
		t.Fatalf("expected %s on a committed-but-unpushed skill; got %+v", skillscan.IssueNotPushed, sk.Issues)
	}

	// Pushed: clean.
	runGit(t, f.home, f.repoDir, "push", "origin", "main")
	if _, err := convergeAndReport(p, postReport); err != nil {
		t.Fatal(err)
	}
	sk = (*reports)[2].Skills[0]
	if hasIssueCode(sk.Issues, skillscan.IssueNotPushed) {
		t.Errorf("a pushed skill must not be flagged; got %+v", sk.Issues)
	}
}

// MAT-90 G12: the reconcile loop runs `git status` on the identity repo every
// tick. A plain status refreshes stat info and rewrites the index under
// index.lock, so a tick that coincided with the identity sync's checkout or
// merge failed the sync with "index.lock exists". kyber-skills only reads the
// repo here and must never take that lock.
func TestUnpushedPaths_DoesNotRewriteTheIndex(t *testing.T) {
	f := newRepoFixture(t)
	f.writeSkill(t, "deploy", skillBody)
	runGit(t, f.home, f.repoDir, "add", "-A")
	runGit(t, f.home, f.repoDir, "commit", "-m", "deploy")
	runGit(t, f.home, f.repoDir, "push", "origin", "main")

	// Stat-dirty but content-clean: exactly what makes a plain status refresh
	// and write the index back.
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(filepath.Join(f.repoDir, "skills", "deploy", "SKILL.md"), future, future); err != nil {
		t.Fatal(err)
	}
	f.writeSkill(t, "draft", "---\nname: draft\ndescription: Not saved yet.\n---\nbody\n")
	index := filepath.Join(f.repoDir, ".git", "index")
	before, err := os.Stat(index)
	if err != nil {
		t.Fatal(err)
	}

	if got := unpushedPaths(f.repoDir); len(got) != 1 || got[0] != "skills/draft/SKILL.md" {
		t.Fatalf("unpushedPaths = %v, want [skills/draft/SKILL.md]", got)
	}
	after, err := os.Stat(index)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) || !after.ModTime().Equal(before.ModTime()) {
		t.Error("unpushedPaths rewrote .git/index, so it took index.lock and can collide with the identity sync")
	}
}

// A lock someone else holds must neither break the read nor be disturbed.
func TestUnpushedPaths_WorksWhileAnotherProcessHoldsIndexLock(t *testing.T) {
	f := newRepoFixture(t)
	f.writeSkill(t, "draft", "---\nname: draft\ndescription: Not saved yet.\n---\nbody\n")
	lock := filepath.Join(f.repoDir, ".git", "index.lock")
	if err := os.WriteFile(lock, []byte("held"), 0o644); err != nil {
		t.Fatal(err)
	}

	if got := unpushedPaths(f.repoDir); len(got) != 1 || got[0] != "skills/draft/SKILL.md" {
		t.Fatalf("unpushedPaths with a held lock = %v, want [skills/draft/SKILL.md]", got)
	}
	if b, err := os.ReadFile(lock); err != nil || string(b) != "held" {
		t.Errorf("the held index.lock was disturbed: %q, %v", b, err)
	}
}

// A steady state must be silent. An endless drip of identical reports would
// bury the one tick that matters, and writes to the store for no reason.
func TestConverge_SnapshotIsStableWhenNothingChanges(t *testing.T) {
	f := newRepoFixture(t)
	f.writeSkill(t, "deploy", skillBody)
	sidecar, _ := captureSidecar(t, http.StatusNoContent)
	t.Setenv("KYBER_SIDECAR_URL", sidecar.URL)
	p := paths{repoDir: f.repoDir, homeDir: f.home}

	first, err := convergeAndReport(p, postReport)
	if err != nil {
		t.Fatal(err)
	}
	second, err := convergeAndReport(p, postReport)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Errorf("an unchanged pod produced two different snapshots:\n%s\n%s", first, second)
	}
}

// Relinking on a loop must not churn: an already-correct link is left alone, so
// a runtime reading the directory never sees it disappear and reappear.
func TestLinkAll_LeavesCorrectLinksAlone(t *testing.T) {
	f := newRepoFixture(t)
	f.writeSkill(t, "deploy", skillBody)

	n, err := linkAll(f.repoDir, f.home)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("first link count = %d, want 1", n)
	}
	before, err := os.Lstat(filepath.Join(f.home, ".claude", "skills", "deploy"))
	if err != nil {
		t.Fatal(err)
	}

	n, err = linkAll(f.repoDir, f.home)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("second link count = %d, want 0 — an already-correct link must not be recreated", n)
	}
	after, err := os.Lstat(filepath.Join(f.home, ".claude", "skills", "deploy"))
	if err != nil {
		t.Fatal(err)
	}
	if !before.ModTime().Equal(after.ModTime()) {
		t.Error("the link was recreated despite already being correct")
	}
}

func hasIssueCode(issues []skillscan.Issue, code string) bool {
	for _, i := range issues {
		if i.Code == code {
			return true
		}
	}
	return false
}

// MAT-85: a skill kept only in one runtime home (the one place an agent with no
// identity repo can keep a skill) must survive a harness switch, which means
// every runtime home has to see it.
func TestConverge_SharesALocalSkillAcrossRuntimeHomes(t *testing.T) {
	home := t.TempDir()
	skill := filepath.Join(home, ".claude", "skills", "switch-test")
	if err := os.MkdirAll(skill, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skill, "SKILL.md"),
		[]byte("---\nname: switch-test\ndescription: Survives a harness switch.\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A different skill of the same name already in the Codex home stays put.
	taken := filepath.Join(home, ".codex", "skills", "switch-test")
	if err := os.MkdirAll(taken, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(taken, "SKILL.md"), []byte("---\nname: switch-test\ndescription: Codex's own.\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sidecar, reports := captureSidecar(t, http.StatusNoContent)
	t.Setenv("KYBER_SIDECAR_URL", sidecar.URL)
	p := paths{homeDir: home, platformDir: filepath.Join(t.TempDir(), "none")}

	if _, err := convergeAndReport(p, postReport); err != nil {
		t.Fatal(err)
	}
	hermes := filepath.Join(home, ".hermes", "skills", "switch-test")
	if !linkPointsAt(hermes, skill) {
		t.Fatalf("~/.hermes/skills/switch-test does not point at the Claude skill")
	}
	if li, err := os.Lstat(taken); err != nil || li.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("the Codex home's own skill was replaced: %v", err)
	}
	rep := (*reports)[0]
	if len(rep.Skills) != 1 || len(rep.Skills[0].Linked) != 3 {
		t.Fatalf("reported skills = %+v, want switch-test loadable in every runtime", rep.Skills)
	}

	// Deleting the skill drops the links made to it, without a dangling-link
	// warning; the freed name then goes to the Codex home's own skill.
	if err := os.RemoveAll(skill); err != nil {
		t.Fatal(err)
	}
	if _, err := convergeAndReport(p, postReport); err != nil {
		t.Fatal(err)
	}
	if !linkPointsAt(hermes, taken) {
		t.Fatalf("~/.hermes/skills/switch-test should now share the Codex skill")
	}
	if !linkPointsAt(filepath.Join(home, ".claude", "skills", "switch-test"), taken) {
		t.Fatalf("~/.claude/skills/switch-test should now share the Codex skill")
	}
	for _, issue := range (*reports)[1].Issues {
		if issue.Code == skillscan.IssueDanglingLink {
			t.Fatalf("stale cross-home link reported as dangling: %+v", issue)
		}
	}
}

// With an identity repo, a hand-written skill in a runtime home is already
// flagged unmanaged; sharing it with the other homes must not add a second
// warning per home.
func TestConverge_SharedUnmanagedSkillIsReportedOnce(t *testing.T) {
	f := newRepoFixture(t)
	sidecar, reports := captureSidecar(t, http.StatusNoContent)
	t.Setenv("KYBER_SIDECAR_URL", sidecar.URL)
	skill := filepath.Join(f.home, ".claude", "skills", "handmade")
	if err := os.MkdirAll(skill, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skill, "SKILL.md"), []byte("---\nname: handmade\ndescription: Not in the repo.\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := convergeAndReport(paths{repoDir: f.repoDir, homeDir: f.home}, postReport); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(f.home, ".hermes", "skills", "handmade")); err != nil {
		t.Fatalf("unmanaged skill not shared: %v", err)
	}
	var unmanaged int
	for _, issue := range (*reports)[0].Issues {
		if issue.Code == skillscan.IssueUnmanaged {
			unmanaged++
		}
	}
	if unmanaged != 1 {
		t.Fatalf("unmanaged issues = %d, want exactly the one for the real directory: %+v", unmanaged, (*reports)[0].Issues)
	}
}

// A shared link the agent deletes, typically from the harness it now runs,
// must stay deleted rather than reappear on the next pass. A skill created
// again later is shared again.
func TestConverge_RespectsAnAgentDeletingASharedSkill(t *testing.T) {
	home := t.TempDir()
	skill := filepath.Join(home, ".claude", "skills", "tidy")
	writeLocal := func() {
		if err := os.MkdirAll(skill, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(skill, "SKILL.md"), []byte("---\nname: tidy\ndescription: Local.\n---\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeLocal()
	sidecar, _ := captureSidecar(t, http.StatusNoContent)
	t.Setenv("KYBER_SIDECAR_URL", sidecar.URL)
	p := paths{homeDir: home, platformDir: filepath.Join(t.TempDir(), "none")}
	converge := func() {
		t.Helper()
		if _, err := convergeAndReport(p, postReport); err != nil {
			t.Fatal(err)
		}
	}
	codex := filepath.Join(home, ".codex", "skills", "tidy")

	converge()
	if !linkPointsAt(codex, skill) {
		t.Fatal("skill was not shared into the Codex home")
	}
	if err := os.Remove(codex); err != nil {
		t.Fatal(err)
	}
	converge()
	if _, err := os.Lstat(codex); !os.IsNotExist(err) {
		t.Fatalf("deleted shared skill came back: %v", err)
	}

	if err := os.RemoveAll(skill); err != nil {
		t.Fatal(err)
	}
	converge()
	writeLocal()
	converge()
	if !linkPointsAt(codex, skill) {
		t.Fatal("a skill created again was not shared again")
	}
}

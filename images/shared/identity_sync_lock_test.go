//go:build integration

package identityreposhared

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// syncFixture is an identity-repo clone parked on a feature branch, plus the
// generated sync script pointed at it. The sync's first job is to check out the
// default branch, so HEAD tells whether it got past a lock.
type syncFixture struct {
	home, repoDir, script string
	env                   []string
}

func newSyncFixture(t *testing.T) *syncFixture {
	t.Helper()
	home := t.TempDir()
	work := t.TempDir()
	// kyber-skills in the pod would post a report to the real sidecar; stub it.
	bin := filepath.Join(work, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "kyber-skills"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	env := []string{
		"HOME=" + home,
		"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"),
		"GIT_CONFIG_GLOBAL=" + filepath.Join(home, ".gitconfig-test"),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		// No pod token, so the sync never asks a control plane for one.
		"KYBER_POD_TOKEN_PATH=" + filepath.Join(work, "no-pod-token"),
	}
	f := &syncFixture{home: home, repoDir: filepath.Join(home, "dev", "agent-repo"), env: env}

	remote := filepath.Join(work, "remote.git")
	f.git(t, work, "init", "--bare", "-b", "main", remote)
	seed := filepath.Join(work, "seed")
	f.git(t, work, "clone", remote, seed)
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.git(t, seed, "add", "-A")
	f.git(t, seed, "commit", "-m", "seed")
	f.git(t, seed, "push", "origin", "main")
	f.git(t, work, "clone", remote, f.repoDir)
	f.git(t, f.repoDir, "checkout", "-q", "-b", "feat/parked")

	body := readFile(t, repoRoot(t), "images", "shared", "kyber-identity-repo.sh")
	const startMark = "<<'SYNC_BODY'\n"
	i := strings.Index(body, startMark)
	if i < 0 {
		t.Fatal("could not find the sync-body heredoc opener in the shared script")
	}
	rest := body[i+len(startMark):]
	j := strings.Index(rest, "\nSYNC_BODY")
	if j < 0 {
		t.Fatal("could not find the sync-body heredoc terminator")
	}
	header := "#!/bin/bash\nset -u\nREPO_DIR='" + f.repoDir + "'\nREPO_SLUG='matty-v/agent-repo'\nHOME_DIR='" + home + "'\n"
	f.script = filepath.Join(work, "kyber-sync-identity.sh")
	if err := os.WriteFile(f.script, []byte(header+rest[:j]+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *syncFixture) git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	c := exec.Command("git", args...)
	c.Dir = dir
	c.Env = f.env
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// writeLock plants an index.lock last touched age ago.
func (f *syncFixture) writeLock(t *testing.T, age time.Duration) string {
	t.Helper()
	lock := filepath.Join(f.repoDir, ".git", "index.lock")
	if err := os.WriteFile(lock, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	when := time.Now().Add(-age)
	if err := os.Chtimes(lock, when, when); err != nil {
		t.Fatal(err)
	}
	return lock
}

func (f *syncFixture) sync(t *testing.T) string {
	t.Helper()
	c := exec.Command("/bin/bash", f.script)
	c.Env = f.env
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("sync exited non-zero: %v\n%s", err, out)
	}
	return string(out)
}

// MAT-90 G12: a git killed mid-operation leaves index.lock behind, and every
// later sync failed its checkout and merge on it, so the agent silently ran a
// stale identity. An abandoned lock must be cleared, said so in the log, and
// the sync must then actually proceed.
func TestSync_RemovesAStaleIndexLockAndProceeds(t *testing.T) {
	f := newSyncFixture(t)
	lock := f.writeLock(t, 5*time.Minute)

	out := f.sync(t)
	if _, err := os.Stat(lock); !os.IsNotExist(err) {
		t.Fatalf("a 5-minute-old lock with no git running was left in place (stat err=%v)\n%s", err, out)
	}
	if !strings.Contains(out, "removed stale") {
		t.Errorf("removing the lock was not logged:\n%s", out)
	}
	if head := f.git(t, f.repoDir, "rev-parse", "--abbrev-ref", "HEAD"); head != "main" {
		t.Errorf("sync did not get past the lock: HEAD = %q, want main\n%s", head, out)
	}
}

// A lock taken seconds ago may belong to an operation still starting up.
func TestSync_KeepsAFreshIndexLock(t *testing.T) {
	f := newSyncFixture(t)
	lock := f.writeLock(t, 0)

	out := f.sync(t)
	if _, err := os.Stat(lock); err != nil {
		t.Fatalf("a fresh lock was removed: %v\n%s", err, out)
	}
}

// However old the lock, a live git in the repo may be the one holding it.
// Deleting it under that process would corrupt the index it is writing.
func TestSync_KeepsALockWhileAGitProcessIsRunningInTheRepo(t *testing.T) {
	f := newSyncFixture(t)
	lock := f.writeLock(t, 5*time.Minute)

	// cat-file --batch sits reading stdin: a real, live git with the repo as cwd.
	live := exec.Command("git", "-C", f.repoDir, "cat-file", "--batch")
	live.Env = f.env
	stdin, err := live.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := live.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stdin.Close(); _ = live.Wait() })

	out := f.sync(t)
	if _, err := os.Stat(lock); err != nil {
		t.Fatalf("a lock was removed while git was running in the repo: %v\n%s", err, out)
	}
	if !strings.Contains(out, "held by a running git process") {
		t.Errorf("keeping the lock was not explained:\n%s", out)
	}
}

// A git working on the repo from elsewhere (--git-dir, a trailing slash)
// also holds it; only matching the exact path as cwd or argument missed it.
func TestSync_KeepsALockForAGitNamingTheRepoIndirectly(t *testing.T) {
	f := newSyncFixture(t)
	lock := f.writeLock(t, 5*time.Minute)

	live := exec.Command("git", "--git-dir="+f.repoDir+"/.git", "cat-file", "--batch")
	live.Dir = t.TempDir()
	live.Env = f.env
	stdin, err := live.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := live.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stdin.Close(); _ = live.Wait() })

	out := f.sync(t)
	if _, err := os.Stat(lock); err != nil {
		t.Fatalf("a lock was removed while git --git-dir was working on the repo: %v\n%s", err, out)
	}
}

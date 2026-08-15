// Package agent_base_test — tests for the durable-root manager (kyber#78).
//
// The three-way merge in `scripts/kyber-rootfs` is the riskiest code in this
// change: it decides, on every base-image upgrade, whether to overwrite a file
// on an agent's root or leave it alone. Getting it wrong either clobbers months
// of an agent's work or silently pins it to a stale base image forever.
//
// These tests drive the real script against synthetic image trees, so they need
// no docker and run in the normal test job.
package agent_base_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// rootfsEnv is one synthetic world: an "image" root, a persist dir, and the
// agent's durable root inside it.
type rootfsEnv struct {
	t       *testing.T
	image   string
	persist string
	root    string
}

func newRootfsEnv(t *testing.T) *rootfsEnv {
	t.Helper()
	base := t.TempDir()
	e := &rootfsEnv{
		t:       t,
		image:   filepath.Join(base, "image"),
		persist: filepath.Join(base, "persist"),
	}
	e.root = filepath.Join(e.persist, "agentroot")
	// `usr` is the sentinel prepare() uses to decide "already seeded", so every
	// synthetic image needs one.
	mustMkdirAll(t, filepath.Join(e.image, "usr", "bin"))
	mustMkdirAll(t, filepath.Join(e.image, "etc"))
	mustMkdirAll(t, e.persist)
	return e
}

// prepare runs `kyber-rootfs prepare` and returns the mode it reported.
func (e *rootfsEnv) prepare() string {
	e.t.Helper()
	script := filepath.Join(rootfsRepoRoot(e.t), "images/agent-base/scripts/kyber-rootfs")
	cmd := exec.Command("bash", script, "prepare", e.root)
	cmd.Env = append(os.Environ(),
		"IMAGE_ROOT="+e.image,
		"PERSIST_DIR="+e.persist,
		"LEGACY_UPPER="+filepath.Join(e.persist, "overlay", "upper"),
	)
	out, err := cmd.Output()
	if err != nil {
		stderr := ""
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = string(ee.Stderr)
		}
		e.t.Fatalf("kyber-rootfs prepare: %v\n%s", err, stderr)
	}
	return strings.TrimSpace(string(out))
}

// writeImage writes a file into the synthetic base image with a fixed mtime, so
// "the image changed this path" is unambiguous rather than timing-dependent.
func (e *rootfsEnv) writeImage(rel, content string, mtime time.Time) {
	e.t.Helper()
	p := filepath.Join(e.image, rel)
	mustMkdirAll(e.t, filepath.Dir(p))
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		e.t.Fatalf("writing image file %s: %v", rel, err)
	}
	if err := os.Chtimes(p, mtime, mtime); err != nil {
		e.t.Fatalf("setting mtime on %s: %v", rel, err)
	}
}

func (e *rootfsEnv) removeImage(rel string) {
	e.t.Helper()
	if err := os.Remove(filepath.Join(e.image, rel)); err != nil {
		e.t.Fatalf("removing image file %s: %v", rel, err)
	}
}

// writeAgent simulates the agent editing its own root.
func (e *rootfsEnv) writeAgent(rel, content string) {
	e.t.Helper()
	p := filepath.Join(e.root, rel)
	mustMkdirAll(e.t, filepath.Dir(p))
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		e.t.Fatalf("writing agent file %s: %v", rel, err)
	}
	// A real edit lands with a current mtime; make that explicit rather than
	// relying on the test running slowly enough for it to differ.
	now := time.Now()
	if err := os.Chtimes(p, now, now); err != nil {
		e.t.Fatalf("setting mtime on %s: %v", rel, err)
	}
}

func (e *rootfsEnv) rootContent(rel string) (string, bool) {
	e.t.Helper()
	b, err := os.ReadFile(filepath.Join(e.root, rel))
	if err != nil {
		return "", false
	}
	return string(b), true
}

func (e *rootfsEnv) conflicts() string {
	e.t.Helper()
	b, err := os.ReadFile(filepath.Join(e.persist, "kyber", "rootfs-upgrade-conflicts.log"))
	if err != nil {
		return ""
	}
	return string(b)
}

// rootfsRepoRoot returns the repo root. Deliberately separate from the
// identically-shaped helper in home_persistence_test.go: that file is behind
// the docker_integration build tag, and these tests need no docker, so they
// must compile without it.
func rootfsRepoRoot(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatalf("git rev-parse --show-toplevel: %v", err)
	}
	return strings.TrimSpace(string(out))
}

func mustMkdirAll(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", p, err)
	}
}

func TestRootfs_SeedsOnFirstBoot(t *testing.T) {
	e := newRootfsEnv(t)
	v1 := time.Now().Add(-72 * time.Hour)
	e.writeImage("etc/motd", "image v1\n", v1)
	e.writeImage("usr/bin/tool", "#!/bin/sh\necho v1\n", v1)

	if mode := e.prepare(); mode != "rootfs-seeded" {
		t.Fatalf("first boot mode = %q, want rootfs-seeded", mode)
	}
	if got, ok := e.rootContent("etc/motd"); !ok || got != "image v1\n" {
		t.Errorf("etc/motd after seed = %q (present=%v), want image v1", got, ok)
	}

	// The bind targets the entrypoint mounts over must exist, or the chroot
	// assembly fails with nowhere to land.
	for _, dir := range []string{"proc", "sys", "dev", "persist", "tmp", "run"} {
		if fi, err := os.Stat(filepath.Join(e.root, dir)); err != nil || !fi.IsDir() {
			t.Errorf("mount target %s missing after seed", dir)
		}
	}
}

func TestRootfs_SecondBootIsANoOp(t *testing.T) {
	e := newRootfsEnv(t)
	v1 := time.Now().Add(-72 * time.Hour)
	e.writeImage("etc/motd", "image v1\n", v1)
	e.prepare()

	if mode := e.prepare(); mode != "rootfs" {
		t.Errorf("unchanged second boot mode = %q, want rootfs (a re-seed would mean state loss)", mode)
	}
}

// The core of the merge: Kyber may update what the agent has not claimed.
func TestRootfs_UpgradeTakesImageVersionWhenAgentDidNotTouchIt(t *testing.T) {
	e := newRootfsEnv(t)
	v1 := time.Now().Add(-72 * time.Hour)
	e.writeImage("etc/motd", "image v1\n", v1)
	e.prepare()

	v2 := time.Now().Add(-1 * time.Hour)
	e.writeImage("etc/motd", "image v2\n", v2)

	if mode := e.prepare(); mode != "rootfs-upgraded" {
		t.Fatalf("upgrade mode = %q, want rootfs-upgraded", mode)
	}
	got, _ := e.rootContent("etc/motd")
	if got != "image v2\n" {
		t.Errorf("etc/motd = %q, want the new image version — an untouched file must "+
			"receive base-image updates or agents freeze on a stale root", got)
	}
}

// The other half: Kyber never silently overwrites the agent's work.
func TestRootfs_UpgradeKeepsAgentVersionAndLogsTheConflict(t *testing.T) {
	e := newRootfsEnv(t)
	v1 := time.Now().Add(-72 * time.Hour)
	e.writeImage("etc/motd", "image v1\n", v1)
	e.prepare()

	e.writeAgent("etc/motd", "AGENT EDITED THIS\n")

	v2 := time.Now().Add(-1 * time.Hour)
	e.writeImage("etc/motd", "image v2\n", v2)
	e.prepare()

	got, _ := e.rootContent("etc/motd")
	if got != "AGENT EDITED THIS\n" {
		t.Errorf("etc/motd = %q, want the agent's edit preserved", got)
	}
	if !strings.Contains(e.conflicts(), "etc/motd") {
		t.Errorf("conflict log does not mention etc/motd; got %q", e.conflicts())
	}
}

func TestRootfs_UpgradeAddsNewImageFiles(t *testing.T) {
	e := newRootfsEnv(t)
	v1 := time.Now().Add(-72 * time.Hour)
	e.writeImage("etc/motd", "image v1\n", v1)
	e.prepare()

	e.writeImage("usr/bin/newtool", "#!/bin/sh\necho new\n", time.Now().Add(-time.Hour))
	e.prepare()

	if got, ok := e.rootContent("usr/bin/newtool"); !ok || !strings.Contains(got, "echo new") {
		t.Errorf("new image file not delivered to the durable root (present=%v, got %q)", ok, got)
	}
}

func TestRootfs_UpgradeRemovesFilesTheAgentNeverTouched(t *testing.T) {
	e := newRootfsEnv(t)
	v1 := time.Now().Add(-72 * time.Hour)
	e.writeImage("etc/motd", "image v1\n", v1)
	e.writeImage("etc/dropped", "going away\n", v1)
	e.prepare()

	e.removeImage("etc/dropped")
	e.prepare()

	if _, ok := e.rootContent("etc/dropped"); ok {
		t.Error("a file dropped by the base image and never touched by the agent should be removed")
	}
}

func TestRootfs_UpgradeKeepsRemovedFilesTheAgentModified(t *testing.T) {
	e := newRootfsEnv(t)
	v1 := time.Now().Add(-72 * time.Hour)
	e.writeImage("etc/motd", "image v1\n", v1)
	e.writeImage("etc/dropped", "going away\n", v1)
	e.prepare()

	e.writeAgent("etc/dropped", "agent depends on this\n")
	e.removeImage("etc/dropped")
	e.prepare()

	got, ok := e.rootContent("etc/dropped")
	if !ok || got != "agent depends on this\n" {
		t.Errorf("etc/dropped = %q (present=%v), want the agent's copy kept even though "+
			"the image dropped it", got, ok)
	}
	if !strings.Contains(e.conflicts(), "etc/dropped") {
		t.Error("keeping a removed-but-modified file should be recorded as a conflict")
	}
}

// Anything the agent created that the image never shipped is simply its own.
func TestRootfs_UpgradePreservesAgentCreatedFiles(t *testing.T) {
	e := newRootfsEnv(t)
	v1 := time.Now().Add(-72 * time.Hour)
	e.writeImage("etc/motd", "image v1\n", v1)
	e.prepare()

	e.writeAgent("usr/local/bin/mytool", "#!/bin/sh\necho mine\n")
	e.writeAgent("etc/myconfig", "mine\n")

	e.writeImage("etc/motd", "image v2\n", time.Now().Add(-time.Hour))
	e.prepare()

	for _, rel := range []string{"usr/local/bin/mytool", "etc/myconfig"} {
		if _, ok := e.rootContent(rel); !ok {
			t.Errorf("agent-created file %s was destroyed by a base-image upgrade", rel)
		}
	}
}

// An agent arriving from the overlay era keeps everything it ever changed, and
// its upper layer is left intact so the rollback path stays open.
func TestRootfs_MigratesLegacyOverlayUpperLayer(t *testing.T) {
	e := newRootfsEnv(t)
	v1 := time.Now().Add(-72 * time.Hour)
	e.writeImage("etc/motd", "image v1\n", v1)
	e.writeImage("usr/bin/tool", "image tool\n", v1)

	upper := filepath.Join(e.persist, "overlay", "upper")
	mustMkdirAll(t, filepath.Join(upper, "etc"))
	mustMkdirAll(t, filepath.Join(upper, "usr", "local", "bin"))
	if err := os.WriteFile(filepath.Join(upper, "etc", "motd"), []byte("overlay edit\n"), 0o644); err != nil {
		t.Fatalf("seeding upper: %v", err)
	}
	if err := os.WriteFile(filepath.Join(upper, "usr", "local", "bin", "legacy"), []byte("legacy\n"), 0o755); err != nil {
		t.Fatalf("seeding upper: %v", err)
	}

	if mode := e.prepare(); mode != "rootfs-migrated" {
		t.Fatalf("migration mode = %q, want rootfs-migrated", mode)
	}

	if got, _ := e.rootContent("etc/motd"); got != "overlay edit\n" {
		t.Errorf("etc/motd = %q, want the overlay upper layer's version to win over the image", got)
	}
	if _, ok := e.rootContent("usr/local/bin/legacy"); !ok {
		t.Error("a file that existed only in the overlay upper layer was lost in migration")
	}
	if got, _ := e.rootContent("usr/bin/tool"); got != "image tool\n" {
		t.Errorf("usr/bin/tool = %q, want the image version for a path the upper layer never had", got)
	}

	// Rollback must stay a flag flip, not a restore.
	if _, err := os.Stat(filepath.Join(upper, "etc", "motd")); err != nil {
		t.Error("migration destroyed the legacy overlay upper layer — rollback is no longer possible")
	}
}

// TestRootfs_SeedsWhenEtcMtabIsASymlink is a regression test for the bug that
// took down the first dev-env boot of this design.
//
// The base image ships /etc/mtab as a symlink to /proc/self/mounts. The seed
// step copied the symlink faithfully, and the follow-up that ensures bind
// targets exist then tested it with `-e`, which follows the link — into a
// /proc that is empty at that point. The test failed, the code tried to create
// the file, the redirect followed the symlink, and the whole boot died.
//
// Nothing was lost, because the fail-closed guard turned it into a refusal to
// start rather than an agent silently running on an ephemeral root. But a
// dangling symlink in the image is ordinary, and it must seed cleanly.
func TestRootfs_SeedsWhenEtcMtabIsASymlink(t *testing.T) {
	e := newRootfsEnv(t)
	v1 := time.Now().Add(-72 * time.Hour)
	e.writeImage("etc/motd", "image v1\n", v1)

	// Exactly the shape the real image has — verified against
	// kyber-claude-code: `/etc/mtab -> ../proc/self/mounts`.
	//
	// RELATIVE is the whole point. An absolute /proc/self/mounts resolves to
	// the host's real procfs during the test and the bug does not reproduce;
	// the relative form resolves inside the seeded root, where /proc is an
	// empty directory, and the redirect fails with ENOENT. The first version
	// of this test used the absolute form and passed against the broken code.
	if err := os.Symlink("../proc/self/mounts", filepath.Join(e.image, "etc", "mtab")); err != nil {
		t.Fatalf("creating the mtab symlink: %v", err)
	}

	if mode := e.prepare(); mode != "rootfs-seeded" {
		t.Fatalf("seed mode = %q, want rootfs-seeded", mode)
	}

	// The symlink must survive as a symlink, not be replaced by an empty file.
	fi, err := os.Lstat(filepath.Join(e.root, "etc", "mtab"))
	if err != nil {
		t.Fatalf("etc/mtab missing from the seeded root: %v", err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Error("etc/mtab was replaced by a regular file; the image's symlink should be preserved")
	}

	// And a second boot must still be a no-op rather than tripping over it.
	if mode := e.prepare(); mode != "rootfs" {
		t.Errorf("second boot mode = %q, want rootfs", mode)
	}
}

// TestRootfs_UnrelatedDirectoryWritesDoNotLookLikeAnUpgrade is a regression
// test for the second bug the dev-env run surfaced.
//
// A directory's mtime changes whenever anything is created inside it. The
// container root `.` therefore differed between two otherwise identical pods —
// the entrypoint creates /merged before the manifest is taken — and that single
// line was enough to make `cmp` disagree, so EVERY boot ran a full three-way
// merge and logged spurious conflicts on `.` and `./kyber`.
//
// A directory mtime says nothing about whether the base image changed, so the
// manifest must not record one.
func TestRootfs_UnrelatedDirectoryWritesDoNotLookLikeAnUpgrade(t *testing.T) {
	e := newRootfsEnv(t)
	v1 := time.Now().Add(-72 * time.Hour)
	e.writeImage("etc/motd", "image v1\n", v1)
	e.prepare()

	// Touch the image root the way a runtime does — a new directory at the top
	// level, no change to any file the image ships.
	mustMkdirAll(t, filepath.Join(e.image, "merged"))
	now := time.Now()
	if err := os.Chtimes(e.image, now, now); err != nil {
		t.Fatalf("bumping the image root mtime: %v", err)
	}

	if mode := e.prepare(); mode != "rootfs" {
		t.Errorf("mode = %q, want rootfs: a directory mtime change is not a base-image "+
			"change, and treating it as one runs a full merge on every boot", mode)
	}
}

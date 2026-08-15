//go:build docker_integration

// Integration coverage for kyber#684: the root-run gnome-keyring daemon must
// not leave ~/.local and ~/.cache owned by root, because the agent user then
// cannot create ~/.local/bin and — under `set -euo pipefail` in both runtime
// start scripts — the boot dies and the agent never reaches Running.
//
// Same build tag and docker requirement as home_persistence_test.go; runs in
// the dedicated agent-base-integration job. Invoke locally with
// `go test -tags docker_integration ./images/agent-base/...`.
package agent_base_test

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// runAgentBase boots the agent-base image against a fresh /persist volume and
// returns the combined output of the supplied in-container command. Mirrors the
// docker invocation used by the home-persistence tests.
func runAgentBase(t *testing.T, persistDir, script string) (string, error) {
	t.Helper()
	cmd := dockerRun(dockerSandboxEnv(),
		"--privileged",
		"-v", persistDir+":/persist",
		testImageTag,
		"/bin/sh", "-c", script,
	)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// newPersistDir makes a world-writable temp dir to mount at /persist.
func newPersistDir(t *testing.T, pattern string) string {
	t.Helper()
	dir, err := os.MkdirTemp("", pattern)
	if err != nil {
		t.Fatalf("mkdir tmp: %v", err)
	}
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatalf("chmod persist dir: %v", err)
	}
	t.Cleanup(func() {
		exec.Command("sudo", "rm", "-rf", dir).Run() //nolint:errcheck
		os.RemoveAll(dir)                            //nolint:errcheck
	})
	return dir
}

// TestHomeDotDirs_AgentUserCanCreateLocalBin is the regression test for the
// actual boot failure: as the agent user, `mkdir -p $HOME/.local/bin` must
// succeed after the entrypoint has started the keyring as root. This is the
// exact call the shared identity-repo helper makes
// (images/shared/kyber-identity-repo.sh:75), and the exact one that failed on
// `echo` with "cannot create directory '/home/kyber/.local': Permission denied".
func TestHomeDotDirs_AgentUserCanCreateLocalBin(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}
	buildAgentBaseImage(t)
	persistDir := newPersistDir(t, "kyber-dotdir-mkdir-*")

	// The entrypoint has already dropped to the agent user by the time it execs
	// this command, so run the mkdir directly — an `su kyber` from kyber would
	// just prompt for a password and fail. Same shell options the runtime start
	// scripts use, so a failure aborts exactly as it would live.
	out, err := runAgentBase(t, persistDir,
		`set -eu; echo "running as $(id -un)"; mkdir -p "$HOME/.local/bin" && echo LOCALBIN_OK`)
	if err != nil {
		t.Fatalf("boot failed: %v\n%s", err, out)
	}
	t.Logf("output:\n%s", out)
	if !strings.Contains(out, "LOCALBIN_OK") {
		t.Errorf("agent user could not create ~/.local/bin — kyber#684 regression; output:\n%s", out)
	}
}

// TestHomeDotDirs_NotRootOwnedAfterBoot asserts the underlying property rather
// than the symptom: after a boot that starts the keyring, neither ~/.local nor
// ~/.cache is owned by root.
func TestHomeDotDirs_NotRootOwnedAfterBoot(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}
	buildAgentBaseImage(t)
	persistDir := newPersistDir(t, "kyber-dotdir-owner-*")

	out, err := runAgentBase(t, persistDir,
		`stat -c '%n owner=%U' /home/kyber/.local /home/kyber/.cache 2>/dev/null || echo NO_DOTDIRS`)
	if err != nil {
		t.Fatalf("boot failed: %v\n%s", err, out)
	}
	t.Logf("output:\n%s", out)

	// If the keyring never ran in this environment the dirs may not exist at
	// all; that is not the regression under test.
	if strings.Contains(out, "NO_DOTDIRS") {
		t.Skip("keyring did not create dotdirs in this environment")
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "owner=root") {
			t.Errorf("dotdir is root-owned after boot — kyber#684 regression: %q", strings.TrimSpace(line))
		}
	}
}

// TestHomeDotDirs_SelfHealsAlreadyWedgedPersist covers the migration case: a
// PVC that already has root-owned dotdirs from the pre-fix image must recover
// on the next boot without a manual chown, since every agent created before
// this fix is in that state.
func TestHomeDotDirs_SelfHealsAlreadyWedgedPersist(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}
	buildAgentBaseImage(t)
	persistDir := newPersistDir(t, "kyber-dotdir-selfheal-*")

	// Boot 1: wedge the persisted home. The command runs as the agent user, so
	// root ownership cannot be simulated from here — instead make ~/.local
	// unwritable, which is the property that actually killed the boot
	// ("cannot create directory '/home/kyber/.local': Permission denied"). The
	// ownership half is covered by TestHomeDotDirs_NotRootOwnedAfterBoot.
	out1, err := runAgentBase(t, persistDir,
		`set -eu; mkdir -p "$HOME/.local/share/keyrings" "$HOME/.cache/keyring-X"; chmod 0500 "$HOME/.local"; echo WEDGED`)
	if err != nil {
		t.Fatalf("boot 1: %v\n%s", err, out1)
	}
	if !strings.Contains(out1, "WEDGED") {
		t.Fatalf("could not set up the wedged state; output:\n%s", out1)
	}

	// Boot 2: same volume. The entrypoint repair must run before the agent user
	// needs the directory.
	out2, err := runAgentBase(t, persistDir,
		`set -eu; mkdir -p "$HOME/.local/bin" && echo SELFHEAL_OK`)
	if err != nil {
		t.Fatalf("boot 2: %v\n%s", err, out2)
	}
	t.Logf("boot 2 output:\n%s", out2)
	if !strings.Contains(out2, "SELFHEAL_OK") {
		t.Errorf("a PVC wedged by the pre-fix image did not self-heal — every pre-existing agent stays broken; output:\n%s", out2)
	}
}

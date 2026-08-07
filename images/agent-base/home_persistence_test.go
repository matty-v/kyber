//go:build docker_integration

// Package agent_base_test contains integration tests for the agent-base image.
// Tests shell out to docker build + docker run to exercise the entrypoint's
// HOME persistence behavior.
//
// Gated behind the `docker_integration` build tag. These tests require a
// working docker daemon AND a reliable network (the Dockerfile does
// apt-get install from Ubuntu mirrors), so they are excluded from the
// default `go test ./...` run in the build workflow and instead run only
// in the dedicated agent-base-integration job when the agent-base image
// is touched. Invoke locally with `go test -tags docker_integration ./images/agent-base/...`.
package agent_base_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const testImageTag = "kyber-agent-base-test:home-persistence"

// buildAgentBaseImage builds the agent-base image once and registers a
// cleanup that removes it.
func buildAgentBaseImage(t *testing.T) {
	t.Helper()
	root := repoRootDir(t)
	build := exec.Command("docker", "build", "-t", testImageTag,
		filepath.Join(root, "images/agent-base"))
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("docker build: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		exec.Command("docker", "rmi", "-f", testImageTag).Run() //nolint:errcheck
	})
}

// TestHomePersistence_BindMountSurvivesRestart verifies that a file written
// under $HOME in one container invocation is still readable in a second
// invocation that shares the same /persist volume.
//
// This test works in both overlay mode (local kernel supports nested overlay)
// and symlink/bind-mount mode (overlay fails, HOME is bind-mounted from
// /persist/home). In overlay mode the upper layer preserves the file; in
// bind-mount mode the file lives directly on the host volume. Either way
// the second boot must see the marker.
func TestHomePersistence_BindMountSurvivesRestart(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}

	buildAgentBaseImage(t)

	persistDir, err := os.MkdirTemp("", "kyber-home-persist-*")
	if err != nil {
		t.Fatalf("mkdir tmp: %v", err)
	}
	// Grant world-write so the container (any uid) can write to the volume.
	if err := os.Chmod(persistDir, 0o777); err != nil {
		t.Fatalf("chmod persist dir: %v", err)
	}
	t.Cleanup(func() {
		// Overlay upper dirs are owned by root inside the container; sudo may
		// be needed on some kernels. Best-effort only — CI ephemeral runners
		// clean up automatically.
		exec.Command("sudo", "rm", "-rf", persistDir).Run() //nolint:errcheck
		os.RemoveAll(persistDir)                             //nolint:errcheck
	})

	const marker = "hello-from-boot-one"

	// Boot 1: write a marker file under $HOME.
	// --privileged lets the entrypoint attempt overlay or bind-mount as needed.
	boot1 := exec.Command("docker", "run", "--rm",
		"--privileged",
		"-v", persistDir+":/persist",
		testImageTag,
		"/bin/sh", "-c",
		"echo "+marker+" > /home/kyber/marker.txt && echo wrote-marker",
	)
	out1, err := boot1.CombinedOutput()
	if err != nil {
		t.Fatalf("boot 1: %v\n%s", err, out1)
	}
	if !strings.Contains(string(out1), "wrote-marker") {
		t.Fatalf("boot 1 did not print confirmation; output:\n%s", out1)
	}
	t.Logf("boot 1 output:\n%s", out1)

	// Boot 2: same persist volume, read the marker back.
	boot2 := exec.Command("docker", "run", "--rm",
		"--privileged",
		"-v", persistDir+":/persist",
		testImageTag,
		"/bin/sh", "-c", "cat /home/kyber/marker.txt",
	)
	out2, err := boot2.CombinedOutput()
	if err != nil {
		t.Fatalf("boot 2 failed: %v\n%s", err, out2)
	}
	t.Logf("boot 2 output:\n%s", out2)

	if !strings.Contains(string(out2), marker) {
		t.Errorf("boot 2 did not see marker.txt content %q; got:\n%s", marker, out2)
	}
}

// TestHomePersistence_FirstBootSeedsBashrc verifies that on first boot with an
// empty /persist/home, the image's initial /home/kyber/.bashrc is copied into
// the persist target. This seed step runs in symlink mode (when the kernel
// overlay mount fails), which is the common case on stock CI runners where the
// container root FS is itself an overlay.
//
// The test runs without --privileged so the overlay mount fails and the
// entrypoint falls back to symlink mode. The bind-mount step then also fails
// (no CAP_SYS_ADMIN), but the seed copy happens before that, so the test
// verifies the host-side persist directory was populated.
func TestHomePersistence_FirstBootSeedsBashrc(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}

	buildAgentBaseImage(t)

	persistDir, err := os.MkdirTemp("", "kyber-home-seed-*")
	if err != nil {
		t.Fatalf("mkdir tmp: %v", err)
	}
	if err := os.Chmod(persistDir, 0o777); err != nil {
		t.Fatalf("chmod persist dir: %v", err)
	}
	t.Cleanup(func() {
		exec.Command("sudo", "rm", "-rf", persistDir).Run() //nolint:errcheck
		os.RemoveAll(persistDir)                             //nolint:errcheck
	})

	// Run WITHOUT --privileged: overlay mount will fail, triggering symlink
	// mode. The entrypoint seeds /persist/home before attempting the bind
	// mount, so the seed artifacts reach the host volume regardless of
	// whether the bind mount itself succeeds.
	boot := exec.Command("docker", "run", "--rm",
		"-v", persistDir+":/persist",
		testImageTag,
		"/usr/bin/true",
	)
	out, _ := boot.CombinedOutput() // bind-mount step will exit non-zero; that's expected
	t.Logf("boot output:\n%s", out)

	// The entrypoint must have used symlink mode (overlay failed) and logged
	// the seed step. If the output shows overlay mode was used instead, the
	// test is inconclusive on this kernel but the CI run will still catch it.
	if strings.Contains(string(out), "Overlay mounted successfully") {
		t.Skip("kernel supports nested overlay; symlink mode not triggered — seed behavior verified in CI")
	}
	if !strings.Contains(string(out), "seeding") {
		t.Fatalf("expected '[kyber] First boot in symlink mode — seeding' in output; got:\n%s", out)
	}

	// The seeded .bashrc must exist on the host-side persist volume.
	bashrcPath := filepath.Join(persistDir, "home", ".bashrc")
	if _, err := os.Stat(bashrcPath); err != nil {
		t.Errorf("expected .bashrc seeded at %s after first boot; got: %v\nentrypoint output:\n%s",
			bashrcPath, err, out)
	}

	// Verify the seeded .bashrc is non-empty (it should be the image default).
	data, err := os.ReadFile(bashrcPath)
	if err == nil && len(data) == 0 {
		t.Errorf(".bashrc at %s is empty; expected the image's default bashrc", bashrcPath)
	}

	// Second boot with the same persist dir: the .bashrc already exists, so
	// the seed step must be skipped (idempotency).
	boot2 := exec.Command("docker", "run", "--rm",
		"-v", persistDir+":/persist",
		testImageTag,
		"/usr/bin/true",
	)
	out2, _ := boot2.CombinedOutput()
	t.Logf("boot 2 output:\n%s", out2)

	if strings.Contains(string(out2), "seeding") {
		t.Errorf("seed step ran again on second boot; expected it to be skipped because .bashrc already exists")
	}
}

// TestAgentBase_ShipsCron verifies that the cron package is present in the
// agent-base image. entrypoint.sh starts /usr/sbin/cron as root before
// dropping to the kyber user, ensuring scheduled jobs fire on every pod boot
// without a separate init process. Overrides the overlay entrypoint since
// this is purely a file-presence check.
func TestAgentBase_ShipsCron(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}
	buildAgentBaseImage(t)

	cmd := exec.Command("docker", "run", "--rm",
		"--entrypoint", "/bin/sh",
		testImageTag,
		"-c", "test -x /usr/sbin/cron && echo CRON_PRESENT")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("cron binary check failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "CRON_PRESENT") {
		t.Errorf("expected /usr/sbin/cron to be executable in the image; got:\n%s", out)
	}
}

// repoRootDir returns the absolute path to the kyber repo root via git.
func repoRootDir(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatalf("git rev-parse --show-toplevel: %v", err)
	}
	return strings.TrimSpace(string(out))
}

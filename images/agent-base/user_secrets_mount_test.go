//go:build docker_integration

package agent_base_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestUserSecrets_BindMountVisibleInChroot is the regression test for kyber#514:
// a file-mode user-secret mounted by kubelet at /user-secrets must be visible to
// the agent (which runs inside the /merged overlay chroot). Before the fix,
// entrypoint.sh bound /persist, /secrets, /kyber/jobs-src and the identity token
// into /merged but NOT /user-secrets, so the agent saw only the empty boot-time
// overlay snapshot — the minted FALCON_ISSUE_TOKEN never reached the pod.
//
// We simulate kubelet's mount with `-v <hostdir>:/user-secrets` and assert the
// agent command (run by the entrypoint INSIDE the chroot) can read the file and
// that /user-secrets is a live mountpoint. Without the bind, in overlay mode the
// read fails — so this test is non-vacuous on the unpatched entrypoint.
func TestUserSecrets_BindMountVisibleInChroot(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}
	buildAgentBaseImage(t)

	persistDir, err := os.MkdirTemp("", "kyber-us-persist-*")
	if err != nil {
		t.Fatalf("mkdir persist: %v", err)
	}
	_ = os.Chmod(persistDir, 0o777)
	usDir, err := os.MkdirTemp("", "kyber-user-secrets-*")
	if err != nil {
		t.Fatalf("mkdir user-secrets: %v", err)
	}
	_ = os.Chmod(usDir, 0o777)
	t.Cleanup(func() {
		exec.Command("sudo", "rm", "-rf", persistDir).Run() //nolint:errcheck
		os.RemoveAll(persistDir)                             //nolint:errcheck
		os.RemoveAll(usDir)                                  //nolint:errcheck
	})

	const tokenContent = "ghs_falcon_issue_token_514"
	if err := os.WriteFile(filepath.Join(usDir, "falcon_issue_token.bin"), []byte(tokenContent), 0o644); err != nil {
		t.Fatalf("write token file: %v", err)
	}

	// The agent command (forwarded by the entrypoint as "$@" and exec'd inside the
	// chroot) checks the mount + reads the secret.
	agentCmd := "mountpoint -q /user-secrets && echo USER_SECRETS_IS_MOUNT; cat /user-secrets/falcon_issue_token.bin"
	run := dockerRun(dockerSandboxEnv(),
		"--privileged", // lets the entrypoint assemble the chroot and bind-mount
		"-v", persistDir+":/persist",
		"-v", usDir+":/user-secrets",
		testImageTag,
		"/bin/sh", "-c", agentCmd,
	)
	out, err := run.CombinedOutput()
	if err != nil {
		t.Fatalf("docker run: %v\n%s", err, out)
	}
	t.Logf("output:\n%s", out)

	if !strings.Contains(string(out), tokenContent) {
		t.Errorf("agent could not read the file-mode user-secret at /user-secrets/falcon_issue_token.bin (kyber#514 regression); got:\n%s", out)
	}
	// In overlay mode the bind makes /user-secrets a mountpoint inside the chroot.
	// (In bind-mount-home fallback mode the agent runs on the real root where the
	// docker -v mount is already live, so the content check above still holds.)
	if strings.Contains(string(out), "Overlay mounted successfully") && !strings.Contains(string(out), "USER_SECRETS_IS_MOUNT") {
		t.Errorf("overlay mode: /user-secrets is not a live mountpoint inside the chroot (the #514 bind is missing); got:\n%s", out)
	}
}

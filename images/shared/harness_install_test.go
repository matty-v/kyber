//go:build integration

package identityreposhared

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func harnessInstaller(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(wd, "kyber-harness-install")
}

func writeExecutable(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/usr/bin/env bash\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func fakeHarnessEnv(t *testing.T) (root, prefix, npm string) {
	t.Helper()
	dir := t.TempDir()
	root = filepath.Join(dir, "lib", "node_modules")
	prefix = dir
	npm = filepath.Join(dir, "npm")
	writeExecutable(t, npm, `
case "${1:-}" in
  root) echo "$FAKE_GLOBAL_ROOT"; exit 0 ;;
  prefix) echo "$FAKE_GLOBAL_PREFIX"; exit 0 ;;
esac
stage=""
while [ "$#" -gt 0 ]; do
  if [ "$1" = "--prefix" ]; then stage="$2"; shift 2; continue; fi
  shift
done
package="$stage/lib/node_modules/@example/harness"
mkdir -p "$package/bin" "$stage/bin"
printf '%s' "$FAKE_OUTPUT_VERSION" > "$package/VERSION"
printf '%s\n' '{"bin":{"harness":"bin/harness"}}' > "$package/package.json"
cat > "$package/bin/harness" <<'EOF'
#!/usr/bin/env bash
count=0
if [ -n "${FAKE_COUNT_FILE:-}" ]; then
  count=$(cat "$FAKE_COUNT_FILE" 2>/dev/null || echo 0)
  count=$((count + 1))
  printf '%s' "$count" > "$FAKE_COUNT_FILE"
fi
if [ -n "${FAKE_SLEEP_ON:-}" ] && [ "$count" = "$FAKE_SLEEP_ON" ]; then sleep 30; fi
script_dir=$(dirname "$(readlink -f "$0")")
echo "harness $(cat "$script_dir/../VERSION")"
EOF
chmod 0755 "$package/bin/harness"
ln -s ../lib/node_modules/@example/harness/bin/harness "$stage/bin/harness"
`)
	return root, prefix, npm
}

func runInstaller(t *testing.T, root, prefix, npm, expected, observed string, extra ...string) ([]byte, error) {
	t.Helper()
	cmd := exec.Command(harnessInstaller(t), "@example/harness", expected, "harness")
	cmd.Env = append(os.Environ(),
		"KYBER_HARNESS_NPM="+npm,
		"KYBER_HARNESS_GLOBAL_ROOT="+root,
		"KYBER_HARNESS_GLOBAL_PREFIX="+prefix,
		"FAKE_GLOBAL_ROOT="+root,
		"FAKE_GLOBAL_PREFIX="+prefix,
		"FAKE_OUTPUT_VERSION="+observed,
	)
	cmd.Env = append(cmd.Env, extra...)
	return cmd.CombinedOutput()
}

func liveBinary(root string) string {
	return filepath.Join(root, "@example", "harness", "bin", "harness")
}

func seedHarness(t *testing.T, path, version string) {
	t.Helper()
	writeExecutable(t, path, fmt.Sprintf("echo 'harness %s'", version))
	manifest := filepath.Join(filepath.Dir(filepath.Dir(path)), "package.json")
	if err := os.WriteFile(manifest, []byte(`{"bin":{"harness":"bin/harness"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestHarnessInstallerActivatesVerifiedStage(t *testing.T) {
	root, prefix, npm := fakeHarnessEnv(t)
	seedHarness(t, liveBinary(root), "1.0.0")
	out, err := runInstaller(t, root, prefix, npm, "2.0.0", "2.0.0")
	if err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	got, err := exec.Command(liveBinary(root), "--version").Output()
	if err != nil || !strings.Contains(string(got), "2.0.0") {
		t.Fatalf("live version = %q, err=%v", got, err)
	}
	link, err := os.Readlink(filepath.Join(prefix, "bin", "harness"))
	if err != nil || !strings.Contains(link, "@example/harness/bin/harness") {
		t.Fatalf("global bin link = %q, err=%v", link, err)
	}
}

func TestHarnessInstallerFailedVerificationPreservesLive(t *testing.T) {
	root, prefix, npm := fakeHarnessEnv(t)
	seedHarness(t, liveBinary(root), "1.0.0")
	out, err := runInstaller(t, root, prefix, npm, "2.0.0", "9.9.9")
	if err == nil || !strings.Contains(string(out), "reports 9.9.9, expected 2.0.0") {
		t.Fatalf("verification should fail: err=%v\n%s", err, out)
	}
	got, _ := exec.Command(liveBinary(root), "--version").Output()
	if !strings.Contains(string(got), "1.0.0") {
		t.Fatalf("failed install replaced live harness: %q", got)
	}
}

func TestHarnessInstallerRecoversInterruptedSwapAndStaleArtifacts(t *testing.T) {
	root, prefix, npm := fakeHarnessEnv(t)
	parent := filepath.Join(root, "@example")
	backup := filepath.Join(parent, ".harness.kyber-backup", "bin", "harness")
	seedHarness(t, backup, "1.0.0")
	seedHarness(t, liveBinary(root), "broken")
	for _, stale := range []string{".harness.kyber-new.123", ".harness-DEADBEEF"} {
		if err := os.MkdirAll(filepath.Join(parent, stale), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	out, err := runInstaller(t, root, prefix, npm, "2.0.0", "2.0.0")
	if err != nil {
		t.Fatalf("recovery install: %v\n%s", err, out)
	}
	for _, stale := range []string{".harness.kyber-new.123", ".harness-DEADBEEF", ".harness.kyber-backup"} {
		if _, err := os.Stat(filepath.Join(parent, stale)); !os.IsNotExist(err) {
			t.Errorf("stale artifact %s remains: %v", stale, err)
		}
	}
}

func TestHarnessInstallerRecoversOfflineBeforeNpm(t *testing.T) {
	root, prefix, npm := fakeHarnessEnv(t)
	parent := filepath.Join(root, "@example")
	backup := filepath.Join(parent, ".harness.kyber-backup", "bin", "harness")
	seedHarness(t, backup, "1.0.0")
	seedHarness(t, liveBinary(root), "broken")
	if err := os.Remove(filepath.Join(filepath.Dir(filepath.Dir(liveBinary(root))), "package.json")); err != nil {
		t.Fatal(err)
	}
	failedNPM := filepath.Join(filepath.Dir(npm), "failed-npm")
	writeExecutable(t, failedNPM, "exit 99")
	out, err := runInstaller(t, root, prefix, failedNPM, "1.0.0", "unused")
	if err != nil {
		t.Fatalf("offline recovery: %v\n%s", err, out)
	}
	got, err := exec.Command(filepath.Join(prefix, "bin", "harness"), "--version").Output()
	if err != nil || !strings.Contains(string(got), "1.0.0") {
		t.Fatalf("recovered command = %q, err=%v", got, err)
	}
}

func TestHarnessInstallerCompletesKilledActivationOffline(t *testing.T) {
	root, prefix, npm := fakeHarnessEnv(t)
	parent := filepath.Join(root, "@example")
	seedHarness(t, filepath.Join(parent, ".harness.kyber-backup", "bin", "harness"), "1.0.0")
	seedHarness(t, liveBinary(root), "2.0.0")
	failedNPM := filepath.Join(filepath.Dir(npm), "failed-npm")
	writeExecutable(t, failedNPM, "exit 99")
	out, err := runInstaller(t, root, prefix, failedNPM, "2.0.0", "unused")
	if err != nil {
		t.Fatalf("offline completion: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(parent, ".harness.kyber-backup")); !os.IsNotExist(err) {
		t.Fatalf("backup remains after completed activation: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(prefix, "bin", "harness")); err != nil {
		t.Fatalf("global command link was not repaired: %v", err)
	}
}

func TestHarnessInstallerSignalDuringActivationRollsBack(t *testing.T) {
	root, prefix, npm := fakeHarnessEnv(t)
	seedHarness(t, liveBinary(root), "1.0.0")
	cmd := exec.Command(harnessInstaller(t), "@example/harness", "2.0.0", "harness")
	cmd.Env = append(os.Environ(),
		"KYBER_HARNESS_NPM="+npm,
		"KYBER_HARNESS_GLOBAL_ROOT="+root,
		"KYBER_HARNESS_GLOBAL_PREFIX="+prefix,
		"FAKE_GLOBAL_ROOT="+root,
		"FAKE_GLOBAL_PREFIX="+prefix,
		"FAKE_OUTPUT_VERSION=2.0.0",
		"KYBER_HARNESS_TEST_SIGNAL_AFTER_SWAP=1",
	)
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("injected signal should fail the install:\n%s", out)
	}
	got, err := exec.Command(liveBinary(root), "--version").Output()
	if err != nil || !strings.Contains(string(got), "1.0.0") {
		t.Fatalf("signal did not restore previous harness: %q, err=%v", got, err)
	}
}

//go:build integration

package identityreposhared

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func runtimeRepairScript(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(wd, "kyber-runtime-repair")
}

func TestRuntimeRepairInstallsAndVerifiesConfiguredHarness(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "agentroot")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	installer := filepath.Join(dir, "kyber-harness-install")
	writeExecutable(t, installer, "exit 0")
	logPath := filepath.Join(dir, "chroot.log")
	writeExecutable(t, filepath.Join(dir, "chroot"), `
printf '%s\n' "$*" >> "$CHROOT_LOG"
if [ "$2" = /usr/bin/env ]; then
  mkdir -p "$1/usr/lib/node_modules/@openai/codex/bin" "$1/usr/bin"
  printf '%s\n' '{"version":"0.150.1","bin":{"codex":"bin/codex"}}' > "$1/usr/lib/node_modules/@openai/codex/package.json"
  cat > "$1/usr/lib/node_modules/@openai/codex/bin/codex" <<'EOF'
#!/usr/bin/env bash
echo 'codex-cli 0.150.1'
EOF
  chmod 0755 "$1/usr/lib/node_modules/@openai/codex/bin/codex"
  ln -sf ../lib/node_modules/@openai/codex/bin/codex "$1/usr/bin/codex"
fi
`)

	cmd := exec.Command(runtimeRepairScript(t), root, "@openai/codex", "codex", "0.150.1", "/usr/lib/node_modules/@openai/codex", "/usr/bin/codex")
	cmd.Env = append(os.Environ(),
		"PATH="+dir+":"+os.Getenv("PATH"),
		"CHROOT_LOG="+logPath,
		"KYBER_REPAIR_TEST_ROOT="+root,
		"KYBER_REPAIR_INSTALLER_SOURCE="+installer,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("repair failed: %v\n%s", err, out)
	}
	logBody, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	logText := string(logBody)
	if !strings.Contains(logText, "/usr/local/bin/kyber-harness-install @openai/codex 0.150.1 codex") ||
		!strings.Contains(logText, "/usr/bin/env KYBER_HARNESS_VERIFY_MODE=manifest") {
		t.Fatalf("chroot calls = %q", logText)
	}
}

func TestRuntimeRepairRejectsMismatchedVerification(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "agentroot")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	installer := filepath.Join(dir, "kyber-harness-install")
	writeExecutable(t, installer, "exit 0")
	writeExecutable(t, filepath.Join(dir, "chroot"), `
if [ "$2" = /usr/bin/env ]; then
  mkdir -p "$1/usr/lib/node_modules/@anthropic-ai/claude-code/bin" "$1/usr/bin"
  printf '%s\n' '{"version":"2.1.250","bin":{"claude":"bin/claude"}}' > "$1/usr/lib/node_modules/@anthropic-ai/claude-code/package.json"
  cat > "$1/usr/lib/node_modules/@anthropic-ai/claude-code/bin/claude" <<'EOF'
#!/usr/bin/env bash
echo 'claude 9.9.9'
EOF
  chmod 0755 "$1/usr/lib/node_modules/@anthropic-ai/claude-code/bin/claude"
  ln -sf ../lib/node_modules/@anthropic-ai/claude-code/bin/claude "$1/usr/bin/claude"
fi
`)
	cmd := exec.Command(runtimeRepairScript(t), root, "@anthropic-ai/claude-code", "claude", "2.1.250", "/usr/lib/node_modules/@anthropic-ai/claude-code", "/usr/bin/claude")
	cmd.Env = append(os.Environ(),
		"PATH="+dir+":"+os.Getenv("PATH"),
		"KYBER_REPAIR_TEST_ROOT="+root,
		"KYBER_REPAIR_INSTALLER_SOURCE="+installer,
	)
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "reports 9.9.9, expected 2.1.250") {
		t.Fatalf("mismatched repair should fail: err=%v\n%s", err, out)
	}
}

//go:build integration

// Package startcodexshell invokes start-codex.sh from Go to verify boot-time
// credential handling. The //go:build tag keeps it out of the default
// `go test ./...` run; run with `go test ./images/codex/ -tags=integration`.
package startcodexshell_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// scriptPath returns the absolute path to start-codex.sh.
func scriptPath(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	return filepath.Join(wd, "start-codex.sh")
}

// stubBin writes a fake `codex` (and the handful of other binaries the boot
// path shells out to) into a temp dir and returns a PATH with it first.
// `codex login status` must succeed or the script exits 42 before it ever
// reaches the block under test.
func stubBin(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	write := func(name, body string) {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("#!/usr/bin/env bash\n"+body+"\n"), 0o755); err != nil {
			t.Fatalf("write stub %s: %v", name, err)
		}
	}

	// `codex --version` -> a version string; `codex login status` -> success.
	write("codex", `
case "$1" in
  --version) echo "codex-cli 0.146.0" ;;
  login)     exit 0 ;;
  *)         exit 0 ;;
esac`)
	// The boot path reports its version through the sidecar and clones the
	// identity repo; neither is under test here.
	write("curl", `exit 0`)
	write("npm", `exit 0`)
	write("tmux", `exit 0`)

	return dir + ":" + os.Getenv("PATH")
}

func stubDeviceAuthBin(t *testing.T) (path, logPath, donePath string) {
	t.Helper()
	dir := t.TempDir()
	logPath = filepath.Join(dir, "tmux.log")
	donePath = filepath.Join(dir, "device.done")
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/usr/bin/env bash\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write("codex", `
if [ "${1:-}" = "--version" ]; then echo "codex-cli 0.146.0"; exit 0; fi
if [ "${1:-}" = "login" ] && [ "${2:-}" = "status" ]; then [ -f "$DEVICE_DONE" ]; exit; fi
exit 0`)
	write("tmux", `
case "${1:-}" in
  new-session)
    printf '%s\n' "$*" >"$DEVICE_LOG"
    printf '%s' '{"auth_mode":"chatgpt","tokens":{"refresh_token":"DEVICE-LOGIN"}}' >"$CODEX_HOME/auth.json"
    touch "$DEVICE_DONE"
    exit 0
    ;;
  has-session) exit 1 ;;
  *) exit 0 ;;
esac`)
	write("curl", `exit 0`)
	write("npm", `exit 0`)
	return dir + ":" + os.Getenv("PATH"), logPath, donePath
}

func stubVersionInstallBin(t *testing.T, initialVersion string) (path, sudoLog, npmLog string) {
	t.Helper()
	dir := t.TempDir()
	versionFile := filepath.Join(dir, "codex.version")
	sudoLog = filepath.Join(dir, "sudo.log")
	npmLog = filepath.Join(dir, "npm.log")
	if err := os.WriteFile(versionFile, []byte(initialVersion), 0o600); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/usr/bin/env bash\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write("codex", `
if [ "${1:-}" = "--version" ]; then echo "codex-cli $(cat "$VERSION_FILE")"; exit 0; fi
if [ "${1:-}" = "login" ] && [ "${2:-}" = "status" ]; then exit 0; fi
exit 0`)
	write("npm", `
printf '%s\n' "$*" >> "$NPM_LOG"
	if [ "${1:-}" = "view" ]; then echo '0.149.0'; fi`)
	write("kyber-harness-install", `
printf '%s\n' "$*" >> "$INSTALLER_LOG"
printf '%s' "$2" > "$VERSION_FILE"`)
	write("sudo", `printf '%s\n' "$*" >> "$SUDO_LOG"; exec "$@"`)
	write("curl", `exit 0`)
	write("tmux", `exit 0`)
	return dir + ":" + os.Getenv("PATH"), sudoLog, npmLog
}

func TestStartCodexLatestInstallsOnceAcrossTwoBoots(t *testing.T) {
	path, sudoLog, npmLog := stubVersionInstallBin(t, "0.146.0")
	home := t.TempDir()
	out, err := runBoot(t, home, "", path,
		"KYBER_REQUESTED_CODEX_VERSION=latest",
		"VERSION_FILE="+filepath.Join(strings.Split(path, ":")[0], "codex.version"),
		"SUDO_LOG="+sudoLog,
		"NPM_LOG="+npmLog,
		"INSTALLER_LOG="+npmLog,
		"KYBER_HARNESS_INSTALLER="+filepath.Join(strings.Split(path, ":")[0], "kyber-harness-install"),
	)
	if err != nil {
		t.Fatalf("latest boot failed: %v\n%s", err, out)
	}
	second, err := runBoot(t, home, "", path,
		"KYBER_REQUESTED_CODEX_VERSION=latest",
		"VERSION_FILE="+filepath.Join(strings.Split(path, ":")[0], "codex.version"),
		"SUDO_LOG="+sudoLog,
		"NPM_LOG="+npmLog,
		"INSTALLER_LOG="+npmLog,
		"KYBER_HARNESS_INSTALLER="+filepath.Join(strings.Split(path, ":")[0], "kyber-harness-install"),
	)
	if err != nil {
		t.Fatalf("second latest boot failed: %v\n%s", err, second)
	}
	got, err := os.ReadFile(npmLog)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(got), "@openai/codex 0.149.0 codex") != 1 {
		t.Fatalf("atomic installer calls = %q, want exactly one across two boots", got)
	}
	if !strings.Contains(string(second), "already installed (0.149.0); skipping install") {
		t.Fatalf("second boot did not skip the resolved latest version:\n%s", second)
	}
	if !strings.Contains(string(out), "runtime version reported: 0.149.0") {
		t.Fatalf("boot did not launch/report installed latest version:\n%s", out)
	}
}

// runBoot invokes start-codex.sh with SKIP_CODEX_LAUNCH so it stops after the
// boot path completes.
func runBoot(t *testing.T, home, authJSON, path string, extraEnv ...string) ([]byte, error) {
	t.Helper()
	persistRoot := t.TempDir()
	cmd := exec.Command("/bin/bash", scriptPath(t))
	env := []string{
		"HOME=" + home,
		"CODEX_HOME=" + filepath.Join(home, ".codex"),
		"KYBER_PERSIST_ROOT=" + persistRoot,
		"PATH=" + path,
		"SKIP_CODEX_LAUNCH=1",
		"AGENT_NAME=unit-test",
	}
	if authJSON != "" {
		env = append(env, "CODEX_AUTH_JSON="+authJSON)
	}
	env = append(env, extraEnv...)
	cmd.Env = env
	return cmd.CombinedOutput()
}

func TestStartCodexRunsDeviceAuthWhenSubscriptionLoginMissing(t *testing.T) {
	path, logPath, donePath := stubDeviceAuthBin(t)
	out, err := runBoot(t, t.TempDir(), `{}`, path,
		"DEVICE_LOG="+logPath, "DEVICE_DONE="+donePath)
	if err != nil {
		t.Fatalf("device-auth boot failed: %v\n%s", err, out)
	}
	logBytes, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(logBytes), "codex login --device-auth") {
		t.Fatalf("tmux command = %q, want codex login --device-auth", logBytes)
	}
	if !strings.Contains(string(out), "device authorization completed") {
		t.Fatalf("boot output does not report completed device auth:\n%s", out)
	}
}

func TestStartCodexMarkerRunsDeviceAuthEvenWhenCLIStatusSucceeds(t *testing.T) {
	path, logPath, donePath := stubDeviceAuthBin(t)
	// Codex 0.146 reports success for `login status` when auth.json is `{}`.
	// Simulate that behavior before boot; Kyber's marker must still win.
	if err := os.WriteFile(donePath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := runBoot(t, t.TempDir(), `{}`, path,
		"DEVICE_LOG="+logPath, "DEVICE_DONE="+donePath)
	if err != nil {
		t.Fatalf("device-auth boot failed: %v\n%s", err, out)
	}
	logBytes, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(logBytes), "codex login --device-auth") {
		t.Fatalf("tmux command = %q, want marker to force codex login --device-auth", logBytes)
	}
}

func TestStartCodexRejectsUnreplacedDeviceMarker(t *testing.T) {
	path, _, donePath := stubDeviceAuthBin(t)
	if err := os.WriteFile(donePath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// Do not use the tmux stub's new-session path, which simulates a completed
	// login by replacing auth.json. This stub ends the auth session immediately
	// while leaving Kyber's marker in place; `codex login status` still succeeds.
	stubDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(stubDir, "tmux"), []byte(`#!/usr/bin/env bash
case "${1:-}" in
  new-session) exit 0 ;;
  has-session) exit 1 ;;
  *) exit 0 ;;
esac
`), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := runBoot(t, t.TempDir(), `{}`, stubDir+":"+path)
	if err == nil {
		t.Fatalf("boot accepted an unreplaced device marker:\n%s", out)
	}
	if !strings.Contains(string(out), "device authorization did not complete") {
		t.Fatalf("unexpected failure output:\n%s", out)
	}
}

func TestStartCodexAPIKeySkipsSubscriptionLogin(t *testing.T) {
	home := t.TempDir()
	path, _, _ := stubDeviceAuthBin(t)
	out, err := runBoot(t, home, "", path, "OPENAI_API_KEY=sk-test")
	if err != nil {
		t.Fatalf("API-key boot failed: %v\n%s", err, out)
	}
	if strings.Contains(string(out), "device authorization") {
		t.Fatalf("API-key boot unexpectedly started device auth:\n%s", out)
	}
}

func TestStartCodexInstallsTelegramSkillOnlyWhenEnabled(t *testing.T) {
	skillRoot := t.TempDir()
	skillDir := filepath.Join(skillRoot, "telegram-messaging")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\nname: telegram-messaging\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	home := t.TempDir()
	out, err := runBoot(t, home, secretCred, stubBin(t),
		"KYBER_TELEGRAM_MCP_URL=http://127.0.0.1:14006/mcp", "KYBER_PLATFORM_SKILLS_DIR="+skillRoot)
	if err != nil {
		t.Fatalf("boot failed: %v\n%s", err, out)
	}
	installed := filepath.Join(home, ".codex", "skills", "telegram-messaging", "SKILL.md")
	if _, err := os.Stat(installed); err != nil {
		t.Fatalf("Telegram skill was not installed: %v\n%s", err, out)
	}

	disabledHome := t.TempDir()
	if out, err := runBoot(t, disabledHome, secretCred, stubBin(t), "KYBER_PLATFORM_SKILLS_DIR="+skillRoot); err != nil {
		t.Fatalf("disabled boot failed: %v\n%s", err, out)
	}
	if _, err := os.Lstat(filepath.Join(disabledHome, ".codex", "skills", "telegram-messaging")); !os.IsNotExist(err) {
		t.Fatalf("Telegram skill installed without Telegram enabled: %v", err)
	}
}

func TestStartCodexInstallsDiscordSkillOnlyWhenEnabled(t *testing.T) {
	skillRoot := t.TempDir()
	skillDir := filepath.Join(skillRoot, "discord-messaging")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\nname: discord-messaging\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	home := t.TempDir()
	out, err := runBoot(t, home, secretCred, stubBin(t),
		"KYBER_DISCORD_MCP_URL=http://127.0.0.1:14007/mcp", "KYBER_PLATFORM_SKILLS_DIR="+skillRoot)
	if err != nil {
		t.Fatalf("boot failed: %v\n%s", err, out)
	}
	installed := filepath.Join(home, ".codex", "skills", "discord-messaging", "SKILL.md")
	if _, err := os.Stat(installed); err != nil {
		t.Fatalf("Discord skill was not installed: %v\n%s", err, out)
	}

	disabledHome := t.TempDir()
	if out, err := runBoot(t, disabledHome, secretCred, stubBin(t), "KYBER_PLATFORM_SKILLS_DIR="+skillRoot); err != nil {
		t.Fatalf("disabled boot failed: %v\n%s", err, out)
	}
	if _, err := os.Lstat(filepath.Join(disabledHome, ".codex", "skills", "discord-messaging")); !os.IsNotExist(err) {
		t.Fatalf("Discord skill installed without Discord enabled: %v", err)
	}
}

func readAuth(t *testing.T, home string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(home, ".codex", "auth.json"))
	if err != nil {
		t.Fatalf("read auth.json: %v", err)
	}
	return string(data)
}

func TestStartCodexDisablesRuntimeSelfUpdatePrompt(t *testing.T) {
	home := t.TempDir()
	managed := filepath.Join(t.TempDir(), "managed_config.toml")
	out, err := runBoot(t, home, secretCred, stubBin(t), "KYBER_MANAGED_CODEX_CONFIG="+managed)
	if err != nil {
		t.Fatalf("boot failed: %v\n%s", err, out)
	}
	// Kyber's settings moved out of the agent-owned config.toml and into the
	// system managed config, so that rewriting them cannot clobber MCP
	// servers the agent registered itself.
	config, err := os.ReadFile(managed)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(config), "check_for_update_on_startup = false") {
		t.Fatalf("managed config does not disable Codex's self-update prompt:\n%s", config)
	}
}

func TestStartCodexRegistersDiscordMCP(t *testing.T) {
	script, err := os.ReadFile(scriptPath(t))
	if err != nil {
		t.Fatal(err)
	}
	// Registration goes through `codex mcp add`, which edits config.toml as
	// TOML. The old heredoc append is deliberately gone: it only worked
	// because the boot path truncated the file first.
	for _, want := range []string{"kyber_converge_mcp kyber_discord", `"${KYBER_DISCORD_MCP_URL:-}"`} {
		if !strings.Contains(string(script), want) {
			t.Fatalf("start-codex.sh missing Discord MCP registration %q", want)
		}
	}
	if strings.Contains(string(script), `cat > "$CODEX_HOME/config.toml"`) {
		t.Fatal("start-codex.sh still truncates the agent's config.toml")
	}
}

func TestStartCodexRegistersRequestReplyMCP(t *testing.T) {
	script, err := os.ReadFile(scriptPath(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"kyber_converge_mcp kyber_request_reply", `"${KYBER_REQUEST_MCP_URL:-}"`} {
		if !strings.Contains(string(script), want) {
			t.Fatalf("start-codex.sh missing request MCP registration %q", want)
		}
	}
}

func TestStartCodexRendersSessionRecall(t *testing.T) {
	home := t.TempDir()
	repo := filepath.Join(home, "dev", "echo-agent")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(t.TempDir(), "session-state.json")
	const snapshot = `{"updated_at":"2026-08-06T14:00:03Z","last_activity":"Saved the launch plan.","recent_exchanges":[{"role":"user","timestamp":"2026-08-06T14:00:01Z","content":"Remember the plan"},{"role":"assistant","timestamp":"2026-08-06T14:00:03Z","content":"Saved the launch plan."}]}`
	if err := os.WriteFile(state, []byte(snapshot), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := runBoot(t, home, secretCred, stubBin(t),
		"KYBER_IDENTITY_REPO=matty-v/echo-agent", "KYBER_SESSION_STATE_FILE="+state,
		"KYBER_IDENTITY_REPO_SCRIPT=/dev/null")
	if err != nil {
		t.Fatalf("boot failed: %v\n%s", err, out)
	}
	recall, err := os.ReadFile(filepath.Join(repo, ".runtime", "session-recall.md"))
	if err != nil {
		t.Fatalf("session-recall.md not written: %v\n%s", err, out)
	}
	for _, want := range []string{"Saved the launch plan.", "Remember the plan", "2026-08-06T14:00:03Z"} {
		if !strings.Contains(string(recall), want) {
			t.Errorf("session recall missing %q:\n%s", want, recall)
		}
	}
}

func TestRestartSessionDoesNotLeakSessionLockIntoTmux(t *testing.T) {
	script, err := os.ReadFile("start-codex.sh")
	if err != nil {
		t.Fatal(err)
	}
	const want = `"\${TMUX[@]}" new-session -d -s agent -c $(printf '%q' "$LAUNCH_DIR") "\$RELAUNCH_CMD" 9>&-`
	if !strings.Contains(string(script), want) {
		t.Fatal("restart-session tmux launch does not close fd 9; tmux would inherit the session lock and permanently block inbound dispatch")
	}
}

// TestStartCodexSessionResumeSourceContract pins the kyber#118 source
// invariants that the rendered-script test below cannot see: the resume
// command is captured AFTER the startup prompt joins CODEX_ARGS (a resumed
// session receives the prompt as its wake-up turn, so an agent interrupted
// mid-task continues instead of idling), and boot gates on the enable flag
// plus a recorded session.
func TestStartCodexSessionResumeSourceContract(t *testing.T) {
	src, err := os.ReadFile(scriptPath(t))
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	resumeDef := strings.Index(s, `CODEX_RESUME_CMD="codex resume --last $(printf '%q ' "${CODEX_ARGS[@]}")"`)
	promptAppend := strings.Index(s, `CODEX_ARGS+=(-- "$KYBER_STARTUP_PROMPT")`)
	if resumeDef < 0 {
		t.Fatal("CODEX_RESUME_CMD definition missing")
	}
	if promptAppend < 0 {
		t.Fatal("startup prompt append missing")
	}
	if resumeDef < promptAppend {
		t.Fatal("CODEX_RESUME_CMD is built BEFORE the startup prompt joins CODEX_ARGS — a resumed session would idle with no turn to act on")
	}
	if !strings.Contains(s, `BOOT_LAUNCH_CMD="$CODEX_RESUME_CMD"`) {
		t.Fatal("boot launch never selects the resume command")
	}
	if !strings.Contains(s, `"$PERSIST_ROOT/last-codex-launch.sh" $RELAUNCH_FLAG || true`) {
		t.Fatal("crash watchdog lost the --fresh fallback for poison transcripts")
	}
}

// TestGeneratedCodexRelaunchScript_SessionResume renders the last-codex-launch.sh
// heredoc exactly as boot would (resume enabled) and executes the result in
// all three modes, asserting against a logging tmux stub:
//
//	bare + empty session store  -> fresh launch (with startup prompt)
//	bare + recorded session     -> `codex resume --last` (prompt delivered
//	                               into the resumed session)
//	--fresh + recorded session  -> fresh launch (intentional restart wins)
func TestGeneratedCodexRelaunchScript_SessionResume(t *testing.T) {
	src, err := os.ReadFile(scriptPath(t))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(src), "\n")
	start, end := -1, -1
	for i, l := range lines {
		if strings.HasPrefix(l, `cat > "$PERSIST_ROOT/last-codex-launch.sh" <<EOF`) {
			start = i
		} else if start >= 0 && l == "EOF" {
			end = i
			break
		}
	}
	if start < 0 || end < 0 {
		t.Fatalf("could not locate last-codex-launch.sh heredoc (start=%d end=%d)", start, end)
	}
	block := strings.Join(lines[start:end+1], "\n")

	work := t.TempDir()
	persist := filepath.Join(work, "persist")
	if err := os.MkdirAll(filepath.Join(persist, "var", "lock"), 0o755); err != nil {
		t.Fatal(err)
	}
	codexHome := filepath.Join(work, "codex-home")
	sessionDay := filepath.Join(codexHome, "sessions", "2026", "08", "23")
	if err := os.MkdirAll(sessionDay, 0o755); err != nil {
		t.Fatal(err)
	}

	bin := filepath.Join(work, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	// has-session answers from a flag file (absent = dead session, the
	// normal watchdog case); everything else is logged.
	tmuxLog := filepath.Join(work, "tmux.log")
	aliveFlag := filepath.Join(work, "session-alive")
	if err := os.WriteFile(filepath.Join(bin, "tmux"),
		[]byte("#!/usr/bin/env bash\ncase \"$*\" in *has-session*) [ -f '"+aliveFlag+"' ] && exit 0 || exit 1;; esac\necho \"tmux $*\" >> '"+tmuxLog+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	wrapper := strings.Join([]string{
		"set -u",
		"PERSIST_ROOT='" + persist + "'",
		"LAUNCH_DIR='/home/kyber/dev/test-agent'",
		"CODEX_HOME='" + codexHome + "'",
		`CODEX_LAUNCH_CMD='codex --model gpt-test --ask-for-approval never --sandbox danger-full-access -- startup\ prompt'`,
		`CODEX_RESUME_CMD='codex resume --last --model gpt-test --ask-for-approval never --sandbox danger-full-access -- startup\ prompt'`,
		"SESSION_RESUME_ENABLED=1",
		block,
		"",
	}, "\n")
	wrapperPath := filepath.Join(work, "wrapper.sh")
	if err := os.WriteFile(wrapperPath, []byte(wrapper), 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("/bin/bash", wrapperPath).CombinedOutput(); err != nil {
		t.Fatalf("rendering heredoc: %v\n%s", err, out)
	}
	gen := filepath.Join(persist, "last-codex-launch.sh")

	run := func(arg ...string) string {
		t.Helper()
		os.Remove(tmuxLog)
		cmd := exec.Command("/bin/bash", append([]string{gen}, arg...)...)
		cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("running generated script %v: %v\n%s", arg, err, out)
		}
		log, err := os.ReadFile(tmuxLog)
		if err != nil {
			t.Fatalf("tmux stub never ran: %v", err)
		}
		return string(log)
	}

	if got := run(); !strings.Contains(got, "codex --model gpt-test") || strings.Contains(got, "resume --last") {
		t.Errorf("empty store: want fresh launch, got:\n%s", got)
	}

	if err := os.WriteFile(filepath.Join(sessionDay, "rollout-1.jsonl"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := run(); !strings.Contains(got, "codex resume --last") {
		t.Errorf("recorded session: want resume launch, got:\n%s", got)
	} else if !strings.Contains(got, "startup") {
		t.Errorf("resume launch must deliver the startup prompt, got:\n%s", got)
	}

	if got := run("--fresh"); strings.Contains(got, "resume --last") {
		t.Errorf("--fresh: intentional restart must stay fresh, got:\n%s", got)
	}

	// Race guard: a bare (watchdog) invocation that finds the session alive
	// must do nothing; --fresh still kills + relaunches.
	if err := os.WriteFile(aliveFlag, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	os.Remove(tmuxLog)
	cmd := exec.Command("/bin/bash", gen)
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("bare run with live session: %v\n%s", err, out)
	} else if !strings.Contains(string(out), "already alive") {
		t.Errorf("bare run with live session did not report the race skip:\n%s", out)
	}
	if _, err := os.Stat(tmuxLog); err == nil {
		log, _ := os.ReadFile(tmuxLog)
		t.Errorf("bare run with live session must not kill/relaunch, but ran:\n%s", log)
	}
	if got := run("--fresh"); !strings.Contains(got, "new-session") {
		t.Errorf("--fresh with live session must still relaunch, got:\n%s", got)
	}
}

const (
	secretCred = `{"auth_mode":"chatgpt","tokens":{"refresh_token":"SECRET-ORIGINAL"},"last_refresh":"2026-07-25T20:18:23Z"}`
	// What the CLI would leave behind after a successful refresh: a NEW
	// refresh token, because ChatGPT rotates them on every use.
	refreshedCred = `{"auth_mode":"chatgpt","tokens":{"refresh_token":"ROTATED-BY-CLI"},"last_refresh":"2026-08-04T20:18:23Z"}`
	reauthCred    = `{"auth_mode":"chatgpt","tokens":{"refresh_token":"OPERATOR-REAUTH"},"last_refresh":"2026-08-05T09:00:00Z"}`
)

// TestStartCodex_FirstBoot_SeedsFromSecret covers the cold path: nothing on
// disk, so the Secret's copy must be installed.
func TestStartCodex_FirstBoot_SeedsFromSecret(t *testing.T) {
	home := t.TempDir()
	path := stubBin(t)

	out, err := runBoot(t, home, secretCred, path)
	if err != nil {
		t.Fatalf("boot failed: %v\noutput:\n%s", err, out)
	}
	if got := readAuth(t, home); got != secretCred {
		t.Fatalf("auth.json = %s, want the secret copy %s", got, secretCred)
	}
	if !strings.Contains(string(out), "no local copy") {
		t.Errorf("expected the no-local-copy seed message, got:\n%s", out)
	}
}

// TestStartCodex_Reboot_KeepsRefreshedCredential is the regression test for
// kyber#681 and the whole point of the fix.
//
// Before the fix this block overwrote auth.json unconditionally, so the token
// the CLI had rotated into place was replaced by the Secret's already-burnt
// original. Because ChatGPT refresh tokens are single use, that made the agent
// permanently unauthenticated — HK-47's failure mode on 2026-08-04.
func TestStartCodex_Reboot_KeepsRefreshedCredential(t *testing.T) {
	home := t.TempDir()
	path := stubBin(t)

	// Boot 1: seed from the Secret.
	if out, err := runBoot(t, home, secretCred, path); err != nil {
		t.Fatalf("first boot failed: %v\noutput:\n%s", err, out)
	}

	// The CLI refreshes during the session and rotates the refresh token.
	authPath := filepath.Join(home, ".codex", "auth.json")
	if err := os.WriteFile(authPath, []byte(refreshedCred), 0o600); err != nil {
		t.Fatalf("simulate refresh: %v", err)
	}

	// Boot 2: the Secret still holds the ORIGINAL, now-burnt credential.
	out, err := runBoot(t, home, secretCred, path)
	if err != nil {
		t.Fatalf("second boot failed: %v\noutput:\n%s", err, out)
	}

	got := readAuth(t, home)
	if got == secretCred {
		t.Fatal("REGRESSION (kyber#681): boot clobbered the CLI-refreshed credential " +
			"with the Secret's burnt original — the agent can never authenticate again")
	}
	if got != refreshedCred {
		t.Fatalf("auth.json = %s, want the refreshed copy %s", got, refreshedCred)
	}
	if !strings.Contains(string(out), "keeping locally refreshed") {
		t.Errorf("expected the keep-local message, got:\n%s", out)
	}
}

// TestStartCodex_SecretChanged_OperatorReauthWins guards the other direction:
// seed-if-changed must not become never-seed-again. Re-authorising from the PWA
// updates the Secret, and that has to reach the pod even though a (dead) local
// copy exists.
func TestStartCodex_SecretChanged_OperatorReauthWins(t *testing.T) {
	home := t.TempDir()
	path := stubBin(t)

	if out, err := runBoot(t, home, secretCred, path); err != nil {
		t.Fatalf("first boot failed: %v\noutput:\n%s", err, out)
	}
	authPath := filepath.Join(home, ".codex", "auth.json")
	if err := os.WriteFile(authPath, []byte(refreshedCred), 0o600); err != nil {
		t.Fatalf("simulate refresh: %v", err)
	}

	// Operator re-authorises: the Secret now carries a brand new document.
	out, err := runBoot(t, home, reauthCred, path)
	if err != nil {
		t.Fatalf("reauth boot failed: %v\noutput:\n%s", err, out)
	}
	if got := readAuth(t, home); got != reauthCred {
		t.Fatalf("auth.json = %s, want the operator's new credential %s", got, reauthCred)
	}
	if !strings.Contains(string(out), "secret changed") {
		t.Errorf("expected the secret-changed seed message, got:\n%s", out)
	}
}

// TestStartCodex_UpgradeAdoptsExistingCredential covers the migration boot: an
// agent created before this fix has a live auth.json but no seed marker.
//
// Seeding there would clobber a possibly-already-refreshed credential exactly
// once, on the upgrade boot — which for a HEALTHY Codex agent means burning a
// working login. The upgrade must adopt what is on disk, not overwrite it.
func TestStartCodex_UpgradeAdoptsExistingCredential(t *testing.T) {
	home := t.TempDir()
	path := stubBin(t)

	// Simulate the pre-fix world: a refreshed credential on disk, no marker.
	codexHome := filepath.Join(home, ".codex")
	if err := os.MkdirAll(codexHome, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(codexHome, "auth.json"), []byte(refreshedCred), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	out, err := runBoot(t, home, secretCred, path)
	if err != nil {
		t.Fatalf("upgrade boot failed: %v\noutput:\n%s", err, out)
	}
	if got := readAuth(t, home); got != refreshedCred {
		t.Fatalf("upgrade boot clobbered a live credential: auth.json = %s, want %s", got, refreshedCred)
	}
	if !strings.Contains(string(out), "adopted existing") {
		t.Errorf("expected the adoption message, got:\n%s", out)
	}

	// And the marker it wrote must make the NEXT operator re-auth still win.
	out, err = runBoot(t, home, reauthCred, path)
	if err != nil {
		t.Fatalf("post-adoption reauth boot failed: %v\noutput:\n%s", err, out)
	}
	if got := readAuth(t, home); got != reauthCred {
		t.Fatalf("re-auth after adoption did not take: auth.json = %s, want %s", got, reauthCred)
	}
}

// TestStartCodex_EmptyLocalFile_Reseeds covers a truncated auth.json — a
// zero-byte file must not be mistaken for a valid local credential.
func TestStartCodex_EmptyLocalFile_Reseeds(t *testing.T) {
	home := t.TempDir()
	path := stubBin(t)

	codexHome := filepath.Join(home, ".codex")
	if err := os.MkdirAll(codexHome, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(codexHome, "auth.json"), nil, 0o600); err != nil {
		t.Fatalf("write empty: %v", err)
	}

	if out, err := runBoot(t, home, secretCred, path); err != nil {
		t.Fatalf("boot failed: %v\noutput:\n%s", err, out)
	}
	if got := readAuth(t, home); got != secretCred {
		t.Fatalf("auth.json = %s, want the secret copy %s", got, secretCred)
	}
}

func TestStartCodex_StartupPromptIsSingleQuotedArgument(t *testing.T) {
	src, err := os.ReadFile(scriptPath(t))
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	if !strings.Contains(s, `CODEX_ARGS+=(-- "$KYBER_STARTUP_PROMPT")`) {
		t.Fatal("startup prompt is not appended as one argument after --")
	}
	if !strings.Contains(s, `CODEX_LAUNCH_CMD="codex $(printf '%q ' "${CODEX_ARGS[@]}")"`) {
		t.Fatal("Codex launch command no longer shell-quotes its argument array")
	}
}

// stubMCPBin writes a `codex` stub that actually implements the `mcp`
// subcommands against $CODEX_HOME/config.toml, so a test can assert what the
// boot path does to a real file rather than only that it shelled out.
// Mirrors the parts of `codex mcp` the boot path relies on: `add` upserts,
// `remove` is a no-op when the entry is absent, and `list` fails when the
// file does not parse.
func stubMCPBin(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/usr/bin/env bash\n"+body+"\n"), 0o755); err != nil {
			t.Fatalf("write stub %s: %v", name, err)
		}
	}
	write("codex", `
cfg="$CODEX_HOME/config.toml"
strip_block() { # $1 = server name
  [ -f "$cfg" ] || return 0
  awk -v name="[mcp_servers.$1]" '
    $0 == name { skip = 1; next }
    /^\[/      { skip = 0 }
    !skip      { print }
  ' "$cfg" > "$cfg.tmp" && mv "$cfg.tmp" "$cfg"
}
case "${1:-}" in
  --version) echo "codex-cli 0.150.1" ;;
  login)     exit 0 ;;
  mcp)
    case "${2:-}" in
      list)   grep -q UNPARSEABLE "$cfg" 2>/dev/null && exit 1; exit 0 ;;
      add)    name="$3"; url=""
              shift 3
              while [ $# -gt 0 ]; do
                if [ "$1" = "--url" ]; then url="$2"; fi
                shift
              done
              strip_block "$name"
              printf '\n[mcp_servers.%s]\nurl = "%s"\n' "$name" "$url" >> "$cfg" ;;
      remove) strip_block "$3" ;;
      *)      exit 0 ;;
    esac ;;
  *) exit 0 ;;
esac`)
	write("curl", `exit 0`)
	write("npm", `exit 0`)
	write("tmux", `exit 0`)
	write("sudo", `exec "$@"`)
	return dir + ":" + os.Getenv("PATH")
}

// seedCodexConfig writes a config.toml containing an agent-registered MCP
// server plus an unrelated top-level key, and returns the managed-config path
// the boot run should use.
func seedCodexConfig(t *testing.T, home, body string) string {
	t.Helper()
	codexHome := filepath.Join(home, ".codex")
	if err := os.MkdirAll(codexHome, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(codexHome, "config.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(t.TempDir(), "managed_config.toml")
}

const agentOwnedConfig = `some_agent_setting = 42

[mcp_servers.atlassian_rovo]
command = "npx"
args = ["-y", "mcp-remote", "https://example.atlassian.net/mcp"]
`

// A custom MCP server registered by the agent must survive a boot. Before
// kyber#MCPFIX the boot path truncated config.toml with `cat >`, so the entry
// was deleted on every restart and no durable integration was possible.
func TestStartCodexPreservesAgentAddedMCPServers(t *testing.T) {
	home := t.TempDir()
	managed := seedCodexConfig(t, home, agentOwnedConfig)

	out, err := runBoot(t, home, secretCred, stubMCPBin(t),
		"KYBER_MANAGED_CODEX_CONFIG="+managed,
		"KYBER_TELEGRAM_MCP_URL=http://127.0.0.1:14004/mcp",
		"KYBER_DISCORD_MCP_URL=http://127.0.0.1:14005/mcp")
	if err != nil {
		t.Fatalf("boot failed: %v\n%s", err, out)
	}

	config, err := os.ReadFile(filepath.Join(home, ".codex", "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(config)
	for _, want := range []string{
		"[mcp_servers.atlassian_rovo]", // the agent's own entry
		`args = ["-y", "mcp-remote", "https://example.atlassian.net/mcp"]`,
		"some_agent_setting = 42",      // unrelated key
		"[mcp_servers.kyber_telegram]", // managed entries still converge
		"[mcp_servers.kyber_discord]",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("config.toml lost %q after boot:\n%s", want, got)
		}
	}
}

// Kyber's own settings must not land in the agent's file at all.
func TestStartCodexKeepsItsOwnSettingsOutOfAgentConfig(t *testing.T) {
	home := t.TempDir()
	managed := seedCodexConfig(t, home, agentOwnedConfig)

	out, err := runBoot(t, home, secretCred, stubMCPBin(t),
		"KYBER_MANAGED_CODEX_CONFIG="+managed)
	if err != nil {
		t.Fatalf("boot failed: %v\n%s", err, out)
	}

	config, err := os.ReadFile(filepath.Join(home, ".codex", "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, unwanted := range []string{"approval_policy", "sandbox_mode", "check_for_update_on_startup", "trust_level"} {
		if strings.Contains(string(config), unwanted) {
			t.Fatalf("Kyber setting %q written into the agent's config.toml:\n%s", unwanted, config)
		}
	}

	managedBody, err := os.ReadFile(managed)
	if err != nil {
		t.Fatalf("managed config not written: %v", err)
	}
	for _, want := range []string{
		`approval_policy = "never"`,
		`sandbox_mode = "danger-full-access"`,
		"check_for_update_on_startup = false",
		`tui.resume_cwd = "current"`,
		`trust_level = "trusted"`,
	} {
		if !strings.Contains(string(managedBody), want) {
			t.Fatalf("managed config missing %q:\n%s", want, managedBody)
		}
	}
}

// A managed channel that gets disabled must not leave a stale entry behind.
func TestStartCodexRemovesDisabledManagedMCP(t *testing.T) {
	home := t.TempDir()
	managed := seedCodexConfig(t, home, agentOwnedConfig+`
[mcp_servers.kyber_discord]
url = "http://127.0.0.1:14005/mcp"
`)

	out, err := runBoot(t, home, secretCred, stubMCPBin(t),
		"KYBER_MANAGED_CODEX_CONFIG="+managed,
		"KYBER_TELEGRAM_MCP_URL=http://127.0.0.1:14004/mcp")
	if err != nil {
		t.Fatalf("boot failed: %v\n%s", err, out)
	}

	config, err := os.ReadFile(filepath.Join(home, ".codex", "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(config)
	if strings.Contains(got, "[mcp_servers.kyber_discord]") {
		t.Fatalf("disabled Discord MCP left a stale entry:\n%s", got)
	}
	if !strings.Contains(got, "[mcp_servers.kyber_telegram]") {
		t.Fatalf("enabled Telegram MCP not registered:\n%s", got)
	}
	if !strings.Contains(got, "[mcp_servers.atlassian_rovo]") {
		t.Fatalf("agent's own MCP entry lost:\n%s", got)
	}
}

// An unparseable config.toml must be recovered, not fatal — the same
// treatment start-claude.sh gives a corrupt ~/.claude.json.
func TestStartCodexRecoversUnparseableConfig(t *testing.T) {
	home := t.TempDir()
	managed := seedCodexConfig(t, home, "UNPARSEABLE ((( not toml\n")

	out, err := runBoot(t, home, secretCred, stubMCPBin(t),
		"KYBER_MANAGED_CODEX_CONFIG="+managed,
		"KYBER_TELEGRAM_MCP_URL=http://127.0.0.1:14004/mcp")
	if err != nil {
		t.Fatalf("boot failed on a corrupt config: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(home, ".codex", "config.toml.corrupt")); err != nil {
		t.Fatalf("corrupt config was not preserved for diagnosis: %v", err)
	}
	config, err := os.ReadFile(filepath.Join(home, ".codex", "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(config), "UNPARSEABLE") {
		t.Fatalf("corrupt config.toml was not reset:\n%s", config)
	}
	if !strings.Contains(string(config), "[mcp_servers.kyber_telegram]") {
		t.Fatalf("managed MCP not registered after recovery:\n%s", config)
	}
}

// stubBrokenMCPBin writes a `codex` whose `mcp list` always fails, standing in
// for a fault that is NOT the agent's config.toml — a malformed managed
// setting, an I/O error, or a broken codex binary. The boot path must not read
// that as "the user's file is corrupt".
func stubBrokenMCPBin(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/usr/bin/env bash\n"+body+"\n"), 0o755); err != nil {
			t.Fatalf("write stub %s: %v", name, err)
		}
	}
	write("codex", `
case "${1:-}" in
  --version) echo "codex-cli 0.150.1" ;;
  login)     exit 0 ;;
  mcp)       exit 1 ;;
  *)         exit 0 ;;
esac`)
	write("curl", `exit 0`)
	write("npm", `exit 0`)
	write("tmux", `exit 0`)
	write("sudo", `exec "$@"`)
	return dir + ":" + os.Getenv("PATH")
}

// A `codex mcp` failure that is not attributable to the agent's config.toml
// must leave that file completely alone. Resetting it here would destroy every
// custom MCP server and unrelated setting — the data loss this change exists
// to prevent — so the boot path probes an empty config first and only treats
// the file as corrupt when the empty one parses and this one does not.
func TestStartCodexKeepsUserConfigWhenFailureIsNotTheUserFile(t *testing.T) {
	home := t.TempDir()
	managed := seedCodexConfig(t, home, agentOwnedConfig)

	out, err := runBoot(t, home, secretCred, stubBrokenMCPBin(t),
		"KYBER_MANAGED_CODEX_CONFIG="+managed,
		"KYBER_TELEGRAM_MCP_URL=http://127.0.0.1:14004/mcp")
	if err != nil {
		t.Fatalf("boot failed: %v\n%s", err, out)
	}

	config, err := os.ReadFile(filepath.Join(home, ".codex", "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(config) != agentOwnedConfig {
		t.Fatalf("agent config.toml was modified despite the failure not being its fault:\ngot:\n%s\nwant:\n%s", config, agentOwnedConfig)
	}
	if _, err := os.Stat(filepath.Join(home, ".codex", "config.toml.corrupt")); err == nil {
		t.Fatal("a valid config.toml was backed up and reset as if it were corrupt")
	}
	if !strings.Contains(string(out), "skipping MCP convergence") {
		t.Fatalf("boot output does not report that convergence was skipped:\n%s", out)
	}
}

// ---- Scheduled-job turn-boundary hooks (FAL-8) -------------------------------
//
// Codex 0.146.0 exposes the same two hook events Claude Code uses
// (UserPromptSubmit, Stop). They are registered in the Kyber-managed config
// rather than the agent's own, because hooks in the agent's config sit behind
// an interactive trust prompt that a headless pod can never answer. These
// cover the three cases the contract turns on: both signals registered, only
// one registered, and neither available.

// cronHookEnv points the boot path at temp paths the test can inspect.
// turnStartExists/postrunExists control which of the two hook commands is
// actually installed, which is what drives the both-or-neither rule.
func cronHookEnv(t *testing.T, turnStartExists, postrunExists bool) (sentinel, managed string, env []string) {
	t.Helper()
	dir := t.TempDir()
	sentinel = filepath.Join(dir, "run", "kyber-cron-postrun-enabled")
	managed = filepath.Join(dir, "etc", "managed_config.toml")

	postrun := filepath.Join(dir, "kyber-cron-postrun")
	turnstart := filepath.Join(dir, "kyber-cron-turn-start")
	install := func(p string) {
		if err := os.WriteFile(p, []byte("#!/usr/bin/env bash\nexit 0\n"), 0o755); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	if turnStartExists {
		install(turnstart)
	}
	if postrunExists {
		install(postrun)
	}

	return sentinel, managed, []string{
		"KYBER_CRON_POSTRUN_SENTINEL=" + sentinel,
		"KYBER_MANAGED_CODEX_CONFIG=" + managed,
		"KYBER_CRON_POSTRUN_CMD=" + postrun,
		"KYBER_CRON_TURNSTART_CMD=" + turnstart,
	}
}

// Happy path: both signals land in the managed config and the sentinel — the
// contract kyber-job-dispatch reads — is armed. Also pins that registering the
// hooks does not cost the managed settings that share the file (kyber#160):
// the hook TABLES must come after every top-level key, or `model` would be
// parsed as a member of the last table instead of a top-level setting.
func TestStartCodex_CronHooks_RegistersBothSignalsAndArmsSentinel(t *testing.T) {
	home := t.TempDir()
	sentinel, managed, env := cronHookEnv(t, true, true)
	env = append(env, "CODEX_MODEL=gpt-5.6-sol")

	out, err := runBoot(t, home, "", stubBin(t), env...)
	if err != nil {
		t.Fatalf("boot failed: %v\n%s", err, out)
	}

	body, readErr := os.ReadFile(managed)
	if readErr != nil {
		t.Fatalf("managed config not written: %v\n%s", readErr, out)
	}
	got := string(body)
	for _, want := range []string{
		"[[hooks.UserPromptSubmit]]",
		"[[hooks.Stop]]",
		"kyber-cron-turn-start",
		"kyber-cron-postrun",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("managed config missing %q:\n%s", want, got)
		}
	}
	// The clear command must ride inside the Stop hook's command string, and it
	// must be a FRESH-CONVERSATION command. This is the whole point of the
	// flag: `/compact` summarizes the thread and carries the summary forward,
	// so a job firing unattended would still see every earlier run's context —
	// measured against 0.146.0, a token planted before `/compact` was still in
	// the next turn's upstream request, and was gone after `/clear`. Asserting
	// the VALUE rather than "some clear text is present" is what makes this
	// catch a regression back to compaction.
	if !strings.Contains(got, "KYBER_CLEAR_SESSION_TEXT=/clear") {
		t.Errorf("Stop hook does not carry a fresh-conversation clear command:\n%s", got)
	}
	if strings.Contains(got, "KYBER_CLEAR_SESSION_TEXT=/compact") {
		t.Errorf("Stop hook uses compaction, which retains prior-job context and breaks "+
			"the clearContextAfter contract:\n%s", got)
	}
	// kyber#160's settings must survive, and `model` must still be top-level.
	for _, want := range []string{"approval_policy", "sandbox_mode", "tui.resume_cwd"} {
		if !strings.Contains(got, want) {
			t.Errorf("registering hooks dropped the managed setting %q:\n%s", want, got)
		}
	}
	modelAt := strings.Index(got, "model = ")
	if modelAt < 0 {
		t.Fatalf("managed config lost the model setting:\n%s", got)
	}
	if firstTable := strings.Index(got, "[["); firstTable >= 0 && modelAt > firstTable {
		t.Errorf("model is written after a [[table]] header, so TOML reads it as part of that table:\n%s", got)
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Errorf("sentinel not armed after a complete registration: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "cron context hooks registered") {
		t.Errorf("boot did not report registration:\n%s", out)
	}
}

// Both-or-neither: when only ONE of the two hook commands is installed, only
// that half can be registered, and the sentinel must stay absent. Half-wired is
// worse than not claiming the capability — arming without consuming leaks
// markers and mutes exclusive; consuming without arming clears context on an
// unrelated turn.
func TestStartCodex_CronHooks_PartialRegistrationLeavesSentinelAbsent(t *testing.T) {
	home := t.TempDir()
	// Turn-start present, turn-end missing.
	sentinel, managed, env := cronHookEnv(t, true, false)

	// A stale sentinel from a boot that DID have both signals must be cleared,
	// not merely left uncreated.
	if err := os.MkdirAll(filepath.Dir(sentinel), 0o755); err != nil {
		t.Fatalf("mkdir sentinel dir: %v", err)
	}
	if err := os.WriteFile(sentinel, nil, 0o644); err != nil {
		t.Fatalf("seed stale sentinel: %v", err)
	}

	out, err := runBoot(t, home, "", stubBin(t), env...)
	if err != nil {
		t.Fatalf("boot failed: %v\n%s", err, out)
	}

	body, _ := os.ReadFile(managed)
	got := string(body)
	if !strings.Contains(got, "[[hooks.UserPromptSubmit]]") {
		t.Errorf("the available half was not registered, so this does not exercise the partial case:\n%s", got)
	}
	if strings.Contains(got, "[[hooks.Stop]]") {
		t.Errorf("registered a Stop hook whose command is not installed:\n%s", got)
	}
	if _, statErr := os.Stat(sentinel); !os.IsNotExist(statErr) {
		t.Errorf("sentinel survived a half-registered managed config (err=%v)\n%s", statErr, out)
	}
	if !strings.Contains(string(out), "cron context hooks incomplete; feature disabled") {
		t.Errorf("boot did not warn about the incomplete registration:\n%s", out)
	}
}

// Neither signal available: no hook tables are written at all and the sentinel
// stays absent, so both flags stay accepted-but-inert rather than half-working.
func TestStartCodex_CronHooks_CommandsMissingLeavesSentinelAbsent(t *testing.T) {
	home := t.TempDir()
	sentinel, managed, env := cronHookEnv(t, false, false)

	if err := os.MkdirAll(filepath.Dir(sentinel), 0o755); err != nil {
		t.Fatalf("mkdir sentinel dir: %v", err)
	}
	if err := os.WriteFile(sentinel, nil, 0o644); err != nil {
		t.Fatalf("seed stale sentinel: %v", err)
	}

	out, err := runBoot(t, home, "", stubBin(t), env...)
	if err != nil {
		t.Fatalf("boot failed: %v\n%s", err, out)
	}

	body, _ := os.ReadFile(managed)
	if strings.Contains(string(body), "[[hooks.") {
		t.Errorf("hook tables written with no hook commands installed:\n%s", body)
	}
	if _, statErr := os.Stat(sentinel); !os.IsNotExist(statErr) {
		t.Errorf("sentinel survived with no hook commands installed (err=%v)\n%s", statErr, out)
	}
	if !strings.Contains(string(out), "cron context hooks incomplete; feature disabled") {
		t.Errorf("boot did not warn that registration is incomplete:\n%s", out)
	}
}

// stopHookCommand pulls the `command` string out of the [[hooks.Stop]] block of
// a rendered managed config. Parsing what was WRITTEN — rather than
// re-deriving the string in the test — is what makes the delivery test below
// exercise the same bytes Codex will execute.
func stopHookCommand(t *testing.T, toml string) string {
	t.Helper()
	at := strings.Index(toml, "[[hooks.Stop]]")
	if at < 0 {
		t.Fatalf("no [[hooks.Stop]] block in managed config:\n%s", toml)
	}
	for _, line := range strings.Split(toml[at:], "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "command = ") {
			continue
		}
		cmd, err := strconv.Unquote(strings.TrimPrefix(line, "command = "))
		if err != nil {
			t.Fatalf("Stop hook command is not a quoted TOML string (%q): %v", line, err)
		}
		return cmd
	}
	t.Fatalf("[[hooks.Stop]] block has no command:\n%s", toml)
	return ""
}

// End-to-end for the half of clearContextAfter that Kyber owns: what the Stop
// hook actually DELIVERS to the Codex pane.
//
// The narrower assertion in the happy-path test reads a string out of the
// generated TOML. That is one hop short of the thing the contract is about —
// the text that reaches the runtime — and it would still pass if
// kyber-cron-postrun dropped or rewrote the value on the way through. So this
// test takes the Stop hook's command string VERBATIM out of the file the boot
// just wrote, runs it through the REAL images/agent-base/scripts/kyber-cron-postrun
// against an armed marker, and records what the clear command is finally
// invoked with.
//
// The environment deliberately does NOT set KYBER_CLEAR_SESSION_TEXT: the only
// way `/clear` can reach the recorder is inside the hook command string that
// start-codex.sh rendered.
//
// What this CANNOT prove is Codex's own semantics for that text — that `/clear`
// starts a fresh conversation while `/compact` carries a summary forward. That
// needs the real binary and a live turn; it was measured against 0.146.0 and
// the method and result are recorded in images/codex/INSTALL_NOTES.md.
func TestStartCodex_CronHooks_StopHookDeliversAFreshConversationCommand(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	realPostrun, err := os.ReadFile(filepath.Join(wd, "..", "agent-base", "scripts", "kyber-cron-postrun"))
	if err != nil {
		t.Fatalf("read the real postrun script: %v", err)
	}

	dir := t.TempDir()
	postrun := filepath.Join(dir, "kyber-cron-postrun")
	turnstart := filepath.Join(dir, "kyber-cron-turn-start")
	if err := os.WriteFile(postrun, realPostrun, 0o755); err != nil {
		t.Fatalf("install the real postrun: %v", err)
	}
	if err := os.WriteFile(turnstart, []byte("#!/usr/bin/env bash\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("install turn-start stub: %v", err)
	}

	managed := filepath.Join(dir, "etc", "managed_config.toml")
	out, err := runBoot(t, t.TempDir(), "", stubBin(t),
		"KYBER_CRON_POSTRUN_SENTINEL="+filepath.Join(dir, "run", "sentinel"),
		"KYBER_MANAGED_CODEX_CONFIG="+managed,
		"KYBER_CRON_POSTRUN_CMD="+postrun,
		"KYBER_CRON_TURNSTART_CMD="+turnstart,
	)
	if err != nil {
		t.Fatalf("boot failed: %v\n%s", err, out)
	}
	body, err := os.ReadFile(managed)
	if err != nil {
		t.Fatalf("managed config not written: %v\n%s", err, out)
	}
	hookCmd := stopHookCommand(t, string(body))

	// An armed marker for a job that asked for a clear — exactly what
	// kyber-job-dispatch writes and kyber-cron-turn-start arms.
	pending := filepath.Join(dir, "pending")
	if err := os.MkdirAll(pending, 0o755); err != nil {
		t.Fatalf("mkdir pending: %v", err)
	}
	marker := filepath.Join(pending, "nightly")
	if err := os.WriteFile(marker, []byte("state=armed\nclear_context=true\n"), 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	// The recorder stands in for kyber-compact-session, the last hop before
	// tmux pastes the text into the Codex pane.
	record := filepath.Join(dir, "delivered.txt")
	clearCmd := filepath.Join(dir, "clear-cmd")
	if err := os.WriteFile(clearCmd,
		[]byte("#!/usr/bin/env bash\nprintf '%s\\n' \"$@\" > "+record+"\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write recorder: %v", err)
	}

	logFile := filepath.Join(dir, "postrun.log")
	hook := exec.Command("/bin/sh", "-c", hookCmd)
	hook.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + dir,
		"KYBER_CRON_PENDING_DIR=" + pending,
		"KYBER_CRON_POSTRUN_LOG=" + logFile,
		"KYBER_CLEAR_SESSION_CMD=" + clearCmd,
	}
	if hookOut, err := hook.CombinedOutput(); err != nil {
		t.Fatalf("Stop hook command failed: %v\n%s", err, hookOut)
	}

	delivered, err := os.ReadFile(record)
	if err != nil {
		logBytes, _ := os.ReadFile(logFile)
		t.Fatalf("the Stop hook never reached the clear command: %v\nlog:\n%s", err, logBytes)
	}
	got := strings.TrimSpace(string(delivered))
	if got == "/compact" {
		t.Fatalf("the Stop hook delivers %q: compaction summarizes the thread and carries "+
			"the summary forward, so every later job still sees earlier runs — the exact "+
			"cross-contamination clearContextAfter exists to stop", got)
	}
	if got != "/clear" {
		t.Fatalf("the Stop hook delivers %q, want the fresh-conversation command %q", got, "/clear")
	}

	// The marker must be gone too, or --exclusive stays latched for this job.
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("armed marker survived the Stop hook (err=%v): exclusive stays latched", err)
	}
}

// runBootUmask is runBoot with a restrictive umask, modelling the real failure:
// in the pod the managed config is created through `sudo`, so it inherits
// root's umask rather than the agent user's. A 0077 umask reproduces that
// without needing root in CI.
func runBootUmask(t *testing.T, home, authJSON, path, umask string, extraEnv ...string) ([]byte, error) {
	t.Helper()
	persistRoot := t.TempDir()
	cmd := exec.Command("/bin/bash", "-c",
		"umask "+umask+"; exec /bin/bash "+scriptPath(t))
	env := []string{
		"HOME=" + home,
		"CODEX_HOME=" + filepath.Join(home, ".codex"),
		"KYBER_PERSIST_ROOT=" + persistRoot,
		"PATH=" + path,
		"SKIP_CODEX_LAUNCH=1",
		"AGENT_NAME=unit-test",
	}
	if authJSON != "" {
		env = append(env, "CODEX_AUTH_JSON="+authJSON)
	}
	env = append(env, extraEnv...)
	cmd.Env = env
	return cmd.CombinedOutput()
}

// The managed config and its directory must stay readable by the agent user.
// Created through sudo they inherit root's umask (0700/0600); codex then cannot
// traverse /etc/codex, its probe for requirements.toml returns EACCES rather
// than ENOENT, and it refuses to load ANY configuration — so every codex
// command fails and the agent crash-loops. Regression for kyber-canary
// 2026-08-28.
func TestStartCodexManagedConfigIsReadableByTheAgentUser(t *testing.T) {
	home := t.TempDir()
	managed := filepath.Join(t.TempDir(), "codex", "managed_config.toml")

	out, err := runBootUmask(t, home, secretCred, stubMCPBin(t), "077",
		"KYBER_MANAGED_CODEX_CONFIG="+managed)
	if err != nil {
		t.Fatalf("boot failed: %v\n%s", err, out)
	}

	fi, err := os.Stat(managed)
	if err != nil {
		t.Fatalf("managed config not written: %v", err)
	}
	if perm := fi.Mode().Perm(); perm&0o044 == 0 {
		t.Fatalf("managed config mode %o is not readable by the agent user", perm)
	}

	di, err := os.Stat(filepath.Dir(managed))
	if err != nil {
		t.Fatal(err)
	}
	// Needs r-x for others: without the x bit codex cannot even stat
	// requirements.toml, which is the failure that took canary down.
	if perm := di.Mode().Perm(); perm&0o005 != 0o005 {
		t.Fatalf("managed config dir mode %o is not traversable by the agent user", perm)
	}
}

// A failed redirection is reported by the SHELL, before the command's own
// 2>/dev/null applies, so probing writability with `: >> "$file"` leaked
// "Permission denied" into every boot log even when the sudo fallback then
// succeeded. Asserted against the script source rather than a boot run: the
// leak only appears when the target is root-owned, which a CI test cannot
// arrange, and a runtime test that cannot reproduce the bug is worse than
// none — it would report safety it never checked.
func TestStartCodexProbesWritabilityWithoutRedirecting(t *testing.T) {
	script, err := os.ReadFile(scriptPath(t))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(script), `: >> "$_target"`) {
		t.Fatal("start-codex.sh probes writability with a redirection again; " +
			"a failed redirect is the shell's own error and leaks past 2>/dev/null")
	}
	if !strings.Contains(string(script), "kyber_managed_config_writable") {
		t.Fatal("start-codex.sh no longer probes writability with a test builtin")
	}
}

// The managed config is best-effort: the launch command passes
// --ask-for-approval and --sandbox explicitly, so a settings file Kyber cannot
// write must degrade to a warning, never abort the boot.
//
// This covers the path where the location cannot be created at all. The
// narrower case — the initial write succeeds but a later append fails — cannot
// be arranged from outside the script, so the guard for it is asserted against
// the source in TestStartCodexAppendsToManagedConfigAreNonFatal.
func TestStartCodexSurvivesAnUnwritableManagedConfig(t *testing.T) {
	home := t.TempDir()
	// A path whose parent cannot be created: /proc rejects mkdir even for root.
	managed := "/proc/kyber-not-a-real-dir/managed_config.toml"

	out, err := runBoot(t, home, secretCred, stubMCPBin(t),
		"KYBER_MANAGED_CODEX_CONFIG="+managed,
		"CODEX_MODEL=claude-sonnet-5",
		"KYBER_TELEGRAM_MCP_URL=http://127.0.0.1:14004/mcp")
	if err != nil {
		t.Fatalf("boot aborted on an unwritable managed config: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "could not write") {
		t.Fatalf("boot did not warn about the unwritable managed config:\n%s", out)
	}
	// The agent's own config must still converge — the two are independent.
	config, err := os.ReadFile(filepath.Join(home, ".codex", "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(config), "[mcp_servers.kyber_telegram]") {
		t.Fatalf("MCP convergence was skipped because the managed config failed:\n%s", config)
	}
}

// Every append to the managed config must be `||`-guarded. The script runs
// under `set -euo pipefail`, so an unguarded failing pipeline exits the whole
// script — and for an agent that means it never starts. A chmod or sudo
// failure while recording the model or the cron hooks is worth a warning, not
// a dead agent.
//
// Asserted against the source because the failure needs a target that is
// writable for the first write and not for a later one, which a test cannot
// arrange from outside.
func TestStartCodexAppendsToManagedConfigAreNonFatal(t *testing.T) {
	script, err := os.ReadFile(scriptPath(t))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(script), "\n")
	found := 0
	for i, line := range lines {
		if !strings.Contains(line, "kyber_append_managed_config \"$KYBER_MANAGED_CODEX_CONFIG\"") {
			continue
		}
		found++
		guarded := strings.Contains(line, "||")
		if !guarded && i+1 < len(lines) {
			guarded = strings.HasPrefix(strings.TrimSpace(lines[i+1]), "||")
		}
		if !guarded {
			t.Fatalf("start-codex.sh:%d appends to the managed config without a `||` guard; "+
				"under set -e a failure here kills the whole boot:\n  %s", i+1, strings.TrimSpace(line))
		}
	}
	if found == 0 {
		t.Fatal("no managed-config append sites found — has the helper been renamed?")
	}
}

// An agent already poisoned by the 0700 /etc/codex from #160 must repair
// itself on the next boot — even though it still exits 42.
//
// That combination is the whole point. A 0700 managed-config directory stops
// codex loading ANY configuration, so `codex login status` fails, the
// device-auth branch runs, it cannot complete either, and the script exits 42.
// The full managed-settings write (which fixes the mode) lives far below that
// exit, so #165 repaired new agents and could never reach an already-broken
// one: every boot died before the chmod. /etc is on /persist under rootfs
// persistence, so the bad directory survives image updates and the agent stays
// broken forever.
//
// The repair therefore has to run before the first codex invocation, and this
// test pins that ordering by asserting the mode is fixed on a boot that fails.
func TestStartCodexRepairsAPoisonedManagedConfigBeforeGivingUp(t *testing.T) {
	managedDir := filepath.Join(t.TempDir(), "codex")
	if err := os.MkdirAll(managedDir, 0o700); err != nil {
		t.Fatal(err)
	}
	managed := filepath.Join(managedDir, "managed_config.toml")
	if err := os.WriteFile(managed, []byte("approval_policy = \"never\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Ends the auth session immediately with Kyber's marker still in place, so
	// the boot takes the exit-42 path — exactly what a poisoned agent does.
	path, _, donePath := stubDeviceAuthBin(t)
	if err := os.WriteFile(donePath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	stubDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(stubDir, "tmux"), []byte(`#!/usr/bin/env bash
case "${1:-}" in
  new-session) exit 0 ;;
  has-session) exit 1 ;;
  *) exit 0 ;;
esac
`), 0o755); err != nil {
		t.Fatal(err)
	}

	out, err := runBoot(t, t.TempDir(), `{}`, stubDir+":"+path,
		"KYBER_MANAGED_CODEX_CONFIG="+managed)
	if err == nil {
		t.Fatalf("expected the exit-42 path for this fixture:\n%s", out)
	}

	di, statErr := os.Stat(managedDir)
	if statErr != nil {
		t.Fatal(statErr)
	}
	if perm := di.Mode().Perm(); perm&0o005 != 0o005 {
		t.Fatalf("a boot that exits 42 left the managed config dir at %o — "+
			"a poisoned agent can never repair itself:\n%s", perm, out)
	}
	fi, statErr := os.Stat(managed)
	if statErr != nil {
		t.Fatal(statErr)
	}
	if perm := fi.Mode().Perm(); perm&0o044 == 0 {
		t.Fatalf("managed config left at %o, unreadable by the agent user", perm)
	}
}

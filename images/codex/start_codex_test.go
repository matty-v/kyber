//go:build integration

// Package startcodexshell invokes start-codex.sh from Go to verify boot-time
// credential handling. The //go:build tag keeps it out of the default
// `go test ./...` run; run with `go test ./images/codex/ -tags=integration`.
package startcodexshell_test

import (
	"os"
	"os/exec"
	"path/filepath"
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
case "$*" in
  *"@openai/codex@latest"*) printf '%s' '0.149.0' > "$VERSION_FILE" ;;
  *) requested="${*: -1}"; printf '%s' "${requested##*@}" > "$VERSION_FILE" ;;
esac`)
	write("sudo", `printf '%s\n' "$*" >> "$SUDO_LOG"; exec "$@"`)
	write("curl", `exit 0`)
	write("tmux", `exit 0`)
	return dir + ":" + os.Getenv("PATH"), sudoLog, npmLog
}

func TestStartCodexLatestInstallsAsRootOnEveryBoot(t *testing.T) {
	path, sudoLog, npmLog := stubVersionInstallBin(t, "0.146.0")
	home := t.TempDir()
	out, err := runBoot(t, home, "", path,
		"KYBER_REQUESTED_CODEX_VERSION=latest",
		"VERSION_FILE="+filepath.Join(strings.Split(path, ":")[0], "codex.version"),
		"SUDO_LOG="+sudoLog,
		"NPM_LOG="+npmLog,
	)
	if err != nil {
		t.Fatalf("latest boot failed: %v\n%s", err, out)
	}
	for _, logPath := range []string{sudoLog, npmLog} {
		got, err := os.ReadFile(logPath)
		if err != nil {
			t.Fatalf("read %s: %v", logPath, err)
		}
		if !strings.Contains(string(got), "install -g @openai/codex@latest") {
			t.Fatalf("install log %q does not contain latest global install", got)
		}
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
	out, err := runBoot(t, home, secretCred, stubBin(t))
	if err != nil {
		t.Fatalf("boot failed: %v\n%s", err, out)
	}
	config, err := os.ReadFile(filepath.Join(home, ".codex", "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(config), "check_for_update_on_startup = false") {
		t.Fatalf("config.toml does not disable Codex's self-update prompt:\n%s", config)
	}
}

func TestStartCodexRegistersDiscordMCP(t *testing.T) {
	script, err := os.ReadFile(scriptPath(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"[mcp_servers.kyber_discord]", `url = "${KYBER_DISCORD_MCP_URL}"`} {
		if !strings.Contains(string(script), want) {
			t.Fatalf("start-codex.sh missing Discord MCP registration %q", want)
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
// command is captured BEFORE the startup prompt joins CODEX_ARGS (a resumed
// session must not receive the new-session prompt), and boot gates on the
// enable flag plus a recorded session.
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
	if resumeDef > promptAppend {
		t.Fatal("CODEX_RESUME_CMD is built AFTER the startup prompt joins CODEX_ARGS — a resumed session would replay the new-session prompt")
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
//	bare + recorded session     -> `codex resume --last` (no prompt)
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
		`CODEX_RESUME_CMD='codex resume --last --model gpt-test --ask-for-approval never --sandbox danger-full-access'`,
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
	} else if strings.Contains(got, "startup") {
		t.Errorf("resume launch must not carry the startup prompt, got:\n%s", got)
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

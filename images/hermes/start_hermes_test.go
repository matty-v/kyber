//go:build integration

package hermes_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const yamlShim = `
import json
def safe_load(value):
    return json.loads(value)
def safe_dump(value, handle, sort_keys=False):
    json.dump(value, handle, indent=2)
`

func scriptPath(t *testing.T, name string) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(wd, name)
}

func TestConfigureHermesConvergesManagedSettings(t *testing.T) {
	home := t.TempDir()
	config := map[string]any{
		"display": map[string]any{"streaming": true},
		"mcp_servers": map[string]any{
			"operator_owned": map[string]any{"url": "https://example.invalid/mcp"},
			"kyber_slack":    map[string]any{"url": "http://stale.invalid/mcp"},
		},
	}
	data, _ := json.Marshal(config)
	if err := os.WriteFile(filepath.Join(home, "config.yaml"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	shim := t.TempDir()
	if err := os.WriteFile(filepath.Join(shim, "yaml.py"), []byte(yamlShim), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("python3", scriptPath(t, "configure-hermes.py"))
	cmd.Env = append(os.Environ(),
		"HERMES_HOME="+home,
		"PYTHONPATH="+shim,
		"KYBER_TELEGRAM_MCP_URL=http://127.0.0.1:14004/mcp",
		"KYBER_REQUEST_MCP_URL=http://127.0.0.1:8091/mcp",
		"KYBER_SLACK_MCP_URL=",
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("configure Hermes: %v\n%s", err, output)
	}
	var got map[string]any
	data, err := os.ReadFile(filepath.Join(home, "config.yaml"))
	if err != nil || json.Unmarshal(data, &got) != nil {
		t.Fatalf("read config: %v, body=%s", err, data)
	}
	if got["display"].(map[string]any)["streaming"] != true {
		t.Fatalf("operator config was not preserved: %+v", got)
	}
	servers := got["mcp_servers"].(map[string]any)
	if _, ok := servers["operator_owned"]; !ok {
		t.Fatalf("operator MCP server was removed: %+v", servers)
	}
	if _, ok := servers["kyber_slack"]; ok {
		t.Fatalf("disabled managed server remains: %+v", servers)
	}
	if servers["kyber_telegram"].(map[string]any)["url"] != "http://127.0.0.1:14004/mcp" ||
		servers["kyber_request_reply"].(map[string]any)["url"] != "http://127.0.0.1:8091/mcp" {
		t.Fatalf("managed MCP servers = %+v", servers)
	}
	background := got["auxiliary"].(map[string]any)["background_review"].(map[string]any)
	if background["enabled"] != false {
		t.Fatalf("background review was not disabled: %+v", background)
	}
}

func TestStartHermesWritesSafeFreshAndResumeLaunches(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	home := filepath.Join(root, "home")
	persist := filepath.Join(root, "persist")
	for _, path := range []string{bin, home, persist} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeExecutable(t, filepath.Join(bin, "hermes"), "#!/usr/bin/env bash\necho 'Hermes 0.21.0'\n")
	writeExecutable(t, filepath.Join(bin, "configure"), "#!/usr/bin/env bash\nexit 0\n")
	identity := filepath.Join(root, "identity.sh")
	if err := os.WriteFile(identity, []byte("REPO_DIR=\"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	prompt := `review $(touch /tmp/hermes-start-test-injection)`
	cmd := exec.Command("bash", scriptPath(t, "start-hermes.sh"))
	cmd.Env = append(os.Environ(),
		"PATH="+bin+":"+os.Getenv("PATH"),
		"HOME="+home,
		"HERMES_HOME="+filepath.Join(home, ".hermes"),
		"KYBER_PERSIST_ROOT="+persist,
		"OPENROUTER_API_KEY=test-only",
		"HERMES_INFERENCE_MODEL=anthropic/claude-sonnet-4.6",
		"KYBER_STARTUP_PROMPT="+prompt,
		"KYBER_SESSION_RESUME=true",
		"KYBER_HERMES_CONFIGURATOR="+filepath.Join(bin, "configure"),
		"KYBER_IDENTITY_REPO_SCRIPT="+identity,
		"SKIP_HERMES_LAUNCH=1",
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("start Hermes: %v\n%s", err, output)
	}
	launch, err := os.ReadFile(filepath.Join(persist, "last-hermes-launch.sh"))
	if err != nil {
		t.Fatal(err)
	}
	body := string(launch)
	for _, want := range []string{"--fresh", "--continue", "anthropic/claude-sonnet-4.6", `/tmp/hermes-start-test-injection`} {
		if !strings.Contains(body, want) {
			t.Errorf("launch script missing %q:\n%s", want, body)
		}
	}
	if _, err := os.Stat("/tmp/hermes-start-test-injection"); !os.IsNotExist(err) {
		t.Fatal("startup prompt executed as shell code")
	}
}

func TestStartHermesRejectsMissingCredential(t *testing.T) {
	root := t.TempDir()
	cmd := exec.Command("bash", scriptPath(t, "start-hermes.sh"))
	cmd.Env = append(os.Environ(), "HOME="+root, "KYBER_PERSIST_ROOT="+filepath.Join(root, "persist"), "OPENROUTER_API_KEY=", "SKIP_HERMES_LAUNCH=1")
	err := cmd.Run()
	exit, ok := err.(*exec.ExitError)
	if !ok || exit.ExitCode() != 42 {
		t.Fatalf("exit = %v, want status 42", err)
	}
}

func writeExecutable(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

// runConfigurator runs configure-hermes.py against a temp HERMES_HOME seeded
// with the given config, and returns the converged result.
func runConfigurator(t *testing.T, seed map[string]any, env ...string) map[string]any {
	t.Helper()
	home := t.TempDir()
	if seed != nil {
		data, _ := json.Marshal(seed)
		if err := os.WriteFile(filepath.Join(home, "config.yaml"), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	shim := t.TempDir()
	if err := os.WriteFile(filepath.Join(shim, "yaml.py"), []byte(yamlShim), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("python3", scriptPath(t, "configure-hermes.py"))
	cmd.Env = append(os.Environ(), append([]string{"HERMES_HOME=" + home, "PYTHONPATH=" + shim}, env...)...)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("configure Hermes: %v\n%s", err, output)
	}
	data, err := os.ReadFile(filepath.Join(home, "config.yaml"))
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("parse config: %v, body=%s", err, data)
	}
	return got
}

// The providers block is what Hermes resolves `--provider` against. Without
// it, an agent pointed at its own endpoint looks up a provider that does not
// exist.
func TestConfigureHermesWritesTheInferenceProvider(t *testing.T) {
	got := runConfigurator(t, map[string]any{"display": map[string]any{"streaming": true}},
		"KYBER_INFERENCE_PROVIDER=kyber-endpoint",
		"KYBER_INFERENCE_BASE_URL=https://llm.example.com/v1",
		"HERMES_INFERENCE_MODEL=qwen3.6-35b-a3b",
	)
	providers, ok := got["providers"].(map[string]any)
	if !ok {
		t.Fatalf("no providers block written: %+v", got)
	}
	entry, ok := providers["kyber-endpoint"].(map[string]any)
	if !ok {
		t.Fatalf("managed provider missing: %+v", providers)
	}
	if entry["base_url"] != "https://llm.example.com/v1" {
		t.Errorf("base_url = %v", entry["base_url"])
	}
	if entry["model"] != "qwen3.6-35b-a3b" {
		t.Errorf("model = %v", entry["model"])
	}
	// The credential is referenced, never inlined — the adapter injects the
	// value as an env var from the operator's Secret.
	if entry["api_key"] != "${OPENAI_API_KEY}" {
		t.Errorf("api_key = %v, want the env reference", entry["api_key"])
	}
	if got["display"].(map[string]any)["streaming"] != true {
		t.Errorf("operator config was not preserved: %+v", got)
	}
}

// Clearing spec.inference must actually move the agent back to its built-in
// provider. The adapter stops setting KYBER_INFERENCE_PROVIDER entirely when
// the field is cleared, so the removal cannot depend on that env being set.
func TestConfigureHermesRemovesTheInferenceProviderWhenCleared(t *testing.T) {
	seed := map[string]any{
		"providers": map[string]any{
			"kyber-endpoint": map[string]any{"base_url": "https://old.invalid/v1"},
			"operator_owned": map[string]any{"base_url": "https://keep.invalid/v1"},
		},
	}
	got := runConfigurator(t, seed)

	providers, ok := got["providers"].(map[string]any)
	if !ok {
		t.Fatalf("operator providers were dropped entirely: %+v", got)
	}
	if _, stale := providers["kyber-endpoint"]; stale {
		t.Errorf("stale managed provider survived: %+v", providers)
	}
	if _, kept := providers["operator_owned"]; !kept {
		t.Errorf("operator-owned provider was removed: %+v", providers)
	}
}

// With no managed provider and nothing operator-owned, the key is dropped
// rather than left as an empty map.
func TestConfigureHermesOmitsAnEmptyProvidersBlock(t *testing.T) {
	got := runConfigurator(t, nil)
	if _, present := got["providers"]; present {
		t.Errorf("empty providers block was written: %+v", got)
	}
}

// The goal hook must be registered so a revision is opened at the start of
// each turn; without it no Hermes agent shows a goal at all.
func TestConfigureHermesRegistersTheGoalHook(t *testing.T) {
	// The configurator only registers the hook when the command is executable,
	// so stand in a fake at the real path via a bind of the check: point the
	// script at a temp command through the module-level constant is not
	// possible, so assert the absence path here and the presence path below
	// using the real image path when it exists.
	got := runConfigurator(t, nil)
	hooks, present := got["hooks"]
	if _, realScript := os.Stat("/usr/local/bin/kyber-hermes-goal-start"); realScript == nil {
		if !present {
			t.Fatalf("goal hook missing while the script is installed: %+v", got)
		}
		entries, _ := hooks.(map[string]any)["pre_llm_call"].([]any)
		if len(entries) == 0 {
			t.Fatalf("pre_llm_call hook not registered: %+v", hooks)
		}
	} else if present {
		// No script on this machine: the block must not be written at all,
		// or Hermes would try to run a command that does not exist on every
		// single model call.
		if _, dangling := hooks.(map[string]any)["pre_llm_call"]; dangling {
			t.Fatalf("dangling goal hook written with no script installed: %+v", hooks)
		}
	}
}

// An operator's own hooks must survive, and a managed hook must not be
// duplicated when the configurator runs again on every boot.
func TestConfigureHermesGoalHookIsIdempotentAndPreservesOperatorHooks(t *testing.T) {
	seed := map[string]any{
		"hooks": map[string]any{
			"pre_llm_call": []any{
				map[string]any{"command": "/opt/operator/my-hook", "timeout": 9},
				map[string]any{"command": "/usr/local/bin/kyber-hermes-goal-start", "timeout": 5},
			},
			"post_tool_call": []any{
				map[string]any{"command": "/opt/operator/after", "timeout": 3},
			},
		},
	}
	got := runConfigurator(t, seed)
	hooks, _ := got["hooks"].(map[string]any)
	if hooks == nil {
		t.Fatalf("operator hooks were dropped entirely: %+v", got)
	}
	if _, kept := hooks["post_tool_call"]; !kept {
		t.Errorf("an unrelated operator hook event was removed: %+v", hooks)
	}
	entries, _ := hooks["pre_llm_call"].([]any)
	managed, operator := 0, 0
	for _, e := range entries {
		switch e.(map[string]any)["command"] {
		case "/usr/local/bin/kyber-hermes-goal-start":
			managed++
		case "/opt/operator/my-hook":
			operator++
		}
	}
	if operator != 1 {
		t.Errorf("operator hook count = %d, want 1: %+v", operator, entries)
	}
	if managed > 1 {
		t.Errorf("managed goal hook duplicated %d times across boots: %+v", managed, entries)
	}
}

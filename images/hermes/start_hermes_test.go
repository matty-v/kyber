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

// The goal hook must be registered, or no Hermes agent shows a goal at all.
// KYBER_HERMES_GOAL_HOOK_COMMAND points the configurator at a real executable
// in the test's temp dir: the production default is an absolute path that does
// not exist on CI, so without this the presence branch asserted nothing — the
// whole convergence block could be deleted and the test still passed.
func TestConfigureHermesRegistersTheGoalHook(t *testing.T) {
	hookPath := filepath.Join(t.TempDir(), "kyber-hermes-goal-start")
	if err := os.WriteFile(hookPath, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	got := runConfigurator(t, nil, "KYBER_HERMES_GOAL_HOOK_COMMAND="+hookPath)
	hooks, ok := got["hooks"].(map[string]any)
	if !ok {
		t.Fatalf("no hooks block written: %+v", got)
	}
	entries, _ := hooks["pre_llm_call"].([]any)
	if len(entries) != 1 {
		t.Fatalf("pre_llm_call entries = %+v, want exactly the managed hook", entries)
	}
	if entries[0].(map[string]any)["command"] != hookPath {
		t.Errorf("registered command = %v, want %s", entries[0], hookPath)
	}
}

// With no executable at the configured path the block must not be written at
// all, or Hermes would try to run a missing command on every model call.
func TestConfigureHermesOmitsTheGoalHookWhenTheScriptIsAbsent(t *testing.T) {
	got := runConfigurator(t, nil,
		"KYBER_HERMES_GOAL_HOOK_COMMAND="+filepath.Join(t.TempDir(), "absent"))
	if _, present := got["hooks"]; present {
		t.Errorf("hooks block written with no script installed: %+v", got["hooks"])
	}
}

// An operator's own hooks must survive, and the managed entry must not pile up
// across the boots that rewrite this file every time.
func TestConfigureHermesGoalHookIsIdempotentAndPreservesOperatorHooks(t *testing.T) {
	hookPath := filepath.Join(t.TempDir(), "kyber-hermes-goal-start")
	if err := os.WriteFile(hookPath, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	seed := map[string]any{
		"hooks": map[string]any{
			"pre_llm_call": []any{
				map[string]any{"command": "/opt/operator/my-hook", "timeout": 9},
				map[string]any{"command": hookPath, "timeout": 5},
			},
			"post_tool_call": []any{
				map[string]any{"command": "/opt/operator/after", "timeout": 3},
			},
		},
	}
	got := runConfigurator(t, seed, "KYBER_HERMES_GOAL_HOOK_COMMAND="+hookPath)
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
		case hookPath:
			managed++
		case "/opt/operator/my-hook":
			operator++
		}
	}
	if operator != 1 || managed != 1 {
		t.Errorf("operator=%d managed=%d, want 1 and 1: %+v", operator, managed, entries)
	}
}

// `pre_llm_call:` with nothing under it is ordinary YAML and parses to None.
// Iterating that raised TypeError, and start-hermes.sh treats a non-zero
// configurator exit as FATAL — so a config-shaped input stopped the agent
// booting at all.
func TestConfigureHermesSurvivesAnEmptyHookEvent(t *testing.T) {
	hookPath := filepath.Join(t.TempDir(), "kyber-hermes-goal-start")
	if err := os.WriteFile(hookPath, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	got := runConfigurator(t, map[string]any{"hooks": map[string]any{"pre_llm_call": nil}},
		"KYBER_HERMES_GOAL_HOOK_COMMAND="+hookPath)
	entries, _ := got["hooks"].(map[string]any)["pre_llm_call"].([]any)
	if len(entries) != 1 {
		t.Fatalf("entries = %+v, want the managed hook", entries)
	}
}

// A single un-listed mapping is also valid YAML. Iterating it yielded its
// string keys, every one failed the isinstance check, and the operator's hook
// was silently deleted.
func TestConfigureHermesPreservesAnUnlistedOperatorHook(t *testing.T) {
	hookPath := filepath.Join(t.TempDir(), "kyber-hermes-goal-start")
	if err := os.WriteFile(hookPath, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	seed := map[string]any{"hooks": map[string]any{
		"pre_llm_call": map[string]any{"command": "/opt/operator/single", "timeout": 3},
	}}
	got := runConfigurator(t, seed, "KYBER_HERMES_GOAL_HOOK_COMMAND="+hookPath)
	entries, _ := got["hooks"].(map[string]any)["pre_llm_call"].([]any)
	found := false
	for _, e := range entries {
		if e.(map[string]any)["command"] == "/opt/operator/single" {
			found = true
		}
	}
	if !found {
		t.Errorf("an un-listed operator hook was destroyed: %+v", entries)
	}
}

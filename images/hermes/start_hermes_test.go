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

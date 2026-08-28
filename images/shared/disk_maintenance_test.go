package identityreposhared

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRuntimeLaunchersPauseForDiskMaintenance(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Clean(filepath.Join(wd, "..", ".."))
	for _, script := range []string{
		"images/claude-code/start-claude.sh",
		"images/codex/start-codex.sh",
	} {
		body, err := os.ReadFile(filepath.Join(root, script))
		if err != nil {
			t.Fatal(err)
		}
		text := string(body)
		for _, want := range []string{
			"DISK_EXHAUSTED_MARKER=/var/run/kyber/disk-exhausted",
			`tmux kill-session -t agent`,
			`while [ -f "$DISK_EXHAUSTED_MARKER" ]`,
			"relaunch_count=0",
		} {
			if !strings.Contains(text, want) {
				t.Errorf("%s lacks disk-maintenance behavior %q", script, want)
			}
		}
	}
}

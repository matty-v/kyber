package identityreposhared

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTelegramSkillPackagedForBothRuntimes(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Clean(filepath.Join(wd, "..", ".."))
	for _, dockerfile := range []string{"images/codex/Dockerfile", "images/claude-code/Dockerfile"} {
		body, err := os.ReadFile(filepath.Join(root, dockerfile))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(body), "images/shared/skills/telegram-messaging") {
			t.Errorf("%s does not package the Telegram skill", dockerfile)
		}
	}
	for _, script := range []string{"images/codex/start-codex.sh", "images/claude-code/start-claude.sh"} {
		body, err := os.ReadFile(filepath.Join(root, script))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(body), `KYBER_TELEGRAM_MCP_URL`) || !strings.Contains(string(body), "telegram-messaging") {
			t.Errorf("%s does not conditionally install the Telegram skill", script)
		}
	}
}

func TestDiscordSkillPackagedForBothRuntimes(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Clean(filepath.Join(wd, "..", ".."))
	for _, dockerfile := range []string{"images/codex/Dockerfile", "images/claude-code/Dockerfile"} {
		body, err := os.ReadFile(filepath.Join(root, dockerfile))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(body), "images/shared/skills/discord-messaging") {
			t.Errorf("%s does not package the Discord skill", dockerfile)
		}
	}
	for _, script := range []string{"images/codex/start-codex.sh", "images/claude-code/start-claude.sh"} {
		body, err := os.ReadFile(filepath.Join(root, script))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(body), `KYBER_DISCORD_MCP_URL`) || !strings.Contains(string(body), "discord-messaging") {
			t.Errorf("%s does not conditionally install the Discord skill", script)
		}
	}
}

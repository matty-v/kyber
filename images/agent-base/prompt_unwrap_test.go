package agent_base_test

import (
	"os/exec"
	"strings"
	"testing"
)

func runPromptUnwrap(t *testing.T, in string) string {
	t.Helper()
	cmd := exec.Command("bash", "scripts/kyber-prompt-unwrap")
	cmd.Stdin = strings.NewReader(in)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("kyber-prompt-unwrap: %v", err)
	}
	return string(out)
}

// The wrapped form is copied from a real Claude Code UserPromptSubmit payload
// (2.1.285, a multi-line bracketed paste): two blank lines, the opener, the
// pasted text, the closer, a trailing newline.
func TestPromptUnwrap(t *testing.T) {
	body := "[kyber-task:task_11111111111111111111111111111111] attempt=attempt_22222222222222222222222222222222\nagent: jack\ntask:\n  id: task_11111111111111111111111111111111"
	cases := []struct {
		name, in, want string
	}{
		{"claude code wrapper", "\n\n<pasted_content id=\"aea0\">\n" + body + "\n</pasted_content id=\"aea0\">\n", body},
		{"wrapper without leading blank lines", "<pasted_content id=\"2b5a\">\n" + body + "\n</pasted_content id=\"2b5a\">", body},
		{"plain prompt is unchanged", body, body},
		{"short single line is unchanged", "run your work tick", "run your work tick"},
		{"leading blank lines without a wrapper are kept", "\n\nhello", "\n\nhello"},
		{"opener without closer is unchanged", "\n\n<pasted_content id=\"aea0\">\n" + body, "\n\n<pasted_content id=\"aea0\">\n" + body},
		{"mismatched closer is unchanged", "<pasted_content id=\"aaaa\">\n" + body + "\n</pasted_content id=\"bbbb\">", "<pasted_content id=\"aaaa\">\n" + body + "\n</pasted_content id=\"bbbb\">"},
		{"unknown wrapper tag is unchanged", "<pasted_text id=\"aaaa\">\n" + body + "\n</pasted_text id=\"aaaa\">", "<pasted_text id=\"aaaa\">\n" + body + "\n</pasted_text id=\"aaaa\">"},
		{"only one layer is removed", "<pasted_content id=\"a1\">\n<pasted_content id=\"b2\">\nx\n</pasted_content id=\"b2\">\n</pasted_content id=\"a1\">", "<pasted_content id=\"b2\">\nx\n</pasted_content id=\"b2\">"},
		{"empty wrapper", "<pasted_content id=\"a1\">\n</pasted_content id=\"a1\">\n", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := runPromptUnwrap(t, c.in); got != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
}

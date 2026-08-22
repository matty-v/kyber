package modelprobe

import "testing"

func TestClassify(t *testing.T) {
	tests := []struct {
		name   string
		exit   int
		output string
		want   Outcome
	}{
		{
			// Captured verbatim from claude CLI on 2026-08-22 with a
			// nonexistent model — the exact output that previously fell
			// through the script heuristic (stdout was discarded and the
			// phrasing matched nothing).
			name: "current CLI rejection phrasing",
			exit: 1,
			output: "There's an issue with the selected model (claude-opus-4-canary-marker). " +
				"It may not exist or you may not have access to it. Run --model to pick a different model.",
			want: OutcomeUnsupported,
		},
		{
			// The stderr the same run produced — a deprecation warning
			// alone must not read as a rejection when exit is 0.
			name:   "success with deprecation warning on stderr",
			exit:   0,
			output: "⚠ Claude Opus 4 was retired on June 15, 2026. Consider switching to a newer model.",
			want:   OutcomeSupported,
		},
		{name: "clean success", exit: 0, output: "pong", want: OutcomeSupported},
		{name: "timeout", exit: 124, output: "", want: OutcomeInconclusive},
		{name: "legacy phrasing keyword-first", exit: 1, output: "Error: unsupported model claude-x", want: OutcomeUnsupported},
		{name: "legacy phrasing model-first", exit: 1, output: "error: model claude-x is not found", want: OutcomeUnsupported},
		{name: "no such model", exit: 1, output: "no such model: claude-x", want: OutcomeUnsupported},
		{
			name:   "auth failure stays inconclusive",
			exit:   1,
			output: "Invalid bearer token. Please run /login.",
			want:   OutcomeInconclusive,
		},
		{
			name:   "network failure stays inconclusive",
			exit:   1,
			output: "fetch failed: getaddrinfo ENOTFOUND api.anthropic.com",
			want:   OutcomeInconclusive,
		},
		{name: "nonzero with empty output", exit: 1, output: "", want: OutcomeInconclusive},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Classify(tt.exit, tt.output); got != tt.want {
				t.Fatalf("Classify(%d, %q) = %q, want %q", tt.exit, tt.output, got, tt.want)
			}
		})
	}
}

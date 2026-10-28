package reasoningeffort

import "testing"

func TestExactTupleAndClearing(t *testing.T) {
	for _, tc := range []struct {
		name, runtime, version, model, effort string
		custom, want                          bool
	}{
		{"opus xhigh", "claude-code", "2.1.119", "claude-opus-4-7", "xhigh", false, true},
		{"sonnet xhigh", "claude-code", "2.1.119", "claude-sonnet-4-6", "xhigh", false, false},
		{"codex ultra", "codex", "0.146.0", "gpt-5.6-sol", "ultra", false, true},
		{"luna ultra", "codex", "0.146.0", "gpt-5.6-luna", "ultra", false, false},
		{"other version", "codex", "0.146.1", "gpt-5.6-sol", "high", false, false},
		{"other model", "codex", "0.146.0", "new-model", "high", false, false},
		{"other route", "codex", "0.146.0", "gpt-5.6-sol", "high", true, false},
		{"other runtime", "hermes", "0.146.0", "gpt-5.6-sol", "high", false, false},
		{"clear unknown", "hermes", "unknown", "unknown", "", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := Validate(tc.runtime, tc.version, tc.model, tc.custom, tc.effort); (err == nil) != tc.want {
				t.Fatalf("validation=%v want=%v", err, tc.want)
			}
		})
	}
	// Callers cannot mutate a later response's allowed values.
	a := Lookup("codex", "0.146.0", "gpt-5.6-sol", false)
	a.Levels[0] = "bad"
	if Lookup("codex", "0.146.0", "gpt-5.6-sol", false).Levels[0] != "low" {
		t.Fatal("shared mutable policy")
	}
}

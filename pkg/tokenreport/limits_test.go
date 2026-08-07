package tokenreport_test

import (
	"testing"

	"github.com/matty-v/kyber/pkg/tokenreport"
)

func TestLimitFor(t *testing.T) {
	cases := []struct {
		name   string
		model  string
		expect int64
	}{
		{"opus 4.7 full name", "claude-opus-4-7", 1_000_000},
		{"opus 4.6 full name", "claude-opus-4-6", 1_000_000},
		{"opus alias", "opus", 1_000_000},
		{"sonnet 4.5", "claude-sonnet-4-5", 200_000},
		{"sonnet alias", "sonnet", 200_000},
		{"haiku 4.5", "claude-haiku-4-5", 200_000},
		{"haiku alias", "haiku", 200_000},
		{"unknown model", "claude-future-5-0", 200_000},
		{"empty string", "", 200_000},
		// Claude Code emits the "[1m]" suffix on transcripts when the 1M
		// variant is invoked (e.g. `--model claude-opus-4-7[1m]`). The
		// lookup must treat the suffixed form as the same model family.
		{"opus 4.7 with 1m suffix", "claude-opus-4-7[1m]", 1_000_000},
		{"sonnet 4.6 with 1m suffix", "claude-sonnet-4-6[1m]", 1_000_000},
		{"opus 4.5 with 1m suffix (unlisted variant, returns base entry)", "claude-opus-4-5[1m]", 200_000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tokenreport.LimitFor(tc.model)
			if got != tc.expect {
				t.Errorf("LimitFor(%q) = %d, want %d", tc.model, got, tc.expect)
			}
		})
	}
}

// LimitForKnown is LimitFor with an explicit "known" signal — true only when
// the model resolved from the knownModels table or a family alias, false when
// it fell through to the 200K floor. kyber#500 needs the bool so the
// serve-time gauge can mark an unknown window as an estimate (the same
// contextWindowKnown semantics the snapshot/ConfigMap paths use).
func TestLimitForKnown(t *testing.T) {
	cases := []struct {
		name      string
		model     string
		wantLimit int64
		wantKnown bool
	}{
		{"known concrete 1M", "claude-opus-4-7", 1_000_000, true},
		{"known concrete 200K", "claude-opus-4-5", 200_000, true},
		{"known alias", "opus", 1_000_000, true},
		{"known with 1m suffix", "claude-sonnet-4-6[1m]", 1_000_000, true},
		{"unknown floors, not known", "claude-opus-4-8", 200_000, false},
		{"unknown future model floors", "claude-future-5-0", 200_000, false},
		{"empty floors, not known", "", 200_000, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotLimit, gotKnown := tokenreport.LimitForKnown(tc.model)
			if gotLimit != tc.wantLimit || gotKnown != tc.wantKnown {
				t.Errorf("LimitForKnown(%q) = (%d, %v), want (%d, %v)", tc.model, gotLimit, gotKnown, tc.wantLimit, tc.wantKnown)
			}
		})
	}
}

func TestModels_OrderedNewestFirst(t *testing.T) {
	got := tokenreport.Models()
	if len(got) == 0 {
		t.Fatalf("Models() returned empty slice")
	}

	// Contract: aliases must never leak into the public list.
	for _, m := range got {
		for _, alias := range []string{"opus", "sonnet", "haiku"} {
			if m.ID == alias {
				t.Errorf("Models() must not include alias %q", alias)
			}
		}
	}

	// Contract: claude-opus-4-7 is present with 1M context window.
	var opus47 *tokenreport.Model
	for i := range got {
		if got[i].ID == "claude-opus-4-7" {
			opus47 = &got[i]
			break
		}
	}
	if opus47 == nil {
		t.Fatalf("Models() missing claude-opus-4-7")
	}
	if opus47.ContextWindow != 1_000_000 {
		t.Errorf("claude-opus-4-7 context window = %d, want 1_000_000", opus47.ContextWindow)
	}

	// Contract: first entry is the newest opus (opus-4-7), so UIs defaulting
	// to Models()[0] get the most current model without extra logic.
	if got[0].ID != "claude-opus-4-7" {
		t.Errorf("Models()[0] = %q, want claude-opus-4-7", got[0].ID)
	}
}

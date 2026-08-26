package codexauth

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The real pane, captured from a Codex 0.146.0 agent on the dev instance,
// 2026-08-26 — banner and all, because that banner is what the operator's pane
// actually contains and a parser that only handles the tidy excerpt is not
// tested against reality.
const livePane = `[Kyber agent shell — debug helpers]

  agent          attach to the agent runtime's tmux session (read/write)
  peek           attach read-only — safer if the agent is mid-task
  restart-agent  kill the tmux session — pod will restart cleanly

Pod runs the selected agent runtime as the 'kyber' user in tmux session 'agent'.


Welcome to Codex [v0.146.0]
OpenAI's command-line coding agent

Follow these steps to sign in with ChatGPT using device code authorization:

1. Open this link in your browser and sign in to your account
   https://auth.openai.com/codex/device

2. Enter this one-time code (expires in 15 minutes)
   E7OV-KG840

Continue only if you started this login in Codex. If a website or another person gave you this code, cancel.
`

func TestParse_LivePane(t *testing.T) {
	start := time.Date(2026, 8, 26, 20, 0, 0, 0, time.UTC)
	got := Parse(livePane, start, start.Add(time.Minute))

	if got.State != StateReady {
		t.Fatalf("state=%q, want %q", got.State, StateReady)
	}
	if got.UserCode != "E7OV-KG840" {
		t.Errorf("userCode=%q, want E7OV-KG840", got.UserCode)
	}
	if got.VerificationURL != "https://auth.openai.com/codex/device" {
		t.Errorf("verificationUrl=%q", got.VerificationURL)
	}
	if want := start.Add(15 * time.Minute); !got.ExpiresAt.Equal(want) {
		t.Errorf("expiresAt=%v, want %v", got.ExpiresAt, want)
	}
}

func TestParse_ExpiredOncePastTheWindow(t *testing.T) {
	start := time.Date(2026, 8, 26, 20, 0, 0, 0, time.UTC)
	got := Parse(livePane, start, start.Add(16*time.Minute))
	if got.State != StateExpired {
		t.Fatalf("state=%q, want %q", got.State, StateExpired)
	}
	// Still carries the code — the panel names what expired rather than
	// blanking out, and "Start again" is the operator's move either way.
	if got.UserCode == "" {
		t.Error("an expired result should still report the code it is talking about")
	}
}

// The pane between session start and the prompt being drawn. This is the
// common case for the first second or two of every login, and it must read as
// "still starting" so the panel holds its spinner.
func TestParse_BannerOnlyIsStarting(t *testing.T) {
	got := Parse("[Kyber agent shell — debug helpers]\n\n  agent   attach\n", time.Now(), time.Now())
	if got.State != StateStarting {
		t.Fatalf("state=%q, want %q", got.State, StateStarting)
	}
}

func TestParse_EmptyPaneIsStarting(t *testing.T) {
	if got := Parse("   \n\t\n", time.Time{}, time.Time{}); got.State != StateStarting {
		t.Fatalf("state=%q, want %q", got.State, StateStarting)
	}
}

// A URL with no code, or a code with no URL, is a half-drawn prompt. Reporting
// either as ready would put an incomplete instruction in front of the operator.
func TestParse_HalfDrawnPromptIsStarting(t *testing.T) {
	urlOnly := "1. Open this link\n   https://auth.openai.com/codex/device\n"
	if got := Parse(urlOnly, time.Now(), time.Now()); got.State != StateStarting {
		t.Errorf("url without code: state=%q, want starting", got.State)
	}
	codeOnly := "2. Enter this one-time code\n   E7OV-KG840\n"
	if got := Parse(codeOnly, time.Now(), time.Now()); got.State != StateStarting {
		t.Errorf("code without url: state=%q, want starting", got.State)
	}
}

// Without a trustworthy origin for the countdown there is no honest absolute
// expiry, so the code is still served and ExpiresAt stays zero. The UI shows no
// timer rather than a wrong one.
func TestParse_UnknownStartLeavesExpiryZero(t *testing.T) {
	got := Parse(livePane, time.Time{}, time.Now())
	if got.State != StateReady {
		t.Fatalf("state=%q, want ready", got.State)
	}
	if !got.ExpiresAt.IsZero() {
		t.Errorf("expiresAt=%v, want zero when the start time is unknown", got.ExpiresAt)
	}
}

// Guards the anchoring in codeRe: pane text is full of hex and identifiers, and
// a loose pattern would happily return one of them as a login code.
func TestParse_DoesNotMistakeOtherTextForACode(t *testing.T) {
	noisy := `https://auth.openai.com/codex/device
	commit a1b2c3d-deadbeef
	lower-case-words-here
	`
	if got := Parse(noisy, time.Now(), time.Now()); got.UserCode != "" {
		t.Fatalf("userCode=%q, want none — nothing here is a device code", got.UserCode)
	}
}

// encoding/json's omitempty does not apply to structs, so the zero ExpiresAt
// this package deliberately leaves behind used to go out as a year-1 deadline
// and read as "already expired" to any client doing the obvious thing.
func TestResultJSONOmitsUnknownExpiry(t *testing.T) {
	b, err := json.Marshal(Result{State: StateReady, VerificationURL: "https://auth.openai.com/codex/device", UserCode: "E7OV-KG840"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), "expiresAt") {
		t.Fatalf("expiresAt must be absent when unknown; got %s", b)
	}

	at := time.Date(2026, 8, 26, 20, 15, 0, 0, time.UTC)
	b, err = json.Marshal(Result{State: StateReady, ExpiresAt: at})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"expiresAt":"2026-08-26T20:15:00Z"`) {
		t.Fatalf("a known expiry must still be serialised; got %s", b)
	}
}

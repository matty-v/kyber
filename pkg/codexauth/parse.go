// Package codexauth parses the output of `codex login --device-auth` into
// something an API can hand to a UI.
//
// Why parse a terminal at all: the Codex CLI has no JSON or otherwise
// machine-readable mode for the device flow — checked against the pinned
// version, `codex login --help` offers only `--device-auth` with no output
// options. Its human prompt is the only source of the code, so Kyber reads the
// tmux pane the flow runs in.
//
// That makes this a version-sensitive contract with an upstream we do not
// control. It is deliberately tolerant: it looks for the two things that
// matter, in any order, anywhere in the pane, and reports StateStarting rather
// than an error when it cannot find them. A prompt reworded upstream degrades
// to "still starting" — the operator sees a spinner instead of a wrong code.
// Re-verify against the pane when bumping the Codex version; see
// images/codex/INSTALL_NOTES.md.
package codexauth

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// State is what the device flow is doing, from the platform's point of view.
type State string

const (
	// StateAbsent means no device-login session is running.
	StateAbsent State = "absent"
	// StateStarting means the session exists but has not printed a code yet.
	StateStarting State = "starting"
	// StateReady means a code is available and has not expired.
	StateReady State = "ready"
	// StateExpired means a code was printed but its window has passed.
	StateExpired State = "expired"
	// StateFailed means Kyber could not read the flow at all — the probe
	// itself did not run. Distinct from StateStarting on purpose: starting is
	// "ask again in a moment", failed is "this will not fix itself".
	StateFailed State = "failed"
)

// Result is the parsed state of one device-login attempt.
type Result struct {
	State State `json:"state"`
	// VerificationURL is the page the operator opens. Empty unless State is
	// StateReady or StateExpired.
	VerificationURL string `json:"verificationUrl,omitempty"`
	// UserCode is the one-time code typed into that page.
	UserCode string `json:"userCode,omitempty"`
	// Detail carries a short operator-facing reason when State is StateFailed.
	// Empty otherwise.
	Detail string `json:"detail,omitempty"`
	// ExpiresAt is when the code stops working, derived from the flow's own
	// "expires in N minutes" anchored to startedAt. Zero when unknown.
	//
	// omitZERO, not omitempty: omitempty has no effect on a struct field, so
	// the obvious tag would ship "0001-01-01T00:00:00Z" for the case Parse
	// deliberately leaves zero. A client reading that as an absolute deadline
	// sees a code that expired two millennia ago and reports a perfectly good
	// code as expired. The API contract says the field is absent here, and
	// omitzero (Go 1.24+) is what actually makes it absent.
	ExpiresAt time.Time `json:"expiresAt,omitzero"`
}

var (
	// The flow prints its link on its own line. Matching the host rather than
	// the full path keeps this working if OpenAI moves the page.
	urlRe = regexp.MustCompile(`https://[a-zA-Z0-9.-]*openai\.com/\S*`)

	// Observed shape: E7OV-KG840. Anchored to a whole "word" so a hex string
	// elsewhere in the pane cannot masquerade as a code, and required to be
	// upper-case because the flow prints it that way — a lower-case match is
	// far more likely to be prose than a code.
	codeRe = regexp.MustCompile(`\b([A-Z0-9]{4,6}-[A-Z0-9]{4,6})\b`)

	// "(expires in 15 minutes)" — the only expiry the flow states.
	expiryRe = regexp.MustCompile(`expires in (\d+) minutes?`)
)

// Parse reads a captured pane and reports what the device flow is showing.
//
// startedAt anchors the countdown: the flow states a relative window ("expires
// in 15 minutes") and never a wall-clock time, so an absolute ExpiresAt is only
// as good as the caller's knowledge of when the flow began. Pass the tmux
// session's creation time, which is within a second or two of the code being
// printed. Pass the zero time when it is unknown and ExpiresAt is left zero
// rather than guessed — a countdown from the wrong origin is worse than none.
//
// now is taken as a parameter so expiry is testable without sleeping.
func Parse(pane string, startedAt, now time.Time) Result {
	if strings.TrimSpace(pane) == "" {
		return Result{State: StateStarting}
	}

	code := codeRe.FindStringSubmatch(pane)
	url := urlRe.FindString(pane)
	if code == nil || url == "" {
		// One without the other is a half-drawn pane, not a usable prompt.
		return Result{State: StateStarting}
	}

	res := Result{
		State:           StateReady,
		VerificationURL: strings.TrimRight(url, ".,)"),
		UserCode:        code[1],
	}

	if m := expiryRe.FindStringSubmatch(pane); m != nil && !startedAt.IsZero() {
		if mins, err := strconv.Atoi(m[1]); err == nil {
			res.ExpiresAt = startedAt.Add(time.Duration(mins) * time.Minute)
			if !now.IsZero() && now.After(res.ExpiresAt) {
				res.State = StateExpired
			}
		}
	}
	return res
}

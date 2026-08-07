package inbound

import (
	"net/http"
	"strings"
	"testing"

	apiv1 "github.com/matty-v/kyber/pkg/api/v1"
)

// githubPRBody is a trimmed pull_request.opened payload — enough fields for
// the typical operator wiring (event name, action, repo full_name, number,
// title, body).
const githubPRBody = `{
  "action": "opened",
  "number": 42,
  "pull_request": {
    "title": "Add inbound prompts",
    "body": "Implements kyber#208"
  },
  "repository": {
    "full_name": "matty-v/kyber"
  }
}`

func githubBinding() apiv1.AgentInboundBinding {
	return apiv1.AgentInboundBinding{
		Name:            "github",
		ExistingSecret:  "github-webhook",
		SignatureHeader: "X-Hub-Signature-256",
		SignaturePrefix: "sha256=",
		EventHeader:     "X-GitHub-Event",
		EventPath:       "$.action",
		MatchEvents:     []string{"pull_request.opened"},
		Filters: []apiv1.AgentInboundFilter{
			{JsonPath: "$.repository.full_name", Equals: "matty-v/kyber"},
		},
		Fields: []apiv1.AgentInboundField{
			{Label: "repo", JsonPath: "$.repository.full_name"},
			{Label: "pr", JsonPath: "$.number"},
			{Label: "title", JsonPath: "$.pull_request.title"},
		},
		Action: "Review the PR and post a TLDR.",
	}
}

func TestDecideHappyPath(t *testing.T) {
	headers := http.Header{}
	headers.Set("X-GitHub-Event", "pull_request")

	d := Decide([]byte(githubPRBody), headers, "req-1", "dave", githubBinding())
	if !d.Match {
		t.Fatalf("expected match, got drop=%q", d.DropReason)
	}

	want := strings.Join([]string{
		"[inbound:req-1] binding=github agent=dave",
		"data:",
		"  repo: matty-v/kyber",
		"  pr: 42",
		"  title: Add inbound prompts",
		"action:",
		"  Review the PR and post a TLDR.",
		"",
	}, "\n")
	if d.Envelope != want {
		t.Fatalf("envelope mismatch:\n--- got ---\n%s\n--- want ---\n%s", d.Envelope, want)
	}
}

func TestDecideUnmatchedEvent(t *testing.T) {
	headers := http.Header{}
	headers.Set("X-GitHub-Event", "issues")

	d := Decide([]byte(githubPRBody), headers, "req-2", "dave", githubBinding())
	if d.Match {
		t.Fatalf("expected drop, got match envelope=%q", d.Envelope)
	}
	if d.DropReason != DropReasonUnmatchedEvent {
		t.Fatalf("DropReason = %q, want %q", d.DropReason, DropReasonUnmatchedEvent)
	}
}

func TestDecideFilterRejected(t *testing.T) {
	body := strings.Replace(githubPRBody, "matty-v/kyber", "evil/repo", 1)
	headers := http.Header{}
	headers.Set("X-GitHub-Event", "pull_request")

	d := Decide([]byte(body), headers, "req-3", "dave", githubBinding())
	if d.Match {
		t.Fatalf("expected drop, got match")
	}
	if d.DropReason != DropReasonFilterRejected {
		t.Fatalf("DropReason = %q, want %q", d.DropReason, DropReasonFilterRejected)
	}
}

func TestDecideFilterInList(t *testing.T) {
	b := githubBinding()
	b.Filters = []apiv1.AgentInboundFilter{
		{JsonPath: "$.repository.full_name", In: []string{"a/b", "matty-v/kyber"}},
	}
	headers := http.Header{}
	headers.Set("X-GitHub-Event", "pull_request")

	d := Decide([]byte(githubPRBody), headers, "req-4", "dave", b)
	if !d.Match {
		t.Fatalf("In-list match expected, got drop=%q", d.DropReason)
	}
}

func TestDecideEmptyFilterTreatedAsPass(t *testing.T) {
	b := githubBinding()
	// Operator misconfig: no In, no Equals.
	b.Filters = []apiv1.AgentInboundFilter{
		{JsonPath: "$.repository.full_name"},
	}
	headers := http.Header{}
	headers.Set("X-GitHub-Event", "pull_request")

	d := Decide([]byte(githubPRBody), headers, "req-misconfig", "dave", b)
	if !d.Match {
		t.Fatalf("empty filter clauses should pass, got drop=%q", d.DropReason)
	}
}

func TestDecideMatchEventsEmptyAcceptsAll(t *testing.T) {
	b := githubBinding()
	b.MatchEvents = nil

	headers := http.Header{}
	headers.Set("X-GitHub-Event", "anything-goes")
	// Filter still wired so we must keep the matching repo.
	d := Decide([]byte(githubPRBody), headers, "req-5", "dave", b)
	if !d.Match {
		t.Fatalf("empty MatchEvents should accept any event, got drop=%q", d.DropReason)
	}
}

func TestDecideTruncateAndPrefix(t *testing.T) {
	body := []byte(`{"long":"abcdefghij","url":"https://example.com/path"}`)
	b := apiv1.AgentInboundBinding{
		Name:            "x",
		ExistingSecret:  "s",
		SignatureHeader: "X-Sig",
		Action:          "do",
		Fields: []apiv1.AgentInboundField{
			{Label: "short", JsonPath: "$.long", Truncate: 4},
			{Label: "link", JsonPath: "$.url", Prefix: "URL: "},
		},
	}
	d := Decide(body, http.Header{}, "rid", "ag", b)
	if !d.Match {
		t.Fatalf("expected match, got drop=%q", d.DropReason)
	}
	if !strings.Contains(d.Envelope, "  short: abcd\n") {
		t.Errorf("expected truncated short field, got envelope:\n%s", d.Envelope)
	}
	if !strings.Contains(d.Envelope, "  link: URL: https://example.com/path\n") {
		t.Errorf("expected prefixed link field, got envelope:\n%s", d.Envelope)
	}
}

func TestDecideTruncateUTF8Safe(t *testing.T) {
	// Each emoji is 4 bytes in UTF-8 but 1 rune. Truncating to 2 should yield
	// two emojis and not split a multi-byte sequence.
	body := []byte(`{"emoji":"` + "\xF0\x9F\x98\x80\xF0\x9F\x98\x81\xF0\x9F\x98\x82" + `"}`)
	b := apiv1.AgentInboundBinding{
		Name:            "x",
		ExistingSecret:  "s",
		SignatureHeader: "X-Sig",
		Action:          "do",
		Fields: []apiv1.AgentInboundField{
			{Label: "e", JsonPath: "$.emoji", Truncate: 2},
		},
	}
	d := Decide(body, http.Header{}, "rid", "ag", b)
	if !d.Match {
		t.Fatalf("expected match, got drop=%q", d.DropReason)
	}
	want := "  e: \xF0\x9F\x98\x80\xF0\x9F\x98\x81\n"
	if !strings.Contains(d.Envelope, want) {
		t.Errorf("envelope missing UTF-8-safe truncation; got:\n%s", d.Envelope)
	}
}

func TestDecideMultiLineAction(t *testing.T) {
	b := apiv1.AgentInboundBinding{
		Name:            "x",
		ExistingSecret:  "s",
		SignatureHeader: "X-Sig",
		Action:          "Step one.\nStep two.\nStep three.",
	}
	d := Decide([]byte(`{}`), http.Header{}, "r", "ag", b)
	if !d.Match {
		t.Fatalf("expected match, got drop=%q", d.DropReason)
	}
	wantSegment := "action:\n  Step one.\n  Step two.\n  Step three.\n"
	if !strings.HasSuffix(d.Envelope, wantSegment) {
		t.Errorf("multi-line action not indented as expected; got:\n%s", d.Envelope)
	}
}

func TestDecideMissingFieldPathCoercesToEmpty(t *testing.T) {
	b := apiv1.AgentInboundBinding{
		Name:            "x",
		ExistingSecret:  "s",
		SignatureHeader: "X-Sig",
		Action:          "do",
		Fields: []apiv1.AgentInboundField{
			{Label: "missing", JsonPath: "$.nope.notHere"},
		},
	}
	d := Decide([]byte(`{}`), http.Header{}, "r", "ag", b)
	if !d.Match {
		t.Fatalf("missing field should not cause drop, got reason=%q", d.DropReason)
	}
	if !strings.Contains(d.Envelope, "  missing: \n") {
		t.Errorf("expected empty-string rendering for missing field, got:\n%s", d.Envelope)
	}
}

func TestNormalizeJSONPath(t *testing.T) {
	tests := map[string]string{
		"":             "{$}",
		"$":            "{$}",
		"$.foo":        "{.foo}",
		"$.foo.bar":    "{.foo.bar}",
		"$.foo[0].bar": "{.foo[0].bar}",
		".foo.bar":     "{.foo.bar}",
		"foo.bar":      "{.foo.bar}",
		"{.already}":   "{.already}",
	}
	for in, want := range tests {
		if got := normalizeJSONPath(in); got != want {
			t.Errorf("normalizeJSONPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEvalJSONPathArrayIndex(t *testing.T) {
	body := []byte(`{"items":[{"name":"a"},{"name":"b"}]}`)
	got, err := evalJSONPath(body, "$.items[1].name")
	if err != nil {
		t.Fatalf("evalJSONPath: %v", err)
	}
	if got != "b" {
		t.Errorf("got %q want %q", got, "b")
	}
}

func TestEvalJSONPathBadJSONReturnsError(t *testing.T) {
	if _, err := evalJSONPath([]byte("not-json"), "$.x"); err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

// TestDecideAndDecideDebugAgreeOnDecision asserts that DecideDebug returns
// byte-identical Match / DropReason / Envelope for the same inputs as Decide.
// DecideDebug adds per-step trace info (FilterResults, FieldResults,
// ResolvedEvent) but must NOT diverge from Decide on the boolean outcome —
// otherwise the operator's "test payload" panel could show a green match
// for a binding that production would drop, or vice versa.
//
// Drives a representative slice of cases through both functions in one
// table. Catches future drift (e.g. one side fixes a bug but not the other).
func TestDecideAndDecideDebugAgreeOnDecision(t *testing.T) {
	emptyAcceptingBinding := func() apiv1.AgentInboundBinding {
		b := githubBinding()
		// Filter that legitimately accepts an empty extracted value
		// (operator opts in to "" via In). DecideDebug used to mis-flag
		// this as missing-key — review #1 caught the drift.
		b.Filters = []apiv1.AgentInboundFilter{
			{JsonPath: "$.optional_field", In: []string{"", "x"}},
			{JsonPath: "$.repository.full_name", Equals: "matty-v/kyber"},
		}
		return b
	}

	cases := []struct {
		name    string
		body    string
		event   string
		binding apiv1.AgentInboundBinding
	}{
		{"happy path", githubPRBody, "pull_request", githubBinding()},
		{"unmatched event", githubPRBody, "issues", githubBinding()},
		{
			"filter rejected (wrong repo)",
			strings.Replace(githubPRBody, "matty-v/kyber", "other/repo", 1),
			"pull_request",
			githubBinding(),
		},
		{"empty filters pass", githubPRBody, "pull_request", func() apiv1.AgentInboundBinding {
			b := githubBinding()
			b.Filters = nil
			return b
		}()},
		{"empty matchEvents accepts", githubPRBody, "pull_request", func() apiv1.AgentInboundBinding {
			b := githubBinding()
			b.MatchEvents = nil
			return b
		}()},
		{"truncate + prefix", githubPRBody, "pull_request", func() apiv1.AgentInboundBinding {
			b := githubBinding()
			b.Fields[2].Truncate = 5
			b.Fields[2].Prefix = "= "
			return b
		}()},
		{"missing field path coerces to empty", githubPRBody, "pull_request", func() apiv1.AgentInboundBinding {
			b := githubBinding()
			b.Fields = append(b.Fields, apiv1.AgentInboundField{Label: "missing", JsonPath: "$.does_not_exist"})
			return b
		}()},
		{"In-list legitimately accepts empty", githubPRBody, "pull_request", emptyAcceptingBinding()},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := http.Header{}
			h.Set("X-GitHub-Event", tc.event)
			d := Decide([]byte(tc.body), h, "req-1", "dave", tc.binding)
			dbg := DecideDebug([]byte(tc.body), h, "req-1", "dave", tc.binding)

			if d.Match != dbg.Match {
				t.Errorf("Match drift: Decide=%v DecideDebug=%v", d.Match, dbg.Match)
			}
			if d.DropReason != dbg.DropReason {
				t.Errorf("DropReason drift: Decide=%q DecideDebug=%q", d.DropReason, dbg.DropReason)
			}
			if d.Envelope != dbg.Envelope {
				t.Errorf("Envelope drift:\nDecide:\n%s\nDecideDebug:\n%s", d.Envelope, dbg.Envelope)
			}
		})
	}
}

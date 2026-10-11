package usagestore

import (
	"errors"
	"testing"
	"time"
)

func n(v int64) *int64 { return &v }

func validEvent() Event {
	return Event{AgentUID: "agent-uid-1", Agent: "test-agent", ObservedAt: time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC), Runtime: "hermes",
		SourceFormat: "hermes-api-call-log", SourceVersion: "1", SourceIdentity: "session-1:call-1", Coverage: "observed",
		ConfiguredRoute: "openrouter", RouteID: "6aba382f-a9f7-45a5-a940-b7d9aa77471c", Model: "m", ModelEvidence: "observed",
		Output: n(0), RawUsage: map[string]int64{"input": 100}}
}

func TestEventValidationPreservesUnknownVersusZero(t *testing.T) {
	e := validEvent()
	if err := e.Validate(); err != nil {
		t.Fatal(err)
	}
	if e.InputUncached != nil || e.CacheRead != nil || e.Output == nil || *e.Output != 0 {
		t.Fatalf("classes = %+v", e)
	}
	for _, tc := range []struct {
		name   string
		change func(*Event)
	}{
		{"missing UID", func(e *Event) { e.AgentUID = "" }},
		{"missing route ID", func(e *Event) { e.RouteID = "" }},
		{"negative token", func(e *Event) { e.Output = n(-1) }},
		{"arbitrary raw key", func(e *Event) { e.RawUsage = map[string]int64{"prompt": 1} }},
		{"unknown route", func(e *Event) { e.ConfiguredRoute = "gateway-secret" }},
		{"inferred upstream", func(e *Event) { provider := "openrouter"; e.ObservedUpstreamProvider = &provider }},
		{"endpoint as route ID", func(e *Event) { e.RouteID = "https://inference.example" }},
		{"caller supplied ID", func(e *Event) { e.ID = "chosen" }},
		{"no source numbers", func(e *Event) { e.Output = nil; e.RawUsage = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			candidate := validEvent()
			tc.change(&candidate)
			if !errors.Is(candidate.Validate(), ErrInvalid) {
				t.Fatalf("expected ErrInvalid: %+v", candidate)
			}
		})
	}
}

func TestGapValidation(t *testing.T) {
	g := Gap{AgentUID: "uid", Runtime: "claude", SourceFormat: "claude-jsonl", SourceIdentity: "file-1:offset-22", DetectedAt: time.Now().UTC(), Reason: "truncated"}
	if err := g.Validate(); err != nil {
		t.Fatal(err)
	}
	g.Reason = "lost 14 calls"
	if !errors.Is(g.Validate(), ErrInvalid) {
		t.Fatal("gap reason must be a bounded code")
	}
}

func TestPostgresStoreRequiresDatabase(t *testing.T) {
	if _, err := NewPostgresStore(nil); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err=%v", err)
	}
}

func TestRetentionDefaultsAndValidation(t *testing.T) {
	r := DefaultRetention()
	if r.DetailDays != 90 || r.DailyMonths != 13 || r.Validate() != nil {
		t.Fatalf("retention=%+v", r)
	}
	if !errors.Is((Retention{DetailDays: 90, DailyMonths: 1}).Validate(), ErrInvalid) {
		t.Fatal("daily aggregates must outlive detail")
	}
}

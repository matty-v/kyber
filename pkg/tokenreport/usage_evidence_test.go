package tokenreport

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestParseClaudeUsageLineFinalizedAndRepeatedBlocks(t *testing.T) {
	line := []byte(`{"timestamp":"2026-10-11T12:00:00Z","type":"assistant","message":{"id":"msg_1","model":"claude-sonnet-4-5","usage":{"input_tokens":50,"cache_creation_input_tokens":3,"cache_read_input_tokens":7,"output_tokens":10,"speed":"standard"}}}`)
	a, err := ParseClaudeUsageLine(line, "session-1")
	b, err2 := ParseClaudeUsageLine(line, "session-1")
	if err != nil || err2 != nil || a == nil || b == nil {
		t.Fatalf("evidence a=%+v b=%+v err=%v/%v", a, b, err, err2)
	}
	if a.SourceIdentity != b.SourceIdentity || a.SourceIdentity != "session-1:msg_1" {
		t.Fatalf("repeated blocks have different identity: %q/%q", a.SourceIdentity, b.SourceIdentity)
	}
	if *a.InputUncached != 50 || *a.CacheWrite != 3 || *a.CacheRead != 7 || *a.Output != 10 {
		t.Fatalf("counts = %+v", a)
	}
	if strings.Contains(a.SourceIdentity, "/persist/") {
		t.Fatal("source identity contains a filesystem path")
	}
	secondLine := strings.Replace(string(line), "msg_1", "msg_2", 1)
	second, err := ParseClaudeUsageLine([]byte(secondLine), "session-1")
	if err != nil || second == nil || second.SourceIdentity == a.SourceIdentity || *second.InputUncached != *a.InputUncached {
		t.Fatalf("equal-count next call should remain distinct: %+v err=%v", second, err)
	}
}

func TestParseClaudeUsageLineRejectsMissingIdentityAndTime(t *testing.T) {
	for _, line := range []string{
		`{"timestamp":"2026-10-11T12:00:00Z","type":"assistant","message":{"model":"m","usage":{"speed":"standard"}}}`,
		`{"type":"assistant","message":{"id":"msg_1","model":"m","usage":{"speed":"standard"}}}`,
		`{"timestamp":"2026-10-11T12:00:00Z","type":"assistant","message":{"id":"msg_1","model":"m","usage":{"input_tokens":-1,"speed":"standard"}}}`,
	} {
		got, err := ParseClaudeUsageLine([]byte(line), "session-1")
		if got != nil || !errors.Is(err, ErrIncompleteUsage) {
			t.Fatalf("got=%+v err=%v", got, err)
		}
	}
	streaming := []byte(`{"type":"assistant","message":{"id":"msg_1","usage":{"speed":"?"}}}`)
	if got, err := ParseClaudeUsageLine(streaming, "session-1"); got != nil || err != nil {
		t.Fatalf("streaming got=%+v err=%v", got, err)
	}
}

func TestUsageParsersReportMalformedRecordsAsGaps(t *testing.T) {
	if got, err := ParseClaudeUsageLine([]byte(`{"type":"assistant"`), "session-1"); got != nil || !errors.Is(err, ErrIncompleteUsage) {
		t.Fatalf("Claude malformed record got=%+v err=%v", got, err)
	}
	if got, err := ParseCodexUsageLine([]byte(`{"type":"event_msg"`), "m"); got != nil || !errors.Is(err, ErrIncompleteUsage) {
		t.Fatalf("Codex malformed record got=%+v err=%v", got, err)
	}
}

func TestParseClaudeUsageLineMissingClassIsUnknown(t *testing.T) {
	line := []byte(`{"timestamp":"2026-10-11T12:00:00Z","type":"assistant","message":{"id":"msg_1","model":"m","usage":{"input_tokens":0,"speed":"standard"}}}`)
	got, err := ParseClaudeUsageLine(line, "session-1")
	if err != nil || got == nil || got.InputUncached == nil || *got.InputUncached != 0 || got.Output != nil || got.CacheRead != nil || got.CacheWrite != nil {
		t.Fatalf("nullable classes got=%+v err=%v", got, err)
	}
}

func TestParseCodexUsageLineKeepsCounterSemantics(t *testing.T) {
	line := []byte(`{"timestamp":"2026-10-11T12:00:00Z","type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":100,"cached_input_tokens":60,"output_tokens":30},"total_token_usage":{"input_tokens":900,"cached_input_tokens":500,"cache_write_input_tokens":4,"output_tokens":750}}}}`)
	got, err := ParseCodexUsageLine(line, "gpt-6.1-sol")
	if err != nil || got == nil {
		t.Fatalf("observation=%+v err=%v", got, err)
	}
	if *got.Last.InputUncached != 40 || *got.Last.CacheRead != 60 || *got.Last.Output != 30 || got.Last.CacheWrite != nil {
		t.Fatalf("last=%+v", got.Last)
	}
	if *got.Total.InputUncached != 400 || *got.Total.Output != 750 || *got.Total.CacheWrite != 4 {
		t.Fatalf("total=%+v", got.Total)
	}
	if got.Last.SourceIdentity != "" || got.Total.SourceIdentity != "" {
		t.Fatal("a token_count observation must not assert a billable call identity")
	}
	bad := strings.Replace(string(line), `"cached_input_tokens":60`, `"cached_input_tokens":101`, 1)
	if _, err := ParseCodexUsageLine([]byte(bad), "gpt-6.1-sol"); !errors.Is(err, ErrIncompleteUsage) {
		t.Fatalf("invalid cached count err=%v", err)
	}
}

func TestParseHermesUsageLineRouteAndUnknownCache(t *testing.T) {
	line := "2026-10-11 12:00:00,000 INFO [session_one] agent.conversation_loop: API call #2: model=z-ai/glm-5.2 provider=openrouter in=100 out=20 total=120 latency=1s"
	got, err := ParseHermesUsageLine(line, "log-1", time.FixedZone("local", -4*3600))
	if err != nil || got == nil {
		t.Fatalf("evidence=%+v err=%v", got, err)
	}
	if got.SourceIdentity != "log-1:session_one:2" || got.ReportedProviderLabel != "openrouter" || got.CacheRead != nil || got.CacheWrite != nil || *got.InputUncached != 100 {
		t.Fatalf("evidence=%+v", got)
	}
	if got.ObservedAt.Format(time.RFC3339) != "2026-10-11T16:00:00Z" {
		t.Fatalf("time=%s", got.ObservedAt)
	}
	withCache := line + " cache=60/100 (60%)"
	got, err = ParseHermesUsageLine(withCache, "log-1", time.UTC)
	if err != nil || *got.InputUncached != 40 || *got.CacheRead != 60 {
		t.Fatalf("cached evidence=%+v err=%v", got, err)
	}
	if got, err := ParseHermesUsageLine(line, "log-1", nil); got != nil || !errors.Is(err, ErrIncompleteUsage) {
		t.Fatalf("missing timezone got=%+v err=%v", got, err)
	}
	second, err := ParseHermesUsageLine(strings.Replace(line, "call #2", "call #3", 1), "log-1", time.UTC)
	if err != nil || second == nil || second.SourceIdentity == got.SourceIdentity || *second.InputUncached != 100 {
		t.Fatalf("equal-count next call should remain distinct: %+v err=%v", second, err)
	}
}

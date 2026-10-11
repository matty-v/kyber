package tokenreport

import (
	"bufio"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReadUsageLineBoundsTranscriptContent(t *testing.T) {
	reader := bufio.NewReader(strings.NewReader(strings.Repeat("x", maxUsageLine+1) + "\n"))
	line, n, complete, large, err := readUsageLine(reader)
	if err != nil || !complete || !large || line != nil || n != maxUsageLine+2 {
		t.Fatalf("line=%d bytes=%d complete=%v large=%v err=%v", len(line), n, complete, large, err)
	}
}

type usageSinkFixture struct {
	events    []UsageEvidence
	gaps      []string
	failOnce  bool
	failGapAt int
}

func (s *usageSinkFixture) AcceptUsage(_ context.Context, e UsageEvidence, _ UsageAckCursor) error {
	if s.failOnce {
		s.failOnce = false
		return errors.New("transport unavailable")
	}
	s.events = append(s.events, e)
	return nil
}

func (s *usageSinkFixture) AcceptGap(_ context.Context, id, reason string, _ time.Time, _ UsageAckCursor) error {
	s.gaps = append(s.gaps, id+":"+reason)
	if s.failGapAt > 0 && len(s.gaps) == s.failGapAt {
		return errors.New("gap transport unavailable")
	}
	return nil
}

func claudeUsageLine(id string) string {
	return `{"timestamp":"2026-10-11T12:00:00Z","type":"assistant","message":{"id":"` + id + `","model":"claude-test","usage":{"input_tokens":50,"cache_read_input_tokens":0,"output_tokens":10,"speed":"standard"}}}` + "\n"
}

func appendUsage(t *testing.T, path, body string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(body); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestClaudeUsageCollectorBootReplayAndRouteChange(t *testing.T) {
	root := filepath.Join(t.TempDir(), "projects")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "session.jsonl")
	if err := os.WriteFile(path, []byte(claudeUsageLine("old")), 0600); err != nil {
		t.Fatal(err)
	}
	cursor := filepath.Join(t.TempDir(), "usage", "cursor.json")
	sink := &usageSinkFixture{}
	c := &ClaudeUsageCollector{ProjectsRoot: root, CursorPath: cursor, RouteFingerprint: "route-A", Sink: sink}
	ctx := context.Background()
	if err := c.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	if len(sink.events) != 0 {
		t.Fatal("pre-cutover transcript was counted")
	}
	duplicateBlock := strings.Replace(claudeUsageLine("msg-2"), "2026-10-11T12:00:00Z", "2026-10-11T12:00:01Z", 1)
	appendUsage(t, path, claudeUsageLine("msg-1")+claudeUsageLine("msg-2")+duplicateBlock)
	sink.failOnce = true
	if err := c.Scan(ctx); err == nil {
		t.Fatal("expected transport failure")
	}
	if len(sink.events) != 0 {
		t.Fatal("failed delivery advanced cursor")
	}
	if err := c.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	if len(sink.events) != 2 || sink.events[0].SourceIdentity == sink.events[1].SourceIdentity ||
		*sink.events[0].InputUncached != 50 || *sink.events[1].InputUncached != 50 {
		t.Fatalf("events=%+v", sink.events)
	}
	restarted := &ClaudeUsageCollector{ProjectsRoot: root, CursorPath: cursor, RouteFingerprint: "route-A", Sink: sink}
	if err := restarted.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	if len(sink.events) != 2 {
		t.Fatal("restart replayed acknowledged events")
	}
	appendUsage(t, path, claudeUsageLine("old-route-tail"))
	if err := os.WriteFile(filepath.Join(root, "unseen.jsonl"), []byte(claudeUsageLine("unseen-old-route")), 0600); err != nil {
		t.Fatal(err)
	}
	changed := &ClaudeUsageCollector{ProjectsRoot: root, CursorPath: cursor, RouteFingerprint: "route-B", Sink: sink}
	sink.failGapAt = 2
	if err := changed.Scan(ctx); err == nil {
		t.Fatal("expected failure before route cursor commit")
	}
	state, err := readClaudeCursor(cursor)
	if err != nil || state.RouteFingerprint != "route-A" {
		t.Fatalf("route cursor advanced before all gaps were acknowledged: %+v %v", state, err)
	}
	sink.failGapAt = 0
	if err := changed.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	if len(sink.events) != 2 || len(sink.gaps) != 4 || sink.gaps[0] != sink.gaps[2] || sink.gaps[1] != sink.gaps[3] || !strings.HasSuffix(sink.gaps[0], ":route_changed") {
		t.Fatalf("route change events=%d gaps=%v", len(sink.events), sink.gaps)
	}
	appendUsage(t, path, claudeUsageLine("new-route"))
	if err := changed.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	if len(sink.events) != 3 {
		t.Fatalf("new route events=%+v", sink.events)
	}
	raw, err := os.ReadFile(cursor)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "input_tokens") || strings.Contains(string(raw), "old-route-tail") {
		t.Fatal("cursor persisted transcript content")
	}
}

func TestClaudeUsageCollectorTruncationAndPartialLine(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "session.jsonl")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	sink := &usageSinkFixture{}
	c := &ClaudeUsageCollector{ProjectsRoot: root, CursorPath: filepath.Join(t.TempDir(), "cursor.json"), RouteFingerprint: "route-A", Sink: sink}
	ctx := context.Background()
	if err := c.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	line := claudeUsageLine("before")
	appendUsage(t, path, line[:len(line)-1])
	if err := c.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	if len(sink.events) != 0 {
		t.Fatal("partial line was counted")
	}
	appendUsage(t, path, "\n")
	if err := c.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	if len(sink.events) != 1 {
		t.Fatalf("events=%+v", sink.events)
	}
	if err := os.WriteFile(path, []byte(claudeUsageLine("x")), 0600); err != nil {
		t.Fatal(err)
	}
	if err := c.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	if len(sink.gaps) != 1 || len(sink.events) != 2 || sink.events[0].SourceIdentity == sink.events[1].SourceIdentity {
		t.Fatalf("truncation events=%+v gaps=%v", sink.events, sink.gaps)
	}
}

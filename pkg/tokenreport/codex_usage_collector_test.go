package tokenreport

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func codexCounterLine(input, cached, output int) string {
	return fmt.Sprintf(`{"timestamp":"2026-10-11T12:00:00Z","type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":50,"cached_input_tokens":10,"output_tokens":10},"total_token_usage":{"input_tokens":%d,"cached_input_tokens":%d,"output_tokens":%d}}}}`+"\n", input, cached, output)
}

func TestCodexUsageCollector_DeltasReplayResetAndModelSwitch(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "rollout.jsonl")
	if err := os.WriteFile(path, []byte(codexCounterLine(50, 10, 10)), 0600); err != nil {
		t.Fatal(err)
	}
	sink := &usageSinkFixture{}
	cursor := filepath.Join(t.TempDir(), "cursor.json")
	c := &CodexUsageCollector{SessionsRoot: root, CursorPath: cursor, RouteFingerprint: "route-A", ConfiguredModel: "gpt-test", Sink: sink}
	ctx := context.Background()
	if err := c.Scan(ctx); err != nil || len(sink.events) != 0 {
		t.Fatalf("historical baseline: err=%v events=%d", err, len(sink.events))
	}
	appendUsage(t, path, codexCounterLine(100, 20, 20)+codexCounterLine(100, 20, 20)+codexCounterLine(150, 30, 30))
	sink.failOnce = true
	if err := c.Scan(ctx); err == nil {
		t.Fatal("expected failed ACK")
	}
	if err := c.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	if len(sink.events) != 2 || sink.events[0].SourceIdentity == sink.events[1].SourceIdentity ||
		*sink.events[0].InputUncached != 40 || *sink.events[1].InputUncached != 40 ||
		*sink.events[0].CacheRead != 10 || *sink.events[1].Output != 10 {
		t.Fatalf("derived calls=%+v", sink.events)
	}
	if err := c.Scan(ctx); err != nil || len(sink.events) != 2 {
		t.Fatalf("replay: err=%v events=%d", err, len(sink.events))
	}
	appendUsage(t, path, codexCounterLine(10, 2, 3))
	if err := c.Scan(ctx); err != nil || len(sink.gaps) != 1 || len(sink.events) != 2 || !strings.HasSuffix(sink.gaps[0], ":cursor_gap") {
		t.Fatalf("counter reset: err=%v gaps=%v events=%d", err, sink.gaps, len(sink.events))
	}
	appendUsage(t, path, codexCounterLine(60, 12, 13))
	changed := &CodexUsageCollector{SessionsRoot: root, CursorPath: cursor, RouteFingerprint: "route-A", ConfiguredModel: "gpt-new", Sink: sink}
	if err := changed.Scan(ctx); err != nil || len(sink.gaps) != 2 || len(sink.events) != 2 {
		t.Fatalf("model switch: err=%v gaps=%v events=%d", err, sink.gaps, len(sink.events))
	}
	appendUsage(t, path, codexCounterLine(110, 22, 23))
	if err := changed.Scan(ctx); err != nil || len(sink.events) != 3 || sink.events[2].Model != "gpt-new" {
		t.Fatalf("new model: err=%v events=%+v", err, sink.events)
	}
}

func TestCodexUsageCollector_ObservedModelWithoutConfiguredModel(t *testing.T) {
	root := t.TempDir()
	sink := &usageSinkFixture{}
	c := &CodexUsageCollector{SessionsRoot: root, CursorPath: filepath.Join(t.TempDir(), "cursor.json"),
		RouteFingerprint: "route-A", Sink: sink}
	if err := c.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "rollout.jsonl")
	contextLine := `{"type":"turn_context","payload":{"model":"gpt-observed"}}` + "\n"
	if err := os.WriteFile(path, []byte(contextLine+codexCounterLine(50, 10, 10)), 0600); err != nil {
		t.Fatal(err)
	}
	if err := c.Scan(context.Background()); err != nil || len(sink.events) != 1 || sink.events[0].Model != "gpt-observed" || sink.events[0].ModelEvidence != "observed" {
		t.Fatalf("observed model: err=%v events=%+v", err, sink.events)
	}
}

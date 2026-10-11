package tokenreport

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func hermesUsageLine(n int) string {
	return "2026-10-11 12:00:00,000 INFO [session_one] agent.conversation_loop: API call #" + string(rune('0'+n)) +
		": model=z-ai/glm-5.2 provider=openrouter in=100 out=20 total=120 latency=1s cache=40/100 (40%)\n"
}

func TestHermesUsageCollector_FirstLogReplayAndRouteChange(t *testing.T) {
	root := t.TempDir()
	logPath := filepath.Join(root, "agent.log")
	cursorPath := filepath.Join(root, "cursor.json")
	sink := &usageSinkFixture{}
	collector := &HermesUsageCollector{LogPath: logPath, CursorPath: cursorPath, RouteFingerprint: "route-A", Location: time.UTC, Sink: sink}
	ctx := context.Background()
	if err := collector.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte(hermesUsageLine(1)+hermesUsageLine(2)), 0600); err != nil {
		t.Fatal(err)
	}
	sink.failOnce = true
	if err := collector.Scan(ctx); err == nil || len(sink.events) != 0 {
		t.Fatalf("unacknowledged call advanced: err=%v events=%d", err, len(sink.events))
	}
	if err := collector.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	if len(sink.events) != 2 || sink.events[0].SourceIdentity == sink.events[1].SourceIdentity ||
		*sink.events[0].InputUncached != 60 || *sink.events[1].InputUncached != 60 {
		t.Fatalf("calls=%+v", sink.events)
	}
	if err := collector.Scan(ctx); err != nil || len(sink.events) != 2 {
		t.Fatalf("restart/replay: err=%v events=%d", err, len(sink.events))
	}
	appendUsage(t, logPath, hermesUsageLine(3))
	changed := &HermesUsageCollector{LogPath: logPath, CursorPath: cursorPath, RouteFingerprint: "route-B", Location: time.UTC, Sink: sink}
	if err := changed.Scan(ctx); err != nil || len(sink.gaps) != 1 || !strings.HasSuffix(sink.gaps[0], ":route_changed") {
		t.Fatalf("route gap: err=%v gaps=%v", err, sink.gaps)
	}
	appendUsage(t, logPath, hermesUsageLine(4))
	if err := changed.Scan(ctx); err != nil || len(sink.events) != 3 {
		t.Fatalf("new route call: err=%v events=%+v", err, sink.events)
	}
}

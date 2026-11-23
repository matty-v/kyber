package tokenreport

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/matty-v/kyber/pkg/activity"
)

func TestCodexWorkInventoryNativeReadOnly(t *testing.T) {
	for _, tc := range []struct {
		name, state            string
		flags                  []string
		jobs                   int
		fail                   bool
		foreground, background int32
		input                  bool
	}{
		{name: "foreground and silent jobs", state: "active", jobs: 2, foreground: 1, background: 2},
		{name: "idle with silent job", state: "idle", jobs: 1, background: 1},
		{name: "fast turn completed between polls", state: "idle"},
		{name: "approval", state: "active", flags: []string{"waitingOnApproval"}, input: true},
		{name: "unsupported job API", state: "idle", fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			socket := filepath.Join(t.TempDir(), "native.sock")
			completedAt := time.Now().Add(-time.Second).Truncate(time.Second)
			listener, err := net.Listen("unix", socket)
			if err != nil {
				t.Fatal(err)
			}
			server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				c, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer c.Close()
				for {
					var req struct {
						ID     int    `json:"id"`
						Method string `json:"method"`
					}
					if c.ReadJSON(&req) != nil {
						return
					}
					if req.Method == "initialized" {
						continue
					}
					var result any
					switch req.Method {
					case "initialize":
						result = map[string]any{}
					case "thread/loaded/list":
						result = map[string]any{"data": []string{"main"}, "nextCursor": nil}
					case "thread/read":
						result = map[string]any{"thread": map[string]any{"status": map[string]any{"type": tc.state, "activeFlags": tc.flags}}}
					case "thread/turns/list":
						result = map[string]any{"data": []map[string]any{{"completedAt": completedAt.Unix()}}}
					case "thread/backgroundTerminals/list":
						if tc.fail {
							_ = c.WriteJSON(map[string]any{"id": req.ID, "error": map[string]any{"code": -32601, "message": "unsupported"}})
							continue
						}
						jobs := make([]map[string]string, tc.jobs)
						for i := range jobs {
							jobs[i] = map[string]string{"processId": strconv.Itoa(i + 1), "command": "must-never-escape"}
						}
						result = map[string]any{"data": jobs, "nextCursor": nil}
					default:
						t.Errorf("mutating or unexpected native method %s", req.Method)
						return
					}
					if c.WriteJSON(map[string]any{"id": req.ID, "result": result}) != nil {
						return
					}
				}
			})}
			go func() { _ = server.Serve(listener) }()
			defer server.Close()
			got, err := CodexWorkInventory(context.Background(), socket)
			if tc.fail {
				if err == nil || got.Complete {
					t.Fatal("unsupported native API claimed complete")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !got.Complete || got.Foreground != tc.foreground || got.Background != tc.background || got.NeedsInput != tc.input || !got.LastWorkedAt.Equal(completedAt) {
				t.Fatalf("snapshot=%+v", got)
			}
		})
	}
}

func TestHermesWorkInventoryProcessLifetimeAndFreshness(t *testing.T) {
	root := t.TempDir()
	stat, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(string(stat)[strings.LastIndex(string(stat), ")")+1:])
	ticks := fields[19]
	snapshot := activity.Snapshot{ObservedAt: time.Now().UTC(), Complete: true, Foreground: 1, Background: 2}
	path := filepath.Join(root, strconv.Itoa(os.Getpid())+"-"+ticks+".json")
	write := func(snapshot activity.Snapshot, pod string) {
		t.Helper()
		data, _ := json.Marshal(map[string]any{"version": 1, "pid": os.Getpid(), "startTicks": ticks, "podUID": pod, "snapshot": snapshot})
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(snapshot, "pod")
	got, err := HermesWorkInventory(root, "pod")
	if err != nil || got.Foreground != 1 || got.Background != 2 || !got.Complete || !got.ObservedAt.Equal(snapshot.ObservedAt) {
		t.Fatalf("snapshot=%+v err=%v", got, err)
	}
	deadPath := filepath.Join(root, "2147483647-1.json")
	deadSource := map[string]any{"version": 1, "pid": 2147483647, "startTicks": "1", "podUID": "pod", "snapshot": activity.Snapshot{ObservedAt: time.Now(), Complete: true, Background: 1}}
	deadData, _ := json.Marshal(deadSource)
	if err := os.WriteFile(deadPath, deadData, 0600); err != nil {
		t.Fatal(err)
	}
	if gap, err := HermesWorkInventory(root, "pod"); err != nil || gap.Complete {
		t.Fatalf("lost publisher's background work became idle: %+v %v", gap, err)
	}
	deadSource["closed"] = true
	deadSource["snapshot"] = activity.Snapshot{ObservedAt: time.Now(), Complete: true}
	deadData, _ = json.Marshal(deadSource)
	if err := os.WriteFile(deadPath, deadData, 0600); err != nil {
		t.Fatal(err)
	}
	if closed, err := HermesWorkInventory(root, "pod"); err != nil || !closed.Complete {
		t.Fatalf("verified clean closure not accepted: %+v %v", closed, err)
	}
	write(snapshot, "previous-pod")
	if _, err := HermesWorkInventory(root, "pod"); err == nil {
		t.Fatal("old pod publisher accepted")
	}
	snapshot.ObservedAt = time.Now().Add(-time.Minute)
	write(snapshot, "pod")
	if _, err := HermesWorkInventory(root, "pod"); err == nil {
		t.Fatal("frozen publisher became idle/working")
	}
}

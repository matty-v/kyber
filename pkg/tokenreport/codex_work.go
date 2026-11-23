package tokenreport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"time"

	"github.com/gorilla/websocket"
	"github.com/matty-v/kyber/pkg/activity"
)

// CodexWorkInventory reads the native daemon over WebSocket/Unix. It never
// resumes/subscribes/steers a thread or starts a model request. Thread status
// and the native background terminal inventory remain available after silence
// and after reporter reconnection; rollout JSONL is not a process registry.
func CodexWorkInventory(ctx context.Context, socketPath string) (activity.Snapshot, error) {
	observedAt := time.Now().UTC()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	d := websocket.Dialer{HandshakeTimeout: 3 * time.Second, NetDialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
	}}
	c, _, err := d.DialContext(ctx, "ws://localhost/", nil)
	if err != nil {
		return activity.Snapshot{}, errors.New("native activity socket unavailable")
	}
	defer c.Close()
	c.SetReadLimit(1 << 20)
	deadline, _ := ctx.Deadline()
	_ = c.SetReadDeadline(deadline)
	_ = c.SetWriteDeadline(deadline)
	id := 0
	call := func(method string, params, dst any) error {
		id++
		if err := c.WriteJSON(map[string]any{"id": id, "method": method, "params": params}); err != nil {
			return errors.New("native activity write failed")
		}
		for n := 0; n < 256; n++ {
			var response struct {
				ID     int             `json:"id"`
				Result json.RawMessage `json:"result"`
				Error  json.RawMessage `json:"error"`
			}
			if c.ReadJSON(&response) != nil {
				return errors.New("native activity read failed")
			}
			if response.ID != id {
				continue
			}
			if len(response.Error) > 0 && string(response.Error) != "null" {
				return fmt.Errorf("native activity method unavailable: %s", method)
			}
			if dst != nil && json.Unmarshal(response.Result, dst) != nil {
				return errors.New("native activity response invalid")
			}
			return nil
		}
		return errors.New("native activity notification limit exceeded")
	}
	if err := call("initialize", map[string]any{"clientInfo": map[string]string{"name": "kyber_activity_observer", "version": "1"}, "capabilities": map[string]bool{"experimentalApi": true}}, nil); err != nil {
		return activity.Snapshot{}, err
	}
	if c.WriteJSON(map[string]any{"method": "initialized", "params": map[string]any{}}) != nil {
		return activity.Snapshot{}, errors.New("native activity initialization failed")
	}
	loaded := func() ([]string, error) {
		var ids []string
		cursor := ""
		for {
			var page struct {
				Data       []string `json:"data"`
				NextCursor *string  `json:"nextCursor"`
			}
			if err := call("thread/loaded/list", map[string]any{"cursor": nullableCursor(cursor), "limit": 100}, &page); err != nil {
				return nil, err
			}
			if page.Data == nil {
				return nil, errors.New("native thread page missing data")
			}
			ids = append(ids, page.Data...)
			if len(ids) > activity.MaxJobs {
				return nil, errors.New("native thread inventory exceeds limit")
			}
			if page.NextCursor == nil {
				break
			}
			if *page.NextCursor == "" || *page.NextCursor == cursor || len(page.Data) == 0 {
				return nil, errors.New("native thread pagination incomplete")
			}
			cursor = *page.NextCursor
		}
		slices.Sort(ids)
		if len(slices.Compact(slices.Clone(ids))) != len(ids) {
			return nil, errors.New("native thread inventory duplicated")
		}
		return ids, nil
	}
	ids, err := loaded()
	if err != nil {
		return activity.Snapshot{}, err
	}
	snapshot := activity.Snapshot{Complete: true}
	for _, threadID := range ids {
		var thread struct {
			Thread struct {
				Status struct {
					Type  string   `json:"type"`
					Flags []string `json:"activeFlags"`
				} `json:"status"`
			} `json:"thread"`
		}
		if err := call("thread/read", map[string]any{"threadId": threadID, "includeTurns": false}, &thread); err != nil {
			return activity.Snapshot{}, err
		}
		switch thread.Thread.Status.Type {
		case "active":
			waiting := false
			for _, flag := range thread.Thread.Status.Flags {
				if flag != "waitingOnApproval" && flag != "waitingOnUserInput" {
					return activity.Snapshot{}, errors.New("unknown native thread active flag")
				}
				waiting = true
			}
			if waiting {
				snapshot.NeedsInput = true
			} else {
				snapshot.Foreground++
			}
		case "idle":
		default:
			return activity.Snapshot{}, errors.New("native thread status unavailable")
		}
		cursor := ""
		seen := map[string]bool{}
		for {
			var page struct {
				Data []struct {
					ProcessID string `json:"processId"`
				} `json:"data"`
				NextCursor *string `json:"nextCursor"`
			}
			if err := call("thread/backgroundTerminals/list", map[string]any{"threadId": threadID, "cursor": nullableCursor(cursor), "limit": 100}, &page); err != nil {
				return activity.Snapshot{}, err
			}
			if page.Data == nil {
				return activity.Snapshot{}, errors.New("native job page missing data")
			}
			for _, job := range page.Data {
				if job.ProcessID == "" || seen[job.ProcessID] {
					return activity.Snapshot{}, errors.New("native job inventory incomplete")
				}
				seen[job.ProcessID] = true
				snapshot.Background++
			}
			if snapshot.Background > activity.MaxJobs {
				return activity.Snapshot{}, errors.New("native job inventory exceeds limit")
			}
			if page.NextCursor == nil {
				break
			}
			if *page.NextCursor == "" || *page.NextCursor == cursor || len(page.Data) == 0 {
				return activity.Snapshot{}, errors.New("native job pagination incomplete")
			}
			cursor = *page.NextCursor
		}
	}
	after, err := loaded()
	if err != nil {
		return activity.Snapshot{}, err
	}
	if !slices.Equal(ids, after) {
		return activity.Snapshot{}, errors.New("native threads changed during inventory")
	}
	snapshot.ObservedAt = observedAt
	return snapshot, nil
}

func nullableCursor(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// RunCodexWorkReporter sends newly collected complete inventories only. On a
// failed native read it sends an explicit incomplete sample rather than
// replaying the previous snapshot with a fresh observation time.
func RunCodexWorkReporter(ctx context.Context, socketPath, sidecarURL string) {
	client := &http.Client{Timeout: 5 * time.Second}
	ticker := time.NewTicker(activity.ObservationInterval)
	defer ticker.Stop()
	var lastWorked time.Time
	for {
		snapshot, err := CodexWorkInventory(ctx, socketPath)
		if err != nil {
			snapshot = activity.Snapshot{ObservedAt: time.Now().UTC(), Complete: false}
		}
		if snapshot.Foreground > 0 || snapshot.Background > 0 {
			lastWorked = snapshot.ObservedAt
		}
		snapshot.LastWorkedAt = lastWorked
		_ = postWorkSnapshot(ctx, client, sidecarURL, snapshot)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

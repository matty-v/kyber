package tokenreport

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type codexEntry struct {
	Type    string `json:"type"`
	Payload struct {
		Type string `json:"type"`
		Info struct {
			Last struct {
				Input      int64 `json:"input_tokens"`
				Cached     int64 `json:"cached_input_tokens"`
				CacheWrite int64 `json:"cache_write_input_tokens"`
			} `json:"last_token_usage"`
			ContextWindow int64 `json:"model_context_window"`
		} `json:"info"`
	} `json:"payload"`
}

type codexActivityEntry struct {
	Timestamp time.Time `json:"timestamp"`
	Type      string    `json:"type"`
	Payload   struct {
		Type string `json:"type"`
	} `json:"payload"`
}

// DetectCodexActivity finds the newest task lifecycle event across persisted
// Codex rollout files. A new rollout file is created on pod restart before a
// task begins, so inspecting only the newest file would erase the last-idle
// timestamp until the next prompt.
func DetectCodexActivity(root string) (state string, at time.Time, err error) {
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() || filepath.Ext(path) != ".jsonl" {
			return nil
		}
		fileState, fileAt, _ := DetectCodexActivityFile(path)
		if fileAt.After(at) {
			state, at = fileState, fileAt
		}
		return nil
	})
	if err != nil {
		return ActivityUnknown, time.Time{}, fmt.Errorf("walking Codex sessions: %w", err)
	}
	if state == "" {
		return ActivityUnknown, time.Time{}, nil
	}
	return state, at, nil
}

// DetectCodexActivityFile returns the newest lifecycle event in one rollout.
// Reporters use this cheap path after one boot-time fleet scan.
func DetectCodexActivityFile(path string) (state string, at time.Time, err error) {
	f, err := os.Open(path)
	if err != nil {
		return ActivityUnknown, time.Time{}, fmt.Errorf("opening %q: %w", path, err)
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 64*1024), 4<<20)
	for s.Scan() {
		var e codexActivityEntry
		if json.Unmarshal(s.Bytes(), &e) != nil || e.Type != "event_msg" || !e.Timestamp.After(at) {
			continue
		}
		switch e.Payload.Type {
		case "task_started":
			state, at = ActivityWorking, e.Timestamp
		case "task_complete":
			state, at = ActivityIdle, e.Timestamp
		}
	}
	if err := s.Err(); err != nil {
		return ActivityUnknown, time.Time{}, fmt.Errorf("scanning %q: %w", path, err)
	}
	if state == "" {
		return ActivityUnknown, time.Time{}, nil
	}
	return state, at, nil
}

// ParseCodexLatest returns the latest token-count observation in a Codex
// rollout JSONL. Cached input is a subset of input, so Used is Input rather
// than Input+Cached; CacheRead is retained as a useful breakdown.
func ParseCodexLatest(path, model string) (*Snapshot, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening %q: %w", path, err)
	}
	defer f.Close()
	var latest *codexEntry
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 64*1024), 4<<20)
	for s.Scan() {
		var e codexEntry
		if json.Unmarshal(s.Bytes(), &e) == nil && e.Type == "event_msg" && e.Payload.Type == "token_count" {
			copy := e
			latest = &copy
		}
	}
	if err := s.Err(); err != nil {
		return nil, fmt.Errorf("scanning %q: %w", path, err)
	}
	if latest == nil {
		return nil, nil
	}
	u := latest.Payload.Info.Last
	limit := latest.Payload.Info.ContextWindow
	percentage := float64(0)
	if limit > 0 {
		percentage = 100 * float64(u.Input) / float64(limit)
	}
	return &Snapshot{Model: model, Tokens: Tokens{Used: u.Input, Limit: limit, Input: u.Input - u.Cached, CacheCreation: u.CacheWrite, CacheRead: u.Cached}, Percentage: percentage, ContextWindowKnown: limit > 0, UpdatedAt: time.Now().UTC()}, nil
}

package tokenreport

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// rawEntry is the shape of a single line in Claude Code's JSONL transcript.
// We only care about assistant messages; other types are ignored.
//
// Schema note: verified 2026-04-15 against Claude Code v2.1.110 transcripts.
// usage.speed is the "finalized" signal ("standard"/"fast", "?" when
// streaming). effortLevel is at the top level for extended-thinking models
// (Opus/Sonnet), absent on Haiku.
type rawEntry struct {
	Type    string `json:"type"`
	Message struct {
		// ID is the API message id (msg_…). Multiple JSONL lines can carry
		// the same message id (one per content block), each repeating the
		// message's usage — the outputTracker dedups on it.
		ID    string `json:"id"`
		Model string `json:"model"`
		Role  string `json:"role"`
		Usage struct {
			InputTokens              int64  `json:"input_tokens"`
			CacheCreationInputTokens int64  `json:"cache_creation_input_tokens"`
			CacheReadInputTokens     int64  `json:"cache_read_input_tokens"`
			OutputTokens             int64  `json:"output_tokens"`
			Speed                    string `json:"speed"`
			ServiceTier              string `json:"service_tier"`
		} `json:"usage"`
	} `json:"message"`
	// EffortLevel is written at the top level of the JSONL line for models
	// that support extended thinking (Opus/Sonnet). Absent on Haiku.
	EffortLevel string `json:"effortLevel"`
	// UUID / LeafUUID are transcript-line identifiers used as dedup-key
	// fallbacks (message.id → uuid → leafUuid) by the outputTracker.
	UUID     string `json:"uuid"`
	LeafUUID string `json:"leafUuid"`
}

// FindLatestSessionFile returns the path to the *.jsonl file in dir with
// the most recent mtime. Claude Code rotates session files on `/clear`,
// so "most recent" is what we want to parse.
//
// Returns an error if dir is empty or cannot be read.
func FindLatestSessionFile(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("reading dir %q: %w", dir, err)
	}
	var best string
	var bestMtime time.Time
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if filepath.Ext(e.Name()) != ".jsonl" {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if best == "" || info.ModTime().After(bestMtime) {
			best = filepath.Join(dir, e.Name())
			bestMtime = info.ModTime()
		}
	}
	if best == "" {
		return "", fmt.Errorf("no *.jsonl files in %q", dir)
	}
	return best, nil
}

// ParseLatest reads the last ~50 lines of dir/file, finds the most recent
// finalized assistant message (speed != "?"), and returns a Snapshot.
// Returns (nil, nil) when no finalized message exists in the file.
func ParseLatest(dir, file string) (*Snapshot, error) {
	path := filepath.Join(dir, file)
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening %q: %w", path, err)
	}
	defer f.Close()

	var lines []string
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 1<<20)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scanning %q: %w", path, err)
	}
	if len(lines) > 50 {
		lines = lines[len(lines)-50:]
	}

	for i := len(lines) - 1; i >= 0; i-- {
		var e rawEntry
		if err := json.Unmarshal([]byte(lines[i]), &e); err != nil {
			continue // skip malformed lines rather than fail the whole file
		}
		if e.Type != "assistant" {
			continue
		}
		// A finalized assistant message has usage.speed set to a concrete value
		// (e.g. "standard", "fast"). Streaming entries have it as "?". Absence
		// of usage tokens also indicates the entry is not usable.
		speed := e.Message.Usage.Speed
		if speed == "?" || speed == "" {
			continue
		}
		used := e.Message.Usage.InputTokens +
			e.Message.Usage.CacheCreationInputTokens +
			e.Message.Usage.CacheReadInputTokens
		// #396: the in-pod reporter no longer computes the per-model limit/pct.
		// The context-window limit is the operator-editable ConfigMap's job and
		// is resolved server-side at serve-time (handleTokenUsageGet), so a
		// ConfigMap correction takes effect for already-running agents without a
		// pod restart. We emit raw Used + Model; Limit=0 / Percentage=0 is the
		// "resolve upstream" sentinel the control plane overwrites. (Keeping the
		// wire-struct shape unchanged keeps the GET contract + PWA stable.)
		return &Snapshot{
			Model: e.Message.Model,
			Tokens: Tokens{
				Used:          used,
				Limit:         0,
				Input:         e.Message.Usage.InputTokens,
				CacheCreation: e.Message.Usage.CacheCreationInputTokens,
				CacheRead:     e.Message.Usage.CacheReadInputTokens,
				// Output is deliberately NOT taken from this single latest
				// message: with several assistant messages per reporter tick
				// that would drop every intermediate message's spend. The
				// Reporter overwrites Tokens.Output with the outputTracker's
				// cumulative total before POSTing.
			},
			Percentage:  0,
			EffortLevel: e.EffortLevel,
			Speed:       speed,
			UpdatedAt:   time.Now().UTC(),
		}, nil
	}
	return nil, nil
}

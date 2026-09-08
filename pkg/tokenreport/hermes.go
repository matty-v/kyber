package tokenreport

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strconv"
	"time"
)

const hermesTimestampLayout = "2006-01-02 15:04:05,000"

const (
	maxHermesLogRead   int64 = 8 << 20
	maxHermesCacheRead int64 = 4 << 20
)

var hermesAPICallPattern = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2},\d{3}) INFO \[([0-9A-Za-z_-]+)\] agent\.conversation_loop: API call #(\d+): model=([^ ]+) provider=([^ ]+) in=(\d+) out=(\d+) total=(\d+) latency=[^ ]+(?: cache=(\d+)/(\d+) \(\d+%\))?$`)

// HermesCatalogModel is the bounded model shape Hermes publishes through the
// existing runtime-catalog endpoint.
type HermesCatalogModel struct {
	ID                 string `json:"id"`
	DisplayName        string `json:"displayName"`
	ContextWindow      int64  `json:"contextWindow"`
	ContextWindowKnown bool   `json:"contextWindowKnown"`
}

type hermesCall struct {
	at, session, model, provider string
	input, output, cacheRead     int64
}

// ParseHermesLatest returns the newest finalized Hermes API-call observation.
// Only the strict agent.conversation_loop summary line is parsed; prompt and
// message-bearing log records are ignored. Output is accumulated for the
// newest session/model so billing deltas are not lost between reporter polls.
func ParseHermesLatest(logPath, metadataPath string) (*Snapshot, error) {
	f, err := os.Open(logPath)
	if err != nil {
		return nil, fmt.Errorf("opening Hermes agent log: %w", err)
	}
	defer f.Close()
	if info, statErr := f.Stat(); statErr == nil && info.Size() > maxHermesLogRead {
		if _, err := f.Seek(info.Size()-maxHermesLogRead, io.SeekStart); err != nil {
			return nil, fmt.Errorf("seeking Hermes agent log tail: %w", err)
		}
	}

	outputs := make(map[string]int64)
	var latest *hermesCall
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 64<<10), 4<<20)
	for s.Scan() {
		call, ok := parseHermesCall(s.Text())
		if !ok {
			continue
		}
		key := call.session + "\x00" + call.model + "\x00" + call.provider
		outputs[key] += call.output
		copy := call
		latest = &copy
	}
	if err := s.Err(); err != nil {
		return nil, fmt.Errorf("scanning Hermes agent log: %w", err)
	}
	if latest == nil {
		return nil, nil
	}

	limit := hermesContextWindow(metadataPath, latest.model)
	percentage := float64(0)
	if limit > 0 {
		percentage = 100 * float64(latest.input) / float64(limit)
	}
	input := latest.input - latest.cacheRead
	if input < 0 {
		input = 0
	}
	key := latest.session + "\x00" + latest.model + "\x00" + latest.provider
	return &Snapshot{
		Model:    latest.model,
		Provider: latest.provider,
		Tokens: Tokens{
			Used:      latest.input,
			Limit:     limit,
			Input:     input,
			CacheRead: latest.cacheRead,
			Output:    outputs[key],
		},
		Percentage:         percentage,
		UpdatedAt:          parseHermesTimestamp(latest.at),
		ContextWindowKnown: limit > 0,
	}, nil
}

func parseHermesCall(line string) (hermesCall, bool) {
	m := hermesAPICallPattern.FindStringSubmatch(line)
	if m == nil {
		return hermesCall{}, false
	}
	parse := func(v string) (int64, bool) {
		n, err := strconv.ParseInt(v, 10, 64)
		return n, err == nil && n >= 0
	}
	input, ok := parse(m[6])
	if !ok {
		return hermesCall{}, false
	}
	output, ok := parse(m[7])
	if !ok {
		return hermesCall{}, false
	}
	cacheRead := int64(0)
	if m[9] != "" {
		cacheRead, ok = parse(m[9])
		if !ok || cacheRead > input {
			return hermesCall{}, false
		}
	}
	return hermesCall{at: m[1], session: m[2], model: m[4], provider: m[5], input: input, output: output, cacheRead: cacheRead}, true
}

func parseHermesTimestamp(value string) time.Time {
	at, err := time.ParseInLocation(hermesTimestampLayout, value, time.Local)
	if err != nil {
		return time.Now().UTC()
	}
	return at.UTC()
}

type hermesModelMetadata struct {
	ContextLength int64  `json:"context_length"`
	Name          string `json:"name"`
}

func readHermesMetadata(path string) (map[string]hermesModelMetadata, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var metadata map[string]hermesModelMetadata
	if err := json.NewDecoder(io.LimitReader(f, maxHermesCacheRead)).Decode(&metadata); err != nil {
		return nil, err
	}
	return metadata, nil
}

func hermesContextWindow(path, model string) int64 {
	metadata, err := readHermesMetadata(path)
	if err != nil {
		return 0
	}
	return metadata[model].ContextLength
}

// LoadHermesCatalog loads Hermes's own OpenRouter-compatible model cache and
// enriches it from Hermes's OpenRouter metadata cache. The result is sorted
// for a stable operator picker and capped before it crosses the sidecar.
func LoadHermesCatalog(providerCachePath, metadataPath string, limit int) ([]HermesCatalogModel, error) {
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	f, err := os.Open(providerCachePath)
	if err != nil {
		return nil, fmt.Errorf("opening Hermes provider cache: %w", err)
	}
	defer f.Close()
	var cache map[string]struct {
		Models []string `json:"models"`
	}
	if err := json.NewDecoder(io.LimitReader(f, maxHermesCacheRead)).Decode(&cache); err != nil {
		return nil, fmt.Errorf("decoding Hermes provider cache: %w", err)
	}
	metadata, err := readHermesMetadata(metadataPath)
	if err != nil {
		return nil, fmt.Errorf("reading Hermes model metadata: %w", err)
	}
	ids := append([]string(nil), cache["openrouter"].Models...)
	sort.Strings(ids)
	seen := make(map[string]struct{}, len(ids))
	models := make([]HermesCatalogModel, 0, min(len(ids), limit))
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		m, ok := metadata[id]
		if !ok || m.ContextLength <= 0 {
			continue
		}
		name := m.Name
		if name == "" {
			name = id
		}
		models = append(models, HermesCatalogModel{ID: id, DisplayName: name, ContextWindow: m.ContextLength, ContextWindowKnown: true})
		if len(models) == limit {
			break
		}
	}
	return models, nil
}

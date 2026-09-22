package tokenreport

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
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

// EndpointContextWindow asks an OpenAI-compatible endpoint what context window
// it is actually serving.
//
// A self-hosted endpoint publishes no OpenRouter metadata, so it is the only
// authoritative source for the window. Without it the token snapshot carries
// limit 0 (the UI shows "Context: -") and, because Hermes declares
// RequireCatalogContext, the control plane rejects the agent's whole model
// catalog rather than storing a guess.
//
// llama.cpp reports the SERVED window as data[].meta.n_ctx, which is what
// matters: n_ctx_train is what the model was trained for and is routinely far
// larger than what the server was started with. Returns 0 when the endpoint
// does not publish it — a guess would be worse than an honest unknown.
func EndpointContextWindow(ctx context.Context, client *http.Client, baseURL, apiKey string) (map[string]int64, error) {
	endpoint := strings.TrimSuffix(baseURL, "/") + "/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s returned %d", endpoint, resp.StatusCode)
	}
	var body struct {
		Data []struct {
			ID   string `json:"id"`
			Meta struct {
				NCtx int64 `json:"n_ctx"`
			} `json:"meta"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxHermesCacheRead)).Decode(&body); err != nil {
		return nil, fmt.Errorf("decoding %s: %w", endpoint, err)
	}
	// Keyed by model id, NOT a single value: the provider cache is a list
	// because an endpoint can serve several models (llama-swap, vLLM
	// multi-model). Stamping the first model's window onto all of them would
	// budget a small model against a window several times its real size —
	// exactly the guess this function refuses to make when the endpoint
	// publishes nothing.
	windows := make(map[string]int64, len(body.Data))
	for _, m := range body.Data {
		if m.ID != "" && m.Meta.NCtx > 0 {
			windows[m.ID] = m.Meta.NCtx
		}
	}
	return windows, nil
}

// openRouterProvider is the built-in provider a Hermes agent uses when it has
// no custom inference endpoint. Its metadata cache is authoritative for it and
// for nothing else.
const openRouterProvider = "openrouter"

type hermesCall struct {
	at, session, model, provider string
	input, output, cacheRead     int64
}

// ParseHermesLatest returns the newest finalized Hermes API-call observation.
// Only the strict agent.conversation_loop summary line is parsed; prompt and
// message-bearing log records are ignored. Output is accumulated for the
// newest session/model so billing deltas are not lost between reporter polls.
func ParseHermesLatest(logPath, metadataPath string, endpointWindows map[string]int64) (*Snapshot, error) {
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
	if limit == 0 && latest.provider != openRouterProvider {
		// A self-hosted model has no OpenRouter metadata, so the endpoint's
		// own answer is the only source. Without it the snapshot carries
		// limit 0 and the UI shows "Context: -".
		//
		// Scoped to the provider on the line being reported. The agent log is
		// durable across a switch onto an endpoint, so an OpenRouter line left
		// in it would otherwise be budgeted against the ENDPOINT's window and
		// asserted as known — a 131k model reported against 65k reads as
		// twice the usage it really is.
		limit = endpointWindows[latest.model]
	}
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

// LoadHermesCatalog loads the model ids Hermes cached for the ACTIVE provider
// and enriches them from Hermes's OpenRouter metadata cache. The result is
// sorted for a stable operator picker and capped before it crosses the
// sidecar.
//
// provider selects which entry of Hermes's provider cache to read. It used to
// be the literal "openrouter", which meant an agent pointed at its own
// inference endpoint reported an empty catalog and got an empty model picker.
//
// Metadata is OpenRouter-specific. For the OpenRouter provider it is
// authoritative, so an id it does not describe is a stale or junk entry and is
// still dropped — unchanged behaviour. For any other provider there is no
// metadata to be authoritative: a self-hosted endpoint publishes none, so
// dropping on absence returned an empty catalog even when the endpoint listed
// its models perfectly well. Those are kept with ContextWindowKnown=false,
// which the API already carries end to end (routes_available.go passes the
// flag straight through).
func LoadHermesCatalog(providerCachePath, metadataPath, provider string, endpointWindows map[string]int64, limit int) ([]HermesCatalogModel, error) {
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	if provider == "" {
		provider = openRouterProvider
	}
	// OpenRouter's metadata cache is authoritative for OpenRouter and for
	// nothing else. Decided up front because the read below branches on it.
	metadataIsAuthoritative := provider == openRouterProvider
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
		// A custom endpoint never talks to OpenRouter, so nothing ever writes
		// its metadata cache. Failing here returned an error on every poll and
		// left the model picker empty — the failure this whole path exists to
		// remove. For OpenRouter the file IS expected, so a missing one stays
		// an error rather than silently publishing an empty catalog over a
		// good one.
		if !errors.Is(err, fs.ErrNotExist) || metadataIsAuthoritative {
			return nil, fmt.Errorf("reading Hermes model metadata: %w", err)
		}
		metadata = nil
	}
	ids := append([]string(nil), cache[provider].Models...)
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
		known := ok && m.ContextLength > 0
		if !known && metadataIsAuthoritative {
			continue
		}
		name := m.Name
		if name == "" {
			name = id
		}
		window := int64(0)
		switch {
		case known:
			window = m.ContextLength
		case endpointWindows[id] > 0:
			// The endpoint's own answer. Reporting it is what lets the
			// control plane accept this catalog at all: Hermes declares
			// RequireCatalogContext, so a model with an unknown window makes
			// it reject the WHOLE catalog, which left the model picker empty
			// and the reporter retrying forever.
			window = endpointWindows[id]
			known = true
		}
		models = append(models, HermesCatalogModel{ID: id, DisplayName: name, ContextWindow: window, ContextWindowKnown: known})
		if len(models) == limit {
			break
		}
	}
	return models, nil
}

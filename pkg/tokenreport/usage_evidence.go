package tokenreport

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ErrIncompleteUsage marks a finalized source observation that cannot be
// counted safely. Reporters must publish a coverage gap for this condition.
var ErrIncompleteUsage = errors.New("tokenreport: incomplete usage evidence")

var safeSourcePart = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// UsageEvidence contains only source-native, non-content fields. It is not an
// API event: the control plane adds the authenticated agent UID, route and
// ingestion metadata. Nil token classes mean that the source did not report
// them, while a pointer to zero means an observed zero.
type UsageEvidence struct {
	Runtime               string
	SourceIdentity        string
	ObservedAt            time.Time
	Model                 string
	ModelEvidence         string
	ReportedProviderLabel string
	InputUncached         *int64
	Output                *int64
	CacheRead             *int64
	CacheWrite            *int64
}

func tokenCount(n int64) *int64 { return &n }

// ParseClaudeUsageLine accepts one finalized assistant message. A transcript
// line without a stable message ID or timestamp cannot enter usage totals.
// sourceNamespace must identify the session, not a filesystem path sent to
// the control plane. Repeated content-block lines produce the same identity.
func ParseClaudeUsageLine(line []byte, sourceNamespace string) (*UsageEvidence, error) {
	var e rawEntry
	if err := json.Unmarshal(line, &e); err != nil {
		return nil, ErrIncompleteUsage
	}
	if e.Type != "assistant" {
		return nil, nil
	}
	if e.Message.Usage.Speed == "?" || e.Message.Usage.Speed == "" {
		return nil, nil
	}
	var fields struct {
		Message struct {
			Usage struct {
				Input      *int64 `json:"input_tokens"`
				CacheRead  *int64 `json:"cache_read_input_tokens"`
				CacheWrite *int64 `json:"cache_creation_input_tokens"`
				Output     *int64 `json:"output_tokens"`
			} `json:"usage"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &fields) != nil || !safeSourcePart.MatchString(sourceNamespace) || !safeSourcePart.MatchString(e.Message.ID) || e.Timestamp.IsZero() || e.Message.Model == "" {
		return nil, ErrIncompleteUsage
	}
	u := fields.Message.Usage
	if u.Input == nil && u.CacheRead == nil && u.CacheWrite == nil && u.Output == nil {
		return nil, ErrIncompleteUsage
	}
	for _, n := range []*int64{u.Input, u.CacheRead, u.CacheWrite, u.Output} {
		if n != nil && *n < 0 {
			return nil, ErrIncompleteUsage
		}
	}
	return &UsageEvidence{
		Runtime: "claude", SourceIdentity: sourceNamespace + ":" + e.Message.ID,
		ObservedAt: e.Timestamp.UTC(), Model: e.Message.Model, ModelEvidence: "observed",
		InputUncached: u.Input, Output: u.Output, CacheRead: u.CacheRead, CacheWrite: u.CacheWrite,
	}, nil
}

// CodexUsageObservation is a cumulative rollout counter, not a billable call.
// Repeated token_count lines can describe the same call. The collector must
// derive a positive delta from Total and use a durable rollout cursor before
// turning one into a UsageEvidence event.
type CodexUsageObservation struct {
	ObservedAt time.Time
	Last       UsageEvidence
	Total      UsageEvidence
}

// ParseCodexUsageLine retains both last and cumulative token counts. It does
// not assign an event identity because a rollout byte offset is an observation
// identity, not proof that a new model call happened.
func ParseCodexUsageLine(line []byte, configuredModel string) (*CodexUsageObservation, error) {
	var envelope struct {
		Timestamp time.Time `json:"timestamp"`
		Type      string    `json:"type"`
		Payload   struct {
			Type string          `json:"type"`
			Info json.RawMessage `json:"info"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(line, &envelope); err != nil {
		return nil, ErrIncompleteUsage
	}
	if envelope.Type != "event_msg" || envelope.Payload.Type != "token_count" {
		return nil, nil
	}
	var info struct {
		Last *struct {
			Input      *int64 `json:"input_tokens"`
			Cached     *int64 `json:"cached_input_tokens"`
			CacheWrite *int64 `json:"cache_write_input_tokens"`
			Output     *int64 `json:"output_tokens"`
		} `json:"last_token_usage"`
		Total *struct {
			Input      *int64 `json:"input_tokens"`
			Cached     *int64 `json:"cached_input_tokens"`
			CacheWrite *int64 `json:"cache_write_input_tokens"`
			Output     *int64 `json:"output_tokens"`
		} `json:"total_token_usage"`
	}
	if envelope.Timestamp.IsZero() || configuredModel == "" || json.Unmarshal(envelope.Payload.Info, &info) != nil || info.Last == nil || info.Total == nil ||
		invalidCodexCounts(info.Last.Input, info.Last.Cached, info.Last.CacheWrite, info.Last.Output) ||
		invalidCodexCounts(info.Total.Input, info.Total.Cached, info.Total.CacheWrite, info.Total.Output) {
		return nil, ErrIncompleteUsage
	}
	makeEvidence := func(input, cached, cacheWrite, output *int64) UsageEvidence {
		var uncached *int64
		if input != nil && cached != nil {
			uncached = tokenCount(*input - *cached)
		}
		return UsageEvidence{Runtime: "codex", ObservedAt: envelope.Timestamp.UTC(), Model: configuredModel, ModelEvidence: "configured",
			InputUncached: uncached, Output: output, CacheRead: cached, CacheWrite: cacheWrite}
	}
	return &CodexUsageObservation{ObservedAt: envelope.Timestamp.UTC(),
		Last:  makeEvidence(info.Last.Input, info.Last.Cached, info.Last.CacheWrite, info.Last.Output),
		Total: makeEvidence(info.Total.Input, info.Total.Cached, info.Total.CacheWrite, info.Total.Output)}, nil
}

func invalidCodexCounts(input, cached, cacheWrite, output *int64) bool {
	if input == nil && cached == nil && cacheWrite == nil && output == nil {
		return true
	}
	for _, n := range []*int64{input, cached, cacheWrite, output} {
		if n != nil && *n < 0 {
			return true
		}
	}
	return input != nil && cached != nil && *cached > *input
}

// ParseHermesUsageLine reads only the strict API-call summary log record.
// Hermes's provider label names its logical route and is never an observed
// upstream inference provider.
func ParseHermesUsageLine(line, sourceNamespace string, location *time.Location) (*UsageEvidence, error) {
	call, ok := parseHermesCall(line)
	if !ok {
		if strings.Contains(line, "agent.conversation_loop: API call #") {
			return nil, ErrIncompleteUsage
		}
		return nil, nil
	}
	if !safeSourcePart.MatchString(sourceNamespace) || location == nil || call.model == "" || call.provider == "" {
		return nil, ErrIncompleteUsage
	}
	at, err := time.ParseInLocation(hermesTimestampLayout, call.at, location)
	if err != nil {
		return nil, fmt.Errorf("parsing Hermes usage time: %w", ErrIncompleteUsage)
	}
	callNumber, err := strconv.ParseUint(call.callNumber, 10, 64)
	if err != nil {
		return nil, ErrIncompleteUsage
	}
	input := call.input - call.cacheRead
	e := &UsageEvidence{Runtime: "hermes", SourceIdentity: fmt.Sprintf("%s:%s:%d", sourceNamespace, call.session, callNumber),
		ObservedAt: at.UTC(), Model: call.model, ModelEvidence: "observed", ReportedProviderLabel: call.provider,
		InputUncached: tokenCount(input), Output: tokenCount(call.output), CacheWrite: nil}
	// The optional cache=read/total segment is the only local evidence for
	// Hermes cache reads. Its absence is unknown, not an observed zero.
	if hermesAPICallPattern.FindStringSubmatch(line)[9] != "" {
		e.CacheRead = tokenCount(call.cacheRead)
	}
	return e, nil
}

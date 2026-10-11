// Package usagestore persists local harness usage evidence without prices.
package usagestore

import (
	"errors"
	"regexp"
	"time"

	"github.com/google/uuid"
)

var (
	ErrInvalid     = errors.New("usagestore: invalid usage evidence")
	ErrConflict    = errors.New("usagestore: conflicting source identity")
	ErrUnavailable = errors.New("usagestore: durable storage unavailable")
)

const MaxRawUsageFields = 12

var safeIdentity = regexp.MustCompile(`^[A-Za-z0-9_:-]{1,256}$`)

// Event is an immutable local observation. AgentUID must be resolved by the
// authenticated control plane caller, never copied from a reporter request.
// Null token fields mean unknown; a pointer to zero is an observed zero.
type Event struct {
	ID                       string           `json:"id"`
	AgentUID                 string           `json:"agentUID"`
	Agent                    string           `json:"agent"`
	ObservedAt               time.Time        `json:"observedAt"`
	IngestedAt               time.Time        `json:"ingestedAt"`
	Runtime                  string           `json:"runtime"`
	SourceFormat             string           `json:"sourceFormat"`
	SourceVersion            string           `json:"sourceVersion"`
	SourceIdentity           string           `json:"sourceIdentity"`
	Coverage                 string           `json:"coverage"`
	ConfiguredRoute          string           `json:"configuredRoute"`
	RouteID                  string           `json:"routeID"`
	Model                    string           `json:"model"`
	ModelEvidence            string           `json:"modelEvidence"`
	ObservedUpstreamProvider *string          `json:"observedUpstreamProvider"`
	ReportedProviderLabel    *string          `json:"reportedProviderLabel"`
	InputUncached            *int64           `json:"inputUncached"`
	Output                   *int64           `json:"output"`
	CacheRead                *int64           `json:"cacheRead"`
	CacheWrite               *int64           `json:"cacheWrite"`
	RawUsage                 map[string]int64 `json:"rawUsage,omitempty"`
	SupersedesID             string           `json:"supersedesID,omitempty"`
}

func (e Event) Validate() error {
	if e.ID != "" || !e.IngestedAt.IsZero() || e.AgentUID == "" || e.Agent == "" || e.ObservedAt.IsZero() ||
		e.Runtime == "" || e.SourceFormat == "" || e.SourceVersion == "" || e.SourceIdentity == "" || e.Coverage != "observed" ||
		e.ConfiguredRoute == "" || e.RouteID == "" || e.Model == "" ||
		(e.ModelEvidence != "observed" && e.ModelEvidence != "configured") {
		return ErrInvalid
	}
	if len(e.AgentUID) > 128 || len(e.Agent) > 253 || len(e.Runtime) > 32 || len(e.SourceFormat) > 64 ||
		len(e.SourceVersion) > 64 || len(e.SourceIdentity) > 256 || len(e.RouteID) > 128 || len(e.Model) > 256 {
		return ErrInvalid
	}
	if !safeIdentity.MatchString(e.SourceIdentity) {
		return ErrInvalid
	}
	if _, err := uuid.Parse(e.RouteID); err != nil {
		return ErrInvalid
	}
	switch e.Runtime {
	case "claude", "codex", "hermes":
		// The supported local sources do not reveal the selected upstream
		// inference provider. A route label must never be promoted to one.
		if e.ObservedUpstreamProvider != nil {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	if e.Runtime != "hermes" && e.ReportedProviderLabel != nil {
		return ErrInvalid
	}
	switch e.ConfiguredRoute {
	case "native", "direct-api", "openrouter", "custom":
	default:
		return ErrInvalid
	}
	if e.ObservedUpstreamProvider != nil && (*e.ObservedUpstreamProvider == "" || len(*e.ObservedUpstreamProvider) > 128) {
		return ErrInvalid
	}
	if e.ReportedProviderLabel != nil && (*e.ReportedProviderLabel == "" || len(*e.ReportedProviderLabel) > 128) {
		return ErrInvalid
	}
	if e.InputUncached == nil && e.Output == nil && e.CacheRead == nil && e.CacheWrite == nil && len(e.RawUsage) == 0 {
		return ErrInvalid
	}
	for _, n := range []*int64{e.InputUncached, e.Output, e.CacheRead, e.CacheWrite} {
		if n != nil && *n < 0 {
			return ErrInvalid
		}
	}
	if len(e.RawUsage) > MaxRawUsageFields {
		return ErrInvalid
	}
	for key, value := range e.RawUsage {
		switch key {
		case "input", "output", "cacheRead", "cacheWrite", "reasoningOutput":
		default:
			return ErrInvalid
		}
		if value < 0 {
			return ErrInvalid
		}
	}
	return nil
}

// Gap records a detected interval or malformed source observation. Count is
// deliberately absent: a gap cannot prove how many calls were lost.
type Gap struct {
	AgentUID       string    `json:"agentUID"`
	Runtime        string    `json:"runtime"`
	SourceFormat   string    `json:"sourceFormat"`
	SourceIdentity string    `json:"sourceIdentity"`
	DetectedAt     time.Time `json:"detectedAt"`
	Reason         string    `json:"reason"`
}

func (g Gap) Validate() error {
	if g.AgentUID == "" || g.Runtime == "" || g.SourceFormat == "" || g.SourceIdentity == "" || g.DetectedAt.IsZero() ||
		len(g.AgentUID) > 128 || len(g.Runtime) > 32 || len(g.SourceFormat) > 64 || len(g.SourceIdentity) > 256 {
		return ErrInvalid
	}
	if !safeIdentity.MatchString(g.SourceIdentity) {
		return ErrInvalid
	}
	switch g.Reason {
	case "malformed", "missing_identity", "missing_time", "truncated", "unsupported_version", "cursor_gap":
		return nil
	default:
		return ErrInvalid
	}
}

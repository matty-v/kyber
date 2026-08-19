package runtimedetect

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// DefaultCadence is the default poll interval — once an hour per the
// design doc. Configurable via the chart (runtimeDetect.cadenceSeconds).
const DefaultCadence = time.Hour

// ContextWindowResolver enriches detected models with operator-supplied
// context-window values. Implemented by pkg/contextwindowmap.Resolver in
// PR-D — defined here as an interface so pkg/runtimedetect doesn't take a
// dependency on the contextwindowmap package. A nil resolver is fine; the
// poller writes ContextWindowFloor + Known=false for every model in that
// case (the PR-A default).
type ContextWindowResolver interface {
	LookupOr(ctx context.Context, modelID string) (int64, bool)
}

// Poller drives the detection loop. It runs as a controller-runtime
// Runnable: register with mgr.Add(manager.RunnableFunc(p.Start)) and the
// loop shuts down on ctx.Done() during pod termination.
type Poller struct {
	// Cache is where each poll result lands; the /available handler reads
	// from this same cache so any control-plane replica returns the same
	// blob.
	Cache Cache
	// Npm is the npm registry client. Required.
	Npm *NpmClient
	// CodexNpm fetches @openai/codex versions. Nil keeps backward-compatible
	// Claude-only behavior for tests and installs that have not configured it.
	CodexNpm *NpmClient
	// Anthropic is the legacy platform-level Anthropic Models API client.
	// Nil disables this leg; authenticated agent pods report their own catalogs.
	Anthropic *AnthropicClient
	// KeySource returns the operator-supplied Anthropic API key per poll
	// cycle. When the source returns "", the Anthropic leg is skipped and
	// the snapshot is npm-only (cached models remain from the previous
	// successful poll, if any).
	KeySource KeySource
	// ContextWindows enriches the detected model list with operator
	// overrides before writing the snapshot to the cache. Nil → every
	// model keeps the floor + ContextWindowKnown=false (PR-A default).
	// PR-D wires this to pkg/contextwindowmap.Resolver.
	ContextWindows ContextWindowResolver
	// Cadence is the interval between polls. Falls back to DefaultCadence
	// when <= 0.
	Cadence time.Duration
	// VersionLimit caps the number of CC versions surfaced. Falls back to
	// DefaultVersionLimit when <= 0.
	VersionLimit int
	// Logger receives soft-failure WARN messages. Nil → slog.Default.
	Logger *slog.Logger
}

// Start runs the poll loop until ctx is canceled. Conforms to
// controller-runtime's manager.RunnableFunc signature.
//
// Behavior:
//   - First poll fires immediately so /available is populated within
//     seconds of startup (rather than waiting a full cadence).
//   - Each poll calls npm + Anthropic, merges with the last cached
//     snapshot, and writes the merged result. Either leg failing keeps the
//     last good value for that leg.
//   - All errors are logged at WARN; the loop never exits on a poll error.
func (p *Poller) Start(ctx context.Context) error {
	if err := p.validate(); err != nil {
		return err
	}
	logger := p.logger()
	cadence := p.cadence()
	logger.Info("runtimedetect: poller starting", "cadenceSeconds", int(cadence.Seconds()))

	// Fire once immediately so /available has data on startup, then on
	// the cadence ticker.
	p.pollOnce(ctx, logger)

	ticker := time.NewTicker(cadence)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			logger.Info("runtimedetect: poller stopping")
			return nil
		case <-ticker.C:
			p.pollOnce(ctx, logger)
		}
	}
}

// pollOnce executes one detection cycle. Exported as PollOnce on the type
// (via the public wrapper below) so tests can drive a single cycle without
// spinning up the goroutine.
func (p *Poller) pollOnce(ctx context.Context, logger *slog.Logger) {
	previous, _ := p.Cache.Get(ctx)

	versions, vErr := p.Npm.Fetch(ctx, p.versionLimit())
	if vErr != nil {
		logger.Warn("runtimedetect: npm fetch failed; preserving last good", "err", vErr)
		if previous != nil {
			versions = previous.ClaudeCodeVersions
		}
	}
	var codexVersions []string
	if p.CodexNpm != nil {
		codexVersions, vErr = p.CodexNpm.Fetch(ctx, p.versionLimit())
		if vErr != nil {
			logger.Warn("runtimedetect: codex npm fetch failed; preserving last good", "err", vErr)
			if previous != nil {
				codexVersions = previous.CodexVersions
			}
		}
	} else if previous != nil {
		codexVersions = previous.CodexVersions
	}

	var models []Model
	var apiKey string
	var kErr error
	if p.KeySource != nil {
		apiKey, kErr = p.KeySource()
	}
	if kErr != nil {
		logger.Warn("runtimedetect: anthropic key source error", "err", kErr)
	}
	if apiKey != "" && p.Anthropic != nil {
		fetched, mErr := p.Anthropic.Fetch(ctx, apiKey)
		switch {
		case mErr == nil:
			models = fetched
		case errors.Is(mErr, ErrAnthropicKeyMissing):
			// Source returned a key but the client disagreed — shouldn't
			// happen, but log and keep last good.
			logger.Warn("runtimedetect: anthropic key disagreement", "err", mErr)
			if previous != nil {
				models = previous.Models
			}
		default:
			// Network / 401 / 5xx: keep last good model list.
			logger.Warn("runtimedetect: anthropic fetch failed; preserving last good", "err", mErr)
			if previous != nil {
				models = previous.Models
			}
		}
	} else {
		// No key configured. Keep the last cached models (if any) so a
		// transient key-mount glitch doesn't blank the picker.
		if previous != nil {
			models = previous.Models
		}
	}

	// Apply the operator-supplied context-window override on top of the
	// auto-detected window. Override-only (kyber#488): the ConfigMap wins
	// ONLY when it carries an entry for the model; when it doesn't, we keep
	// the window the Anthropic API already gave us (max_input_tokens) rather
	// than clobbering it back to the floor. Re-applies every cycle so a
	// ConfigMap edit propagates within one cadence + ResolveCacheTTL. Nil
	// resolver leaves the auto-detected values untouched.
	if p.ContextWindows != nil && len(models) > 0 {
		for i := range models {
			if cw, known := p.ContextWindows.LookupOr(ctx, models[i].ID); known {
				models[i].ContextWindow = cw
				models[i].ContextWindowKnown = true
			}
		}
	}

	// If both legs failed AND we had no previous snapshot, write nothing —
	// the cache stays in ErrCacheEmpty state and /available returns the
	// empty fallback.
	if versions == nil && models == nil {
		return
	}

	snap := &Snapshot{
		ClaudeCodeVersions: versions,
		CodexVersions:      codexVersions,
		Models:             models,
		FetchedAt:          time.Now().UTC(),
	}
	if previous != nil {
		snap.CodexModels = previous.CodexModels
		snap.AgentModels = previous.AgentModels
	}
	if err := p.Cache.Put(ctx, snap); err != nil {
		logger.Warn("runtimedetect: cache put failed", "err", err)
	}
}

// PollOnce runs one poll cycle synchronously. Tests use this to assert
// the cache state without driving the ticker loop.
func (p *Poller) PollOnce(ctx context.Context) {
	if err := p.validate(); err != nil {
		// Tests will surface this via the empty cache; keeping PollOnce
		// non-error matches the production loop's "log and continue"
		// posture so test setup mistakes don't masquerade as upstream
		// errors.
		p.logger().Error("runtimedetect: PollOnce validation failed", "err", err)
		return
	}
	p.pollOnce(ctx, p.logger())
}

func (p *Poller) validate() error {
	if p.Cache == nil {
		return fmt.Errorf("runtimedetect: Poller.Cache is required")
	}
	if p.Npm == nil {
		return fmt.Errorf("runtimedetect: Poller.Npm is required")
	}
	return nil
}

func (p *Poller) cadence() time.Duration {
	if p.Cadence <= 0 {
		return DefaultCadence
	}
	return p.Cadence
}

func (p *Poller) versionLimit() int {
	if p.VersionLimit <= 0 {
		return DefaultVersionLimit
	}
	return p.VersionLimit
}

func (p *Poller) logger() *slog.Logger {
	if p.Logger == nil {
		return slog.Default()
	}
	return p.Logger
}

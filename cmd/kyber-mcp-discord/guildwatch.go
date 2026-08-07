package main

import (
	"context"
	"log/slog"
	"sort"
	"sync/atomic"
	"time"

	"github.com/bwmarrin/discordgo"
)

// Guild readiness and late-join recovery (kyber#674).
//
// The failure this exists to prevent, observed in production:
// a brand-new agent's sidecar opened its Gateway session at 19:24:42, and the
// bot was only invited to the Discord server at 19:28:38 — nearly four minutes
// LATER. From then on the sidecar was permanently deaf: it had logged
// "gateway connected", it never errored, it never dropped anything (the drop
// tallies only count messages that actually reach handleMessage), and it never
// forwarded anything. A message @-mentioning the agent 26 seconds after the
// invite reached a DIFFERENT agent's sidecar in the same channel and never
// reached this one. From every observable surface the agent looked healthy and
// idle.
//
// Two independent defects, fixed independently here because either one alone
// still leaves an operator stuck:
//
//  1. RECOVERY. A session identified while the bot belonged to no relevant
//     server does not reliably receive that server's messages once it joins.
//     When a configured guild shows up after we were missing it, reconnect —
//     a fresh identify with membership already in place is the known-good
//     state (it is exactly how every working sidecar starts).
//
//  2. VISIBILITY. "Connected but in none of my configured guilds" is a
//     terminal, operator-actionable misconfiguration — the bot was never
//     invited — and it MUST NOT look like a healthy idle agent. It is logged
//     loudly and fails the health check.
//
// Deliberately conservative: guild membership is read from the session's local
// state cache, populated by GUILD_CREATE, so the steady-state cost is zero
// Discord API calls and this can never contribute to a rate limit.

const (
	// guildCheckInterval is how often membership is re-evaluated.
	guildCheckInterval = 30 * time.Second
	// guildGracePeriod is how long to wait after connecting before trusting a
	// "not a member" reading. GUILD_CREATE events stream in just after
	// identify, so an immediate check would false-alarm on every healthy boot.
	guildGracePeriod = 15 * time.Second
	// guildWarnEvery re-logs a persistent misconfiguration on this cadence, so
	// the reason stays discoverable in a log tail long after the transition.
	guildWarnEvery = 5 * time.Minute
)

// guildWatcher tracks whether the session is a member of every configured
// guild, and recovers the session when membership arrives late.
type guildWatcher struct {
	// allowed is the configured guild allowlist. Empty means unconstrained —
	// the sidecar accepts any guild it is in, so there is nothing to verify
	// and the watcher reports ready.
	allowed map[string]bool
	ready   atomic.Bool
	// sawMissing records that we have observed a configured guild to be
	// absent, which is what makes a later appearance a LATE JOIN (needing a
	// reconnect) rather than an ordinary healthy startup. Atomic so tests can
	// observe it without racing the watcher goroutine.
	sawMissing atomic.Bool
}

// sawMissingSnapshot reports whether a configured guild has been observed
// absent. Test-facing; the watcher itself reads the field directly.
func (w *guildWatcher) sawMissingSnapshot() bool { return w.sawMissing.Load() }

func newGuildWatcher(allowed map[string]bool) *guildWatcher {
	w := &guildWatcher{allowed: allowed}
	// Unconstrained installs have nothing to wait for; start ready so the
	// health check does not fail closed on a valid configuration.
	w.ready.Store(len(allowed) == 0)
	return w
}

// isReady reports whether the sidecar is a member of every configured guild.
// Wired into /healthz so a never-invited bot surfaces as unhealthy instead of
// idle.
func (w *guildWatcher) isReady() bool { return w.ready.Load() }

// missingGuilds returns the configured guild IDs the session is not currently
// a member of, sorted for stable log output. Reads the local state cache only.
func (w *guildWatcher) missingGuilds(s *discordgo.Session) []string {
	if len(w.allowed) == 0 {
		return nil
	}
	joined := make(map[string]bool)
	if s != nil && s.State != nil {
		// discordgo mutates State.Guilds from its own gateway goroutine as
		// GUILD_CREATE / GUILD_DELETE arrive — precisely the events this
		// watcher exists to notice — so reading the slice unlocked is a real
		// data race, not a theoretical one. State embeds sync.RWMutex for this.
		s.State.RLock()
		for _, g := range s.State.Guilds {
			if g != nil {
				joined[g.ID] = true
			}
		}
		s.State.RUnlock()
	}
	var missing []string
	for id := range w.allowed {
		if !joined[id] {
			missing = append(missing, id)
		}
	}
	sort.Strings(missing)
	return missing
}

// run evaluates guild membership until ctx is done, flipping readiness,
// logging a persistent misconfiguration, and invoking reconnect exactly once
// per late join.
//
// reconnect must re-establish the Gateway session; it is called from this
// goroutine and may block.
func (w *guildWatcher) run(ctx context.Context, s *discordgo.Session, reconnect func(), interval, grace time.Duration) {
	// Let GUILD_CREATE land before the first reading, or every healthy boot
	// would log a spurious "not a member" warning.
	select {
	case <-ctx.Done():
		return
	case <-time.After(grace):
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	var lastWarn time.Time

	for {
		missing := w.missingGuilds(s)
		switch {
		case len(missing) == 0:
			if w.sawMissing.Load() {
				// LATE JOIN. The session was identified while this guild was
				// absent, which is the state that goes silently deaf. Rebuild
				// it now that membership exists.
				slog.Info("discord-sidecar: configured guild became available after connect — " +
					"reconnecting the gateway so its messages are delivered")
				w.sawMissing.Store(false)
				if reconnect != nil {
					reconnect()
				}
			}
			if !w.ready.Swap(true) {
				slog.Info("discord-sidecar: guild membership verified", "guilds", len(w.allowed))
			}
		default:
			w.sawMissing.Store(true)
			w.ready.Store(false)
			if time.Since(lastWarn) >= guildWarnEvery {
				lastWarn = time.Now()
				// Loud on purpose: inbound can NEVER arrive in this state, and
				// the sidecar otherwise looks identical to a healthy idle one.
				slog.Warn("discord-sidecar: connected but NOT a member of configured guild(s) — "+
					"no inbound message can reach this agent. Invite the bot to the server; "+
					"recovery is automatic once it joins.",
					"missing_guild_ids", missing)
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

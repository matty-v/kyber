package main

import (
	"log/slog"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
)

// botRoleResolver reports the set of role IDs that stand in for the bot itself
// in a guild — mentioning any of them is mentioning the bot. A nil result is
// valid and means "no role information"; callers treat it as "no role mention
// matches", which is exactly the behaviour before role mentions were honoured.
type botRoleResolver func(guildID, botID string) map[string]bool

// defaultRoleRetryAfter bounds how often a guild whose role lookup failed (or
// came back empty) is retried. Long enough that a permissions problem can't
// turn into per-message API traffic, short enough that granting the bot a role
// takes effect without a pod restart.
const defaultRoleRetryAfter = 5 * time.Minute

// botRoleCache resolves, and remembers, the roles the bot holds in each guild.
//
// The set comes from GET /guilds/{guild}/members/{bot} — a single-member read
// that needs no privileged intent (unlike the guild-wide member list, which
// requires GUILD_MEMBERS). It is cached because it changes only when a server
// admin re-roles the bot; resolving per message would be both wasteful and
// rate-limit bait.
//
// The lock is deliberately held across the API call. Contention is a non-issue
// at one call per guild per retry window, and the alternative (in-flight
// tracking) buys nothing but complexity.
type botRoleCache struct {
	// fetch returns the raw role IDs the bot holds in a guild. A field rather
	// than a direct session call so the caching and @everyone rules are
	// testable without standing up a fake Discord.
	fetch      func(guildID, botID string) ([]string, error)
	retryAfter time.Duration

	mu      sync.Mutex
	roles   map[string]map[string]bool // guildID → set of role IDs the bot holds
	lastTry map[string]time.Time       // guildID → last lookup attempt
}

func newBotRoleCache(s *discordgo.Session) *botRoleCache {
	return &botRoleCache{
		fetch: func(guildID, botID string) ([]string, error) {
			member, err := s.GuildMember(guildID, botID)
			if err != nil {
				return nil, err
			}
			return member.Roles, nil
		},
		retryAfter: defaultRoleRetryAfter,
		roles:      map[string]map[string]bool{},
		lastTry:    map[string]time.Time{},
	}
}

// lookup returns the bot's role IDs in guildID, fetching them on first use and
// re-fetching a failed/empty guild no more than once per retryAfter.
func (c *botRoleCache) lookup(guildID, botID string) map[string]bool {
	if guildID == "" || botID == "" {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	if set, ok := c.roles[guildID]; ok && len(set) > 0 {
		return set
	}
	if last, ok := c.lastTry[guildID]; ok && time.Since(last) < c.retryAfter {
		return c.roles[guildID] // may be nil/empty — the known-bad answer, not a re-fetch
	}
	c.lastTry[guildID] = time.Now()

	roles, err := c.fetch(guildID, botID)
	if err != nil {
		// Degrade to the pre-fix behaviour rather than failing the message:
		// user mentions and replies still wake the agent, role mentions don't.
		// Warn (not Debug) because this is a misconfiguration an operator can
		// fix, and it silently costs the agent inbound messages.
		slog.Warn("discord-sidecar: bot role lookup failed — role mentions will not wake the agent",
			"guild", guildID, "error", err)
		return c.roles[guildID]
	}

	set := make(map[string]bool, len(roles))
	for _, id := range roles {
		// The @everyone role's ID is the guild ID. Counting it would make every
		// @everyone ping wake the agent, which mention-only exists to prevent.
		if id == "" || id == guildID {
			continue
		}
		set[id] = true
	}
	c.roles[guildID] = set
	slog.Info("discord-sidecar: resolved bot roles", "guild", guildID, "role_count", len(set))
	return set
}

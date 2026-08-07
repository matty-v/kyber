// Command kyber-mcp-discord is the Discord channel sidecar (kyber#646).
//
// It runs as a sidecar container in an agent pod, holds a Discord Gateway
// connection, and bridges inbound Discord messages onto Kyber's existing
// generic inbound-binding rail (kyber#208): on an allowlisted message it
// HMAC-signs a JSON envelope and POSTs it to
//
//	{KYBER_INBOUND_URL}/webhooks/inbound/{agent}/{binding}
//
// which the control plane authenticates, buffers, and dispatches to the agent
// as an inbound prompt — reusing the same wake/dedup/rate-limit machinery that
// backs Telegram. Outbound replies pass through this process's loopback-only
// send endpoint, keeping the bot token out of the runtime container.
//
// Why a separate container (Model A, see the design spec): Discord delivers
// normal messages ONLY over a persistent Gateway WebSocket — there is no
// message webhook Kyber could hold while a pod sleeps — so something must keep
// a live socket open. The sidecar does, for the life of the (warm) pod. On
// SIGTERM it closes the Gateway cleanly so a redeploy doesn't leave a ghost
// session. It also exposes a loopback-only send endpoint so the runtime can
// reply without receiving the Discord bot token.
package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/bwmarrin/discordgo"
)

// config is the sidecar's runtime configuration, all from the environment.
// The bot token and HMAC secret arrive via mounted Secret env refs; the
// allowlists and routing come from the chart-rendered container env.
type config struct {
	botToken string // DISCORD_BOT_TOKEN (Secret) — required

	allowedGuilds   map[string]bool // DISCORD_ALLOWED_GUILD_IDS (csv) — empty = any guild the bot is in
	allowedChannels map[string]bool // DISCORD_ALLOWED_CHANNEL_IDS (csv) — empty = any channel
	allowedUsers    map[string]bool // DISCORD_ALLOWED_USER_IDS (csv) — REQUIRED, empty = deny-all (fail closed)

	mentionOnly bool // DISCORD_MENTION_ONLY — forward only messages addressing the bot

	// botRoles resolves which guild roles stand in for the bot, so that
	// mention-only honours "@Barf" whether the author picked the bot's user or
	// its auto-created managed role. nil disables role matching (tests, and the
	// window before the Gateway hands us an identity).
	botRoles botRoleResolver

	inboundURL     string // KYBER_INBOUND_URL — e.g. http://<release>-control-plane:8080
	agentName      string // KYBER_AGENT_NAME
	bindingName    string // KYBER_INBOUND_BINDING (default "discord")
	hmacSecret     []byte // KYBER_INBOUND_HMAC_SECRET (Secret) — the binding's webhook-secret
	sigHeader      string // KYBER_INBOUND_SIGNATURE_HEADER (default "X-Kyber-Signature-256")
	sigPrefix      string // KYBER_INBOUND_SIGNATURE_PREFIX (default "sha256=")
	eventHeader    string // KYBER_INBOUND_EVENT_HEADER (optional) — sent with eventValue when set
	eventValue     string // KYBER_INBOUND_EVENT_VALUE (default "message")
	healthAddr     string // KYBER_DISCORD_HEALTH_ADDR (default ":14002")
	sendAddr       string // KYBER_DISCORD_SEND_ADDR (default "127.0.0.1:14005")
	mcpAddr        string // KYBER_DISCORD_MCP_ADDR (default shared runtime wiring)
	downloadDir    string // KYBER_DISCORD_DOWNLOAD_DIR (shared /persist mount)
	requestTimeout time.Duration
	lifecycle      *discordLifecycle
	attachments    *attachmentStore
	threadParents  *sync.Map // observed thread ID -> allowlisted parent channel ID
	contextLoader  func(channelID, beforeID string, limit int) ([]*discordgo.Message, error)
}

type sendRequest struct {
	ChannelID string `json:"channel_id"`
	Content   string `json:"content"`
	MessageID string `json:"message_id,omitempty"`
}

type discordSender interface {
	ChannelMessageSendComplex(channelID string, data *discordgo.MessageSend, options ...discordgo.RequestOption) (*discordgo.Message, error)
	ChannelMessageEditComplex(data *discordgo.MessageEdit, options ...discordgo.RequestOption) (*discordgo.Message, error)
	MessageReactionAdd(channelID, messageID, emojiID string, options ...discordgo.RequestOption) error
	MessageReactionRemove(channelID, messageID, emojiID, userID string, options ...discordgo.RequestOption) error
}

// inboundEnvelope is the JSON body POSTed to the inbound binding. Field names
// mirror the <channel source="discord" …> tag shape the runtime prompt expects,
// so the binding's field extraction and the agent-side tag match Telegram.
type inboundEnvelope struct {
	Source            string                  `json:"source"` // always "discord"
	GuildID           string                  `json:"guild_id"`
	ChannelID         string                  `json:"channel_id"`
	MessageID         string                  `json:"message_id"`
	User              string                  `json:"user"`     // display name
	UserID            string                  `json:"user_id"`  // stable Discord snowflake (allowlist key)
	Username          string                  `json:"username"` // login handle
	Timestamp         string                  `json:"ts"`       // RFC3339
	Content           string                  `json:"content"`  // message text (requires Message Content Intent)
	Attachments       []inboundAttachment     `json:"attachments,omitempty"`
	ThreadID          string                  `json:"thread_id,omitempty"`
	ThreadName        string                  `json:"thread_name,omitempty"`
	ParentChannelID   string                  `json:"parent_channel_id,omitempty"`
	ReferencedMessage *inboundMessageContext  `json:"referenced_message,omitempty"`
	RecentContext     []inboundMessageContext `json:"recent_context,omitempty"`
}

type inboundMessageContext struct {
	MessageID string `json:"message_id"`
	User      string `json:"user"`
	UserID    string `json:"user_id"`
	Timestamp string `json:"ts,omitempty"`
	Content   string `json:"content"`
}

const discordGatewayIntents = discordgo.IntentsGuilds | discordgo.IntentsGuildMessages | discordgo.IntentsMessageContent

func main() {
	cfg, err := loadConfig()
	if err != nil {
		slog.Error("discord-sidecar: bad config", "error", err)
		os.Exit(2)
	}

	slog.Info("discord-sidecar: starting",
		"agent", cfg.agentName,
		"binding", cfg.bindingName,
		"allowed_users", len(cfg.allowedUsers),
		"allowed_channels", len(cfg.allowedChannels),
		"allowed_guilds", len(cfg.allowedGuilds),
		"mention_only", cfg.mentionOnly,
	)
	if len(cfg.allowedUsers) == 0 {
		// Fail closed, loudly. An empty allowlist means nobody can drive the
		// agent — the same fail-closed stance as an empty Telegram webhook
		// secret (kyber#564). We still start (so the pod is healthy) but every
		// inbound message is dropped until an allowlist is configured.
		slog.Warn("discord-sidecar: DISCORD_ALLOWED_USER_IDS is empty — all inbound will be DROPPED (fail-closed)")
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	dg, err := discordgo.New("Bot " + cfg.botToken)
	if err != nil {
		slog.Error("discord-sidecar: create session", "error", err)
		os.Exit(1)
	}
	// GuildMessages to receive MESSAGE_CREATE; MessageContent (privileged) to
	// actually read the text — the bot must have Message Content Intent enabled
	// in the Discord developer portal or Content arrives empty.
	// Guilds carries THREAD_CREATE/UPDATE/DELETE and the active-thread state
	// needed to resolve a thread back to its parent channel. The REST fallback
	// in discordThreadContext still covers threads created before this Gateway
	// session or absent after a reconnect.
	dg.Identify.Intents = discordGatewayIntents

	// Role membership drives mention-only's "@Agent as a role" case; the cache
	// is built on the same session so it inherits its auth and rate limiting.
	cfg.botRoles = newBotRoleCache(dg).lookup

	client := &http.Client{Timeout: cfg.requestTimeout}
	cfg.attachments = newAttachmentStore(256)
	cfg.threadParents = &sync.Map{}
	cfg.contextLoader = func(channelID, beforeID string, limit int) ([]*discordgo.Message, error) {
		return dg.ChannelMessages(channelID, limit, beforeID, "", "")
	}
	cfg.lifecycle = newDiscordLifecycle(ctx, dg, 8*time.Second)
	dg.AddHandler(func(s *discordgo.Session, m *discordgo.MessageCreate) {
		handleMessage(context.Background(), cfg, client, s, m)
	})

	// Start the local services before connecting the Gateway. A Discord outage
	// or a portal misconfiguration must not kill this container and hold the
	// entire agent pod in Starting; the Gateway reconnect loop remains loud and
	// recovers automatically once Discord accepts the configured intents.
	// kyber#674: a session identified before the bot was invited to the server
	// goes permanently deaf without erroring. The watcher recovers that case
	// and makes "never invited" visible instead of indistinguishable from idle.
	guilds := newGuildWatcher(cfg.allowedGuilds)

	healthSrv := startHealth(cfg.healthAddr, guilds.isReady)
	sendSrv := startSendServer(cfg.sendAddr, dg, cfg.allowedChannels, cfg.lifecycle)
	mcpSrv := startDiscordMCPServer(cfg.mcpAddr, dg, cfg)
	go reportDrops(ctx, dropSummaryInterval)

	connected := openGatewayUntilConnected(ctx, dg.Open, 5*time.Second)

	if connected {
		// Reconnect by closing and re-opening: the reopen re-identifies with
		// membership already in place, which is the state a normally-started
		// sidecar is in. Errors are logged rather than fatal — a failed
		// reconnect leaves the previous session up and the next tick retries.
		reconnect := func() {
			if err := dg.Close(); err != nil {
				slog.Warn("discord-sidecar: gateway close before reconnect", "error", err)
			}
			if !openGatewayUntilConnected(ctx, dg.Open, 5*time.Second) {
				slog.Warn("discord-sidecar: gateway reconnect abandoned — shutting down")
			}
		}
		go guilds.run(ctx, dg, reconnect, guildCheckInterval, guildGracePeriod)

		<-ctx.Done()
		slog.Info("discord-sidecar: shutting down — closing gateway")
		if err := dg.Close(); err != nil {
			slog.Warn("discord-sidecar: gateway close", "error", err)
		}
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = healthSrv.Shutdown(shutdownCtx)
	_ = sendSrv.Shutdown(shutdownCtx)
	_ = mcpSrv.Shutdown(shutdownCtx)
}

func openGatewayUntilConnected(ctx context.Context, open func() error, retryDelay time.Duration) bool {
	for {
		if err := open(); err == nil {
			slog.Info("discord-sidecar: gateway connected")
			return true
		} else {
			slog.Warn("discord-sidecar: open gateway failed; retrying", "error", err, "retry_in", retryDelay)
		}
		timer := time.NewTimer(retryDelay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return false
		case <-timer.C:
		}
	}
}

// drops tallies messages the sidecar declined to forward, by reason.
//
// Every drop path is individually silent-by-design (a shared channel would
// otherwise produce an info line per human sentence), but silence across ALL of
// them means a misrouted message looks identical to a broken agent from the
// outside — which is how a role-mention bug survived until a human in the
// server said "you're only answering sometimes". The periodic summary restores
// the signal at a fixed, chatter-independent cost.
var drops struct {
	outOfScope     atomic.Uint64 // wrong guild or channel
	nonAllowlisted atomic.Uint64 // author not on DISCORD_ALLOWED_USER_IDS
	unaddressed    atomic.Uint64 // mention-only: not aimed at the bot
}

// dropSummaryInterval is how often the drop tallies are logged (and reset).
const dropSummaryInterval = 5 * time.Minute

// reportDrops logs a one-line drop summary every interval until ctx is done,
// staying quiet in windows where nothing was dropped.
func reportDrops(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			scope := drops.outOfScope.Swap(0)
			users := drops.nonAllowlisted.Swap(0)
			unaddressed := drops.unaddressed.Swap(0)
			if scope|users|unaddressed == 0 {
				continue
			}
			slog.Info("discord-sidecar: dropped inbound messages",
				"window", interval.String(),
				"out_of_scope", scope,
				"non_allowlisted_user", users,
				"unaddressed_mention_only", unaddressed)
		}
	}
}

// handleMessage applies the allowlist and, on a pass, bridges the message to
// the agent's inbound binding.
func handleMessage(ctx context.Context, cfg *config, client *http.Client, s *discordgo.Session, m *discordgo.MessageCreate) {
	// Never react to our own messages or other bots (loop guard).
	if m.Author == nil || m.Author.Bot || (s.State != nil && s.State.User != nil && m.Author.ID == s.State.User.ID) {
		return
	}
	// Allowlist: guild (if constrained), channel (if constrained), user (always).
	if len(cfg.allowedGuilds) > 0 && !cfg.allowedGuilds[m.GuildID] {
		drops.outOfScope.Add(1)
		return
	}
	channelAllowed := len(cfg.allowedChannels) == 0 || cfg.allowedChannels[m.ChannelID]
	threadID, threadName, parentChannelID := "", "", ""
	if !channelAllowed {
		if cfg.threadParents != nil {
			if observedParent, ok := cfg.threadParents.Load(m.ChannelID); ok {
				if parent, ok := observedParent.(string); ok && cfg.allowedChannels[parent] {
					threadID, parentChannelID, channelAllowed = m.ChannelID, parent, true
				}
			}
		}
	}
	if !channelAllowed {
		threadID, threadName, parentChannelID = discordThreadContext(s, m.ChannelID)
		if parentChannelID != "" && cfg.allowedChannels[parentChannelID] {
			channelAllowed = true
			if cfg.threadParents != nil {
				cfg.threadParents.Store(threadID, parentChannelID)
			}
		}
	}
	if !channelAllowed {
		drops.outOfScope.Add(1)
		return
	}
	if !cfg.allowedUsers[m.Author.ID] {
		// Not on the allowlist (or allowlist empty) — drop silently. Debug-log
		// so an operator can see why a message didn't reach the agent, without
		// spamming at info level for every non-allowlisted chatter. The periodic
		// drop summary carries the count at info level.
		drops.nonAllowlisted.Add(1)
		slog.Debug("discord-sidecar: drop non-allowlisted user", "user_id", m.Author.ID, "channel", m.ChannelID)
		return
	}

	var botID string
	if s.State != nil && s.State.User != nil {
		botID = s.State.User.ID
	}
	// Resolve the bot's roles only when the message actually mentions a role —
	// the common case (plain chatter, or a direct @user ping) never touches the
	// Discord API.
	var botRoles map[string]bool
	if cfg.botRoles != nil && botID != "" && len(m.MentionRoles) > 0 {
		botRoles = cfg.botRoles(m.GuildID, botID)
	}
	if cfg.mentionOnly && !addressesBot(m, botID, botRoles) {
		// Shared-channel mode: humans talk to each other here, and only messages
		// aimed at the bot should wake the agent (every forward costs a turn).
		drops.unaddressed.Add(1)
		slog.Debug("discord-sidecar: drop unaddressed message (mention-only)",
			"message_id", m.ID, "channel", m.ChannelID)
		return
	}

	env := inboundEnvelope{
		Source:          "discord",
		GuildID:         m.GuildID,
		ChannelID:       m.ChannelID,
		MessageID:       m.ID,
		User:            displayName(m),
		UserID:          m.Author.ID,
		Username:        m.Author.Username,
		Timestamp:       m.Timestamp.Format(time.RFC3339),
		Content:         stripBotMentions(m.Content, botID, botRoles),
		Attachments:     inboundAttachments(m.Attachments),
		ThreadID:        threadID,
		ThreadName:      threadName,
		ParentChannelID: parentChannelID,
	}
	if m.ReferencedMessage != nil {
		referenced := boundedDiscordContext(m.ReferencedMessage)
		env.ReferencedMessage = &referenced
	}
	if cfg.contextLoader != nil {
		messages, err := cfg.contextLoader(m.ChannelID, m.ID, 5)
		if err != nil {
			slog.Debug("discord-sidecar: recent context unavailable", "channel", m.ChannelID, "error", err)
		} else {
			for i := len(messages) - 1; i >= 0; i-- {
				env.RecentContext = append(env.RecentContext, boundedDiscordContext(messages[i]))
			}
		}
	}
	if cfg.attachments != nil {
		cfg.attachments.observe(m.Attachments)
	}
	if err := postInbound(ctx, cfg, client, env); err != nil {
		if cfg.lifecycle != nil {
			cfg.lifecycle.failed(m.ChannelID, m.ID)
		}
		slog.Error("discord-sidecar: forward to inbound binding failed",
			"error", err, "message_id", m.ID, "user_id", m.Author.ID)
		return
	}
	if cfg.lifecycle != nil {
		cfg.lifecycle.begin(m.ChannelID, m.ID)
	}
	slog.Info("discord-sidecar: forwarded message", "message_id", m.ID, "user_id", m.Author.ID, "channel", m.ChannelID)
}

func boundedDiscordContext(m *discordgo.Message) inboundMessageContext {
	if m == nil {
		return inboundMessageContext{}
	}
	user, userID := "", ""
	if m.Author != nil {
		user, userID = m.Author.GlobalName, m.Author.ID
		if user == "" {
			user = m.Author.Username
		}
	}
	content := strings.TrimSpace(m.Content)
	const maxContextUnits = 500
	if utf16Units(content) > maxContextUnits {
		content = splitByUTF16Limit(content, maxContextUnits)[0] + "…"
	}
	return inboundMessageContext{MessageID: m.ID, User: user, UserID: userID,
		Timestamp: m.Timestamp.Format(time.RFC3339), Content: content}
}

func discordThreadContext(s *discordgo.Session, channelID string) (threadID, threadName, parentChannelID string) {
	if s == nil {
		return "", "", ""
	}
	var channel *discordgo.Channel
	var err error
	if s.State != nil {
		channel, err = s.State.Channel(channelID)
	}
	if (channel == nil || err != nil) && s.Client != nil && s.Ratelimiter != nil {
		// Active threads are not guaranteed to be present in discordgo's state
		// cache (notably after reconnect and without a THREAD_CREATE event in
		// this process). Resolve the channel through Discord's REST API so a
		// parent-channel allowlist works for those threads too.
		channel, err = s.Channel(channelID)
	}
	if err != nil || channel == nil || channel.ParentID == "" {
		return "", "", ""
	}
	switch channel.Type {
	case discordgo.ChannelTypeGuildPublicThread, discordgo.ChannelTypeGuildPrivateThread, discordgo.ChannelTypeGuildNewsThread:
		return channel.ID, channel.Name, channel.ParentID
	default:
		return "", "", ""
	}
}

// postInbound HMAC-signs the envelope and POSTs it to the inbound binding.
func postInbound(ctx context.Context, cfg *config, client *http.Client, env inboundEnvelope) error {
	body, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("marshal envelope: %w", err)
	}
	mac := hmac.New(sha256.New, cfg.hmacSecret)
	mac.Write(body)
	sig := cfg.sigPrefix + hex.EncodeToString(mac.Sum(nil))

	url := fmt.Sprintf("%s/webhooks/inbound/%s/%s",
		strings.TrimRight(cfg.inboundURL, "/"), cfg.agentName, cfg.bindingName)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(cfg.sigHeader, sig)
	if cfg.eventHeader != "" {
		req.Header.Set(cfg.eventHeader, cfg.eventValue)
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("post inbound: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("inbound binding returned %d", resp.StatusCode)
	}
	return nil
}

// addressesBot reports whether a message is aimed at the bot: an explicit
// @-mention of the bot user, a mention of a role the bot holds, or a reply to
// one of the bot's own messages.
//
// Replies count because that is how people actually follow up in Discord —
// requiring a re-tag to continue a thread the bot started reads as the bot
// ignoring you.
//
// Role mentions count because Discord makes them indistinguishable from user
// mentions at the point of typing. Adding a bot to a server auto-creates a
// managed role with the bot's own name, so "@Barf" in the composer resolves to
// EITHER the user (<@id>) or that role (<@&id>) depending on which entry the
// author picks from autocomplete — both render as "@Barf" in the client. Only
// honouring the user form meant a human who picked the role saw the agent
// ignore them with no way to tell why (found in Steve's server, 2026-07-31:
// role-tagged questions were dropped while user-tagged ones were answered).
// botRoles carries the roles the bot actually holds in this guild; it is nil
// when the message mentions no roles, when identity is unknown, or when the
// lookup failed (in which case role mentions simply don't match — the pre-fix
// behaviour).
//
// @everyone/@here still deliberately do NOT count: they land in
// MentionEveryone, not Mentions/MentionRoles, so a server-wide ping can't wake
// every agent. The @everyone *role* (whose ID equals the guild ID) is excluded
// from botRoles at the source for the same reason.
func addressesBot(m *discordgo.MessageCreate, botID string, botRoles map[string]bool) bool {
	if botID == "" {
		// Identity unknown (Gateway READY not yet applied) — fail OPEN rather
		// than silently swallowing the operator's messages. The allowlist is
		// still enforced above, so this is a routing question, not a security
		// one.
		return true
	}
	for _, u := range m.Mentions {
		if u != nil && u.ID == botID {
			return true
		}
	}
	for _, roleID := range m.MentionRoles {
		if botRoles[roleID] {
			return true
		}
	}
	if m.ReferencedMessage != nil && m.ReferencedMessage.Author != nil &&
		m.ReferencedMessage.Author.ID == botID {
		return true
	}
	return false
}

// stripBotMentions removes the bot's own mention tokens from the message text
// so the agent sees "what's up?" rather than "<@123456> what's up?". Discord
// renders the user form as <@id> or the legacy nickname form <@!id>, and a role
// the bot holds as <@&roleID> — all three read as "@Agent" on screen, so all
// three are plumbing. Mentions of anyone else are left intact — they're content.
func stripBotMentions(content, botID string, botRoles map[string]bool) string {
	if content == "" || (botID == "" && len(botRoles) == 0) {
		return content
	}
	stripped := content
	if botID != "" {
		for _, token := range []string{"<@" + botID + ">", "<@!" + botID + ">"} {
			stripped = strings.ReplaceAll(stripped, token, "")
		}
	}
	for roleID := range botRoles {
		stripped = strings.ReplaceAll(stripped, "<@&"+roleID+">", "")
	}
	if stripped = strings.TrimSpace(stripped); stripped == "" {
		// A bare "@Agent" with no text — keep the original so the agent sees a
		// ping rather than an empty prompt it can't act on.
		return content
	}
	return stripped
}

func displayName(m *discordgo.MessageCreate) string {
	if m.Member != nil && m.Member.Nick != "" {
		return m.Member.Nick
	}
	if m.Author.GlobalName != "" {
		return m.Author.GlobalName
	}
	return m.Author.Username
}

// startHealth serves /healthz. ready reports whether the sidecar can actually
// receive inbound messages; a nil ready keeps the old always-OK behaviour.
//
// kyber#674: this used to return 200 unconditionally, so a bot that had never
// been invited to its server — and therefore could never deliver a single
// message — was indistinguishable from a healthy idle one. Readiness now
// reflects guild membership.
func startHealth(addr string, ready func() bool) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		if ready != nil && !ready() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("not a member of configured guild(s) — inbound cannot arrive"))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("discord-sidecar: health server", "error", err)
		}
	}()
	return srv
}

func startSendServer(addr string, sender discordSender, allowedChannels map[string]bool, lifecycle *discordLifecycle) *http.Server {
	srv := &http.Server{Addr: addr, Handler: sendHandler(sender, allowedChannels, lifecycle), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("discord-sidecar: send server", "error", err)
		}
	}()
	return srv
}

// sendHandler serves the loopback reply endpoint. allowedChannels mirrors the
// inbound gate: when DISCORD_ALLOWED_CHANNEL_IDS is set, the agent may only
// reply into those channels. Without this the outbound path is strictly wider
// than the inbound one — inbound is allowlisted by guild/channel/user, but a
// runtime (or a prompt-injected agent) could post into any channel the bot can
// see. An empty list means "any channel", matching inbound's semantics.
func sendHandler(sender discordSender, allowedChannels map[string]bool, lifecycle *discordLifecycle) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 32<<10)
		var req sendRequest
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			http.Error(w, "invalid JSON body", http.StatusBadRequest)
			return
		}
		req.ChannelID = strings.TrimSpace(req.ChannelID)
		req.Content = strings.TrimSpace(req.Content)
		req.MessageID = strings.TrimSpace(req.MessageID)
		if req.ChannelID == "" || req.Content == "" {
			http.Error(w, "channel_id and content are required", http.StatusBadRequest)
			return
		}
		if len(allowedChannels) > 0 && !allowedChannels[req.ChannelID] {
			slog.Warn("discord-sidecar: outbound send refused — channel not in DISCORD_ALLOWED_CHANNEL_IDS",
				"channel", req.ChannelID)
			http.Error(w, "channel_id is not in the allowlist", http.StatusForbidden)
			return
		}
		if _, err := sendDiscordText(sender, req.ChannelID, req.Content, req.MessageID); err != nil {
			slog.Warn("discord-sidecar: outbound send failed", "error", err, "channel", req.ChannelID)
			http.Error(w, "Discord send failed", http.StatusBadGateway)
			return
		}
		if lifecycle != nil {
			lifecycle.complete(req.ChannelID, req.MessageID)
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

func loadConfig() (*config, error) {
	cfg := &config{
		allowedGuilds:   csvSet(os.Getenv("DISCORD_ALLOWED_GUILD_IDS")),
		allowedChannels: csvSet(os.Getenv("DISCORD_ALLOWED_CHANNEL_IDS")),
		allowedUsers:    csvSet(os.Getenv("DISCORD_ALLOWED_USER_IDS")),
		mentionOnly:     truthy(os.Getenv("DISCORD_MENTION_ONLY")),
		botToken:        os.Getenv("DISCORD_BOT_TOKEN"),
		inboundURL:      os.Getenv("KYBER_INBOUND_URL"),
		agentName:       os.Getenv("KYBER_AGENT_NAME"),
		bindingName:     envOr("KYBER_INBOUND_BINDING", "discord"),
		hmacSecret:      []byte(os.Getenv("KYBER_INBOUND_HMAC_SECRET")),
		sigHeader:       envOr("KYBER_INBOUND_SIGNATURE_HEADER", "X-Kyber-Signature-256"),
		sigPrefix:       envOr("KYBER_INBOUND_SIGNATURE_PREFIX", "sha256="),
		eventHeader:     os.Getenv("KYBER_INBOUND_EVENT_HEADER"),
		eventValue:      envOr("KYBER_INBOUND_EVENT_VALUE", "message"),
		healthAddr:      envOr("KYBER_DISCORD_HEALTH_ADDR", ":14002"),
		sendAddr:        envOr("KYBER_DISCORD_SEND_ADDR", "127.0.0.1:14005"),
		mcpAddr:         envOr("KYBER_DISCORD_MCP_ADDR", "127.0.0.1:14007"),
		downloadDir:     envOr("KYBER_DISCORD_DOWNLOAD_DIR", "/persist/discord-attachments"),
		requestTimeout:  10 * time.Second,
	}
	if cfg.botToken == "" {
		return nil, fmt.Errorf("DISCORD_BOT_TOKEN is required")
	}
	if cfg.inboundURL == "" {
		return nil, fmt.Errorf("KYBER_INBOUND_URL is required")
	}
	if cfg.agentName == "" {
		return nil, fmt.Errorf("KYBER_AGENT_NAME is required")
	}
	if len(cfg.hmacSecret) == 0 {
		return nil, fmt.Errorf("KYBER_INBOUND_HMAC_SECRET is required")
	}
	return cfg, nil
}

func csvSet(s string) map[string]bool {
	out := map[string]bool{}
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out[p] = true
		}
	}
	return out
}

// truthy parses the boolean env knobs. Absent/empty is false, so an unset
// var means "off" without the controller having to render an explicit "false".
func truthy(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

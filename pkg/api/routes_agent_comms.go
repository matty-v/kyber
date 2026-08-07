// Per-agent comms configuration (kyber#664).
//
// One endpoint family for every channel an agent can talk on, so configuring
// Telegram or two-way Discord is an API call rather than a hand-run sequence of
// kubectl steps. Before this, Telegram could only be set up at agent-creation
// time (routes_agents.go createAgent) and Discord had no API surface at all —
// the operator created a Secret, created a matching inbound binding, patched
// the spec, and rolled the pod, with a hand-copied HMAC secret in the middle
// that fails silently when it doesn't match.
//
//	GET    /api/v1/agents/{name}/comms              all channels
//	GET    /api/v1/agents/{name}/comms/{channel}    one channel
//	PUT    /api/v1/agents/{name}/comms/{channel}    configure (idempotent)
//	DELETE /api/v1/agents/{name}/comms/{channel}    disable + clean up
//
// PUT rather than POST: a channel is a singleton per agent, so "configure" and
// "update" are the same call and a retried request is harmless.
//
// Secret material is write-only. Tokens go in through PUT and are never
// returned by any endpoint — GET reports presence (`botTokenSet`) only.
//
// Scope (Matt, 2026-07-31): Telegram and two-way Discord only. The legacy
// outbound-only Discord webhook (spec.secrets.discordEnabled) is deliberately
// NOT a channel here — it keeps working untouched and stays configured the way
// it always was. Because it shares the <agent>-discord Secret, every write
// below is key-scoped so the two can coexist on one agent.
package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/controllers/agent"
)

const (
	commsChannelTelegram = "telegram"
	commsChannelDiscord  = "discord"

	// telegramSecretSuffix / telegramTokenKey mirror the naming the runtime
	// adapter reads (pkg/runtimes/claudecode/adapter.go:253) and that
	// createAgentSecrets writes.
	telegramSecretSuffix      = "-telegram"
	telegramTokenKey          = "token"
	telegramAllowedUserIDsKey = "allowed-user-ids"

	// discordSecretSuffix names the per-agent Discord Secret. It is shared with
	// the legacy outbound webhook path, which owns the "webhook-url" key.
	discordSecretSuffix = "-discord"

	discordBotTokenKey       = "bot-token"
	discordGuildIDKey        = "guild-id"
	discordChannelIDKey      = "channel-id"
	discordAllowedUserIDsKey = "allowed-user-ids"
	discordLegacyWebhookKey  = "webhook-url"

	// commsHMACRandomBytes matches the inbound-binding generator: 32 random
	// bytes, hex-encoded, stored as the ASCII hex string (the hex string IS the
	// HMAC key — see routes_inbound_bindings.go).
	commsHMACRandomBytes = 32
)

// discordSnowflakeRe matches a Discord ID. Discord IDs are unsigned 64-bit
// snowflakes rendered as decimal digits; anything else is a paste error
// (a channel *name*, a URL, a stray "#") and is worth rejecting at the edge
// rather than letting it silently mismatch every message at runtime.
var discordSnowflakeRe = regexp.MustCompile(`^[0-9]{5,25}$`)
var telegramUserIDRe = regexp.MustCompile(`^[0-9]{1,20}$`)

// commsChannelResponse is one channel's configuration. It never carries secret
// material: BotTokenSet reports presence, not value.
type commsChannelResponse struct {
	Channel    string `json:"channel"`
	Configured bool   `json:"configured"`
	// PodRestartRequired is true when the stored config differs from what the
	// running pod actually has. Neither channel takes effect on a live pod —
	// the Discord sidecar is injected at pod-build time and Telegram's token is
	// injected as env — so the operator has to roll the pod to apply a change.
	// Kyber does not roll it for them: that would destroy the agent's session.
	PodRestartRequired bool `json:"podRestartRequired"`
	BotTokenSet        bool `json:"botTokenSet"`

	// Discord-only. Empty guild/channel lists mean "any"; an empty user list is
	// fail-closed (nobody can drive the agent), which is why PUT rejects it.
	GuildIDs       []string `json:"guildIds,omitempty"`
	ChannelIDs     []string `json:"channelIds,omitempty"`
	AllowedUserIDs []string `json:"allowedUserIds,omitempty"`
	MentionOnly    bool     `json:"mentionOnly,omitempty"`

	// DiscordConnection summarizes the running sidecar's Kubernetes-observed
	// state. It deliberately does not expose credentials or require the control
	// plane to hold the Discord token.
	DiscordConnection *discordConnectionResponse `json:"discordConnection,omitempty"`
}

type discordConnectionResponse struct {
	Status       string `json:"status"`
	Ready        bool   `json:"ready"`
	RestartCount int32  `json:"restartCount"`
	Detail       string `json:"detail,omitempty"`
}

type commsListResponse struct {
	Channels []commsChannelResponse `json:"channels"`
}

// putTelegramCommsRequest configures Telegram. BotToken may be omitted when a
// token is already stored — that is the "re-enable what was disabled" path.
type putTelegramCommsRequest struct {
	BotToken       string   `json:"botToken,omitempty"`
	AllowedUserIDs []string `json:"allowedUserIds"`
}

// putDiscordCommsRequest configures two-way Discord.
type putDiscordCommsRequest struct {
	BotToken       string   `json:"botToken,omitempty"`
	GuildIDs       []string `json:"guildIds,omitempty"`
	ChannelIDs     []string `json:"channelIds,omitempty"`
	AllowedUserIDs []string `json:"allowedUserIds,omitempty"`
	MentionOnly    bool     `json:"mentionOnly,omitempty"`
	// Action overrides the inbound binding's instruction text — the static
	// prose the agent sees on every Discord message. Empty uses the generated
	// default, which already contains working reply instructions.
	Action string `json:"action,omitempty"`
}

// handleAgentComms dispatches the comms sub-tree under /api/v1/agents/{name}.
// `subpath` is whatever follows "comms" — "" for the collection, "{channel}"
// for a single channel.
func (s *Server) handleAgentComms(w http.ResponseWriter, r *http.Request, agentName, subpath string) {
	subpath = strings.Trim(subpath, "/")

	if subpath == "" {
		if r.Method != http.MethodGet {
			writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
			return
		}
		s.listAgentComms(w, r, agentName)
		return
	}

	if strings.Contains(subpath, "/") {
		writeJSONError(w, http.StatusNotFound, "not_found", "unknown comms action")
		return
	}

	channel := subpath
	if channel != commsChannelTelegram && channel != commsChannelDiscord {
		writeJSONError(w, http.StatusNotFound, "not_found",
			"unknown comms channel '"+channel+"' (supported: telegram, discord)")
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.getAgentComm(w, r, agentName, channel)
	case http.MethodPut:
		s.putAgentComm(w, r, agentName, channel)
	case http.MethodDelete:
		s.deleteAgentComm(w, r, agentName, channel)
	default:
		writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
	}
}

// listAgentComms handles GET /api/v1/agents/{name}/comms.
func (s *Server) listAgentComms(w http.ResponseWriter, r *http.Request, agentName string) {
	ag, ok := s.getAgentForComms(w, r, agentName)
	if !ok {
		return
	}
	pod := s.podForComms(r.Context(), agentName)

	writeJSON(w, http.StatusOK, commsListResponse{Channels: []commsChannelResponse{
		s.telegramCommsState(r.Context(), ag, pod),
		s.discordCommsState(r.Context(), ag, pod),
	}})
}

// getAgentComm handles GET /api/v1/agents/{name}/comms/{channel}.
func (s *Server) getAgentComm(w http.ResponseWriter, r *http.Request, agentName, channel string) {
	ag, ok := s.getAgentForComms(w, r, agentName)
	if !ok {
		return
	}
	pod := s.podForComms(r.Context(), agentName)

	if channel == commsChannelTelegram {
		writeJSON(w, http.StatusOK, s.telegramCommsState(r.Context(), ag, pod))
		return
	}
	writeJSON(w, http.StatusOK, s.discordCommsState(r.Context(), ag, pod))
}

// getAgentForComms fetches the Agent, writing the 404/500 response itself and
// reporting whether the caller should continue.
func (s *Server) getAgentForComms(w http.ResponseWriter, r *http.Request, agentName string) (*kyberv1.Agent, bool) {
	ag := &kyberv1.Agent{}
	key := types.NamespacedName{Name: agentName, Namespace: s.Namespace}
	if err := s.K8sClient.Get(r.Context(), key, ag); err != nil {
		if k8serrors.IsNotFound(err) {
			writeJSONError(w, http.StatusNotFound, "not_found", "agent '"+agentName+"' not found")
			return nil, false
		}
		slog.Error("comms: failed to get agent", "agent", agentName, "error", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to get agent")
		return nil, false
	}
	return ag, true
}

// podForComms returns the agent's running pod, or nil when there isn't one.
// A missing pod is not an error here — it just means there is nothing stale to
// report, so podRestartRequired stays false.
func (s *Server) podForComms(ctx context.Context, agentName string) *corev1.Pod {
	pod := &corev1.Pod{}
	key := types.NamespacedName{Name: "agent-" + agentName, Namespace: s.Namespace}
	if err := s.K8sClient.Get(ctx, key, pod); err != nil {
		return nil
	}
	return pod
}

// secretData reads a Secret's data, returning nil when it doesn't exist.
func (s *Server) secretData(ctx context.Context, name string) map[string][]byte {
	sec := &corev1.Secret{}
	key := types.NamespacedName{Name: name, Namespace: s.Namespace}
	if err := s.K8sClient.Get(ctx, key, sec); err != nil {
		return nil
	}
	return sec.Data
}

// --- Telegram -------------------------------------------------------------

func (s *Server) telegramCommsState(ctx context.Context, ag *kyberv1.Agent, pod *corev1.Pod) commsChannelResponse {
	enabled := ag.Spec.Secrets.TelegramEnabled
	data := s.secretData(ctx, ag.Name+telegramSecretSuffix)

	resp := commsChannelResponse{
		Channel:        commsChannelTelegram,
		Configured:     enabled,
		BotTokenSet:    len(data[telegramTokenKey]) > 0,
		AllowedUserIDs: splitCSV(string(data[telegramAllowedUserIDsKey])),
	}

	// The runtime container only gets TELEGRAM_BOT_TOKEN when the flag was set
	// at pod-build time (adapter.go), so its presence is what the live pod
	// actually believes — independent of what the spec now says.
	if pod != nil {
		if ag.Spec.Runtime == "codex" {
			resp.PodRestartRequired = enabled != podHasContainer(pod, agent.TelegramSidecarContainerName)
		} else {
			resp.PodRestartRequired = enabled != containerHasEnv(pod, agent.AgentContainerName, "TELEGRAM_BOT_TOKEN")
		}
	}
	return resp
}

func (s *Server) putTelegramComms(w http.ResponseWriter, r *http.Request, ag *kyberv1.Agent) {
	var req putTelegramCommsRequest
	if !decodeCommsBody(w, r, &req) {
		return
	}

	// Telegram's plugin needs a Max-subscription OAuth session; an api-key agent
	// cannot run it. Same rule createAgent enforces — see validateTelegramAuth.
	if err := validateTelegramAuth(ag.Spec.Secrets.AuthType); err != nil {
		writeJSONErrorWithField(w, http.StatusBadRequest, "VALIDATION_ERROR", err.Error(), "authType")
		return
	}

	secretName := ag.Name + telegramSecretSuffix
	existing := s.secretData(r.Context(), secretName)
	if req.BotToken == "" && len(existing[telegramTokenKey]) == 0 {
		writeJSONErrorWithField(w, http.StatusBadRequest, "VALIDATION_ERROR",
			"botToken is required — this agent has no stored Telegram token", "botToken")
		return
	}
	// Runtime-neutral since kyber#684. This used to branch: a Codex agent got
	// the full inbound rail (HMAC secret, allowlist, signed binding) while a
	// Claude Code agent got a Secret containing only the bot token, because its
	// in-process plugin polled, allowlisted and replied entirely on its own.
	//
	// That plugin is gone and every runtime is on the sidecar now, so every
	// runtime needs the rail. Leaving the branch here would have been the more
	// dangerous half of the split: the controller would inject a sidecar that
	// the API had never given anything to sign or allowlist with.
	if err := validateTelegramAllowedUsers(req.AllowedUserIDs); err != nil {
		writeJSONErrorWithField(w, http.StatusBadRequest, "VALIDATION_ERROR", err.Error(), "allowedUserIds")
		return
	}
	hmacSecret := string(existing[webhookSecretKey])
	if hmacSecret == "" {
		var err error
		hmacSecret, err = generateCommsHMACSecret()
		if err != nil {
			slog.Error("comms: failed to generate telegram secret", "agent", ag.Name, "error", err)
			writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to generate secret")
			return
		}
	}

	if err := s.upsertSecretKeys(r.Context(), secretName, ag.Name, map[string][]byte{
		telegramTokenKey:          []byte(firstNonEmpty(req.BotToken, string(existing[telegramTokenKey]))),
		telegramAllowedUserIDsKey: []byte(strings.Join(req.AllowedUserIDs, ",")),
		webhookSecretKey:          []byte(hmacSecret),
	}, nil); err != nil {
		slog.Error("comms: failed to write telegram secret", "agent", ag.Name, "error", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to store bot token")
		return
	}

	if err := s.patchAgentForComms(r.Context(), ag, func(a *kyberv1.Agent) {
		a.Spec.Secrets.TelegramEnabled = true
		binding := telegramInboundBinding(secretName, defaultTelegramAction())
		for i := range a.Spec.InboundBindings {
			if a.Spec.InboundBindings[i].Name == agent.TelegramInboundBindingName {
				a.Spec.InboundBindings[i] = binding
				return
			}
		}
		a.Spec.InboundBindings = append(a.Spec.InboundBindings, binding)
	}); err != nil {
		slog.Error("comms: failed to enable telegram", "agent", ag.Name, "error", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to enable Telegram")
		return
	}

	resp := s.telegramCommsState(r.Context(), ag, s.podForComms(r.Context(), ag.Name))
	// The spec just changed; whatever the pod has is by definition stale until
	// it is rolled. Report that unconditionally rather than trusting a pod read
	// that may race the patch we just made.
	resp.PodRestartRequired = true
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) deleteTelegramComms(w http.ResponseWriter, r *http.Request, ag *kyberv1.Agent) {
	if err := s.patchAgentForComms(r.Context(), ag, func(a *kyberv1.Agent) {
		a.Spec.Secrets.TelegramEnabled = false
		for i := range a.Spec.InboundBindings {
			if a.Spec.InboundBindings[i].Name == agent.TelegramInboundBindingName {
				a.Spec.InboundBindings = append(a.Spec.InboundBindings[:i], a.Spec.InboundBindings[i+1:]...)
				break
			}
		}
	}); err != nil {
		slog.Error("comms: failed to disable telegram", "agent", ag.Name, "error", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to disable Telegram")
		return
	}

	// Drop the stored token too — "disable" should not leave a live bot token
	// sitting in the cluster. The Secret holds nothing else.
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: ag.Name + telegramSecretSuffix, Namespace: s.Namespace,
	}}
	if err := s.K8sClient.Delete(r.Context(), sec); err != nil && !k8serrors.IsNotFound(err) {
		slog.Warn("comms: failed to delete telegram secret",
			"agent", ag.Name, "error", err)
		// Not fatal: the flag is off, so the next pod won't mount it.
	}
	w.WriteHeader(http.StatusNoContent)
}

// validateTelegramAuth enforces the one rule Telegram has: it needs an OAuth
// (Max-subscription) session, so api-key agents cannot use it. Shared by
// createAgent and PUT /comms/telegram so the two cannot drift apart.
func validateTelegramAuth(authType kyberv1.AgentAuthType) error {
	if authType == kyberv1.AgentAuthTypeAPIKey {
		return errors.New("Telegram requires OAuth authentication — api-key agents cannot use Telegram channels")
	}
	return nil
}

func validateTelegramAllowedUsers(ids []string) error {
	if len(ids) == 0 {
		return errors.New("allowedUserIds must list at least one Telegram user ID — an empty allowlist is fail-closed")
	}
	for _, id := range ids {
		if !telegramUserIDRe.MatchString(id) {
			return errors.New("Telegram user IDs must contain decimal digits only")
		}
	}
	return nil
}

func generateCommsHMACSecret() (string, error) {
	buf := make([]byte, commsHMACRandomBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generating comms HMAC secret: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// Both live in the controllers/agent package (kyber#684), same as
// agent.DefaultDiscordAction: the comms API writes these bindings and the
// controller's migration heals them, and the two must produce identical output.
func telegramInboundBinding(secretName, action string) kyberv1.AgentInboundBinding {
	return agent.TelegramInboundBinding(secretName, action)
}

func defaultTelegramAction() string { return agent.DefaultTelegramAction() }

// --- Discord --------------------------------------------------------------

func (s *Server) discordCommsState(ctx context.Context, ag *kyberv1.Agent, pod *corev1.Pod) commsChannelResponse {
	cfg := discordChannelSpec(ag)
	resp := commsChannelResponse{
		Channel:    commsChannelDiscord,
		Configured: cfg != nil,
	}
	if cfg != nil {
		resp.MentionOnly = cfg.MentionOnly
		data := s.secretData(ctx, cfg.ExistingSecret)
		resp.BotTokenSet = len(data[discordBotTokenKey]) > 0
		resp.GuildIDs = splitCSV(string(data[discordGuildIDKey]))
		resp.ChannelIDs = splitCSV(string(data[discordChannelIDKey]))
		resp.AllowedUserIDs = splitCSV(string(data[discordAllowedUserIDsKey]))
	}

	// The sidecar is injected at pod-build time, so its presence in the running
	// pod is the honest answer to "did this config take effect?".
	if pod != nil {
		resp.PodRestartRequired = (cfg != nil) != podHasContainer(pod, agent.DiscordSidecarContainerName)
		if revision := ag.Annotations[agent.DiscordConfigRevisionAnnotation]; revision != "" && pod.Annotations[agent.DiscordConfigRevisionAnnotation] != revision {
			resp.PodRestartRequired = true
		}
	}
	resp.DiscordConnection = discordConnectionState(cfg != nil, resp.PodRestartRequired, pod)
	return resp
}

func discordConnectionState(configured, restartRequired bool, pod *corev1.Pod) *discordConnectionResponse {
	state := &discordConnectionResponse{Status: "not-configured"}
	if !configured {
		return state
	}
	if restartRequired {
		state.Status = "restart-required"
		state.Detail = "waiting for the agent to become idle; Kyber will restart its pod to apply the saved Discord configuration"
		return state
	}
	if pod == nil {
		state.Status = "not-running"
		state.Detail = "the agent pod is not running"
		return state
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name != agent.DiscordSidecarContainerName {
			continue
		}
		state.Ready = cs.Ready
		state.RestartCount = cs.RestartCount
		if cs.Ready {
			state.Status = "connected"
			return state
		}
		state.Status = "starting"
		if cs.State.Waiting != nil {
			state.Status = "degraded"
			state.Detail = cs.State.Waiting.Reason
			if cs.State.Waiting.Message != "" {
				state.Detail += ": " + cs.State.Waiting.Message
			}
		} else if cs.State.Terminated != nil {
			state.Status = "degraded"
			state.Detail = cs.State.Terminated.Reason
		}
		return state
	}
	state.Status = "starting"
	state.Detail = "waiting for Discord sidecar status"
	return state
}

func (s *Server) putDiscordComms(w http.ResponseWriter, r *http.Request, ag *kyberv1.Agent) {
	var req putDiscordCommsRequest
	if !decodeCommsBody(w, r, &req) {
		return
	}

	secretName := ag.Name + discordSecretSuffix
	existing := s.secretData(r.Context(), secretName)

	if req.BotToken == "" && len(existing[discordBotTokenKey]) == 0 {
		writeJSONErrorWithField(w, http.StatusBadRequest, "VALIDATION_ERROR",
			"botToken is required — this agent has no stored Discord bot token", "botToken")
		return
	}

	// An empty user allowlist is fail-closed in the sidecar: the agent would
	// come up wired, healthy, and unable to hear anyone. That is a
	// configuration mistake, not a valid state, so reject it here rather than
	// let the operator debug silence.
	if len(req.AllowedUserIDs) == 0 {
		writeJSONErrorWithField(w, http.StatusBadRequest, "VALIDATION_ERROR",
			"allowedUserIds must list at least one Discord user ID — an empty allowlist is fail-closed and nobody could reach the agent",
			"allowedUserIds")
		return
	}
	for field, ids := range map[string][]string{
		"guildIds":       req.GuildIDs,
		"channelIds":     req.ChannelIDs,
		"allowedUserIds": req.AllowedUserIDs,
	} {
		for _, id := range ids {
			if !discordSnowflakeRe.MatchString(id) {
				writeJSONErrorWithField(w, http.StatusBadRequest, "VALIDATION_ERROR",
					"'"+id+"' is not a Discord ID — enable Developer Mode in Discord, then right-click → Copy ID",
					field)
				return
			}
		}
	}

	// Reuse the existing HMAC secret when there is one: an update (toggling
	// mentionOnly, adding a user) must not silently rotate the secret out from
	// under a running sidecar.
	hmacSecret := string(existing[webhookSecretKey])
	if hmacSecret == "" {
		buf := make([]byte, commsHMACRandomBytes)
		if _, err := rand.Read(buf); err != nil {
			slog.Error("comms: rand.Read failed", "agent", ag.Name, "error", err)
			writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to generate secret")
			return
		}
		hmacSecret = hex.EncodeToString(buf)
	}

	// One Secret, two readers: the sidecar mounts bot-token + the allowlist,
	// and the inbound receiver verifies against webhook-secret. Writing both
	// here is what removes the hand-copied-secret failure mode — the operator
	// never sees the HMAC secret at all, so they cannot mismatch it.
	//
	// Key-scoped write: the legacy outbound "webhook-url" key on this same
	// Secret is preserved, so an agent can run both Discord paths at once.
	if err := s.upsertSecretKeys(r.Context(), secretName, ag.Name, map[string][]byte{
		discordBotTokenKey:       []byte(firstNonEmpty(req.BotToken, string(existing[discordBotTokenKey]))),
		webhookSecretKey:         []byte(hmacSecret),
		discordGuildIDKey:        []byte(strings.Join(req.GuildIDs, ",")),
		discordChannelIDKey:      []byte(strings.Join(req.ChannelIDs, ",")),
		discordAllowedUserIDsKey: []byte(strings.Join(req.AllowedUserIDs, ",")),
	}, nil); err != nil {
		slog.Error("comms: failed to write discord secret", "agent", ag.Name, "error", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to store Discord credentials")
		return
	}

	action := req.Action
	if action == "" {
		action = agent.DefaultDiscordAction(req.MentionOnly)
	}
	binding := discordInboundBinding(secretName, action)

	// Spec write is one patch covering both halves — the inbound binding the
	// sidecar forwards to, and the channel config that injects the sidecar.
	// They are meaningless apart, so they land together or not at all.
	if err := s.patchAgentForComms(r.Context(), ag, func(a *kyberv1.Agent) {
		if a.Annotations == nil {
			a.Annotations = map[string]string{}
		}
		a.Annotations[agent.DiscordConfigRevisionAnnotation] = time.Now().UTC().Format(time.RFC3339Nano)
		replaced := false
		for i := range a.Spec.InboundBindings {
			if a.Spec.InboundBindings[i].Name == agent.DiscordInboundBindingName {
				// Preserve an operator's hand-tuned Action unless this request
				// explicitly supplies one. Migrate Kyber's obsolete generated
				// action, which exposed DISCORD_BOT_TOKEN to the runtime; it can
				// no longer work now that credentials are sidecar-only.
				if req.Action == "" && a.Spec.InboundBindings[i].Action != "" &&
					!agent.IsLegacyDiscordDefaultAction(a.Spec.InboundBindings[i].Action) {
					binding.Action = a.Spec.InboundBindings[i].Action
				}
				a.Spec.InboundBindings[i] = binding
				replaced = true
				break
			}
		}
		if !replaced {
			a.Spec.InboundBindings = append(a.Spec.InboundBindings, binding)
		}
		if a.Spec.Channels == nil {
			a.Spec.Channels = &kyberv1.AgentChannels{}
		}
		a.Spec.Channels.Discord = &kyberv1.AgentDiscordChannel{
			ExistingSecret: secretName,
			MentionOnly:    req.MentionOnly,
		}
	}); err != nil {
		slog.Error("comms: failed to enable discord", "agent", ag.Name, "error", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to enable Discord")
		return
	}

	resp := s.discordCommsState(r.Context(), ag, s.podForComms(r.Context(), ag.Name))
	resp.PodRestartRequired = true
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) deleteDiscordComms(w http.ResponseWriter, r *http.Request, ag *kyberv1.Agent) {
	cfg := discordChannelSpec(ag)
	if cfg == nil {
		writeJSONError(w, http.StatusNotFound, "not_found", "Discord is not configured on this agent")
		return
	}
	secretName := cfg.ExistingSecret

	if err := s.patchAgentForComms(r.Context(), ag, func(a *kyberv1.Agent) {
		if a.Annotations == nil {
			a.Annotations = map[string]string{}
		}
		a.Annotations[agent.DiscordConfigRevisionAnnotation] = time.Now().UTC().Format(time.RFC3339Nano)
		for i := range a.Spec.InboundBindings {
			if a.Spec.InboundBindings[i].Name == agent.DiscordInboundBindingName {
				a.Spec.InboundBindings = append(a.Spec.InboundBindings[:i], a.Spec.InboundBindings[i+1:]...)
				break
			}
		}
		// Clear the channel, not the whole Channels block — a future second
		// channel must not be collaterally removed by disabling Discord.
		if a.Spec.Channels != nil {
			a.Spec.Channels.Discord = nil
		}
	}); err != nil {
		slog.Error("comms: failed to disable discord", "agent", ag.Name, "error", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to disable Discord")
		return
	}

	// Remove only the two-way keys. If the legacy outbound webhook still lives
	// on this Secret, the Secret stays; otherwise there is nothing left to keep.
	if err := s.upsertSecretKeys(r.Context(), secretName, ag.Name, nil, []string{
		discordBotTokenKey, webhookSecretKey,
		discordGuildIDKey, discordChannelIDKey, discordAllowedUserIDsKey,
	}); err != nil {
		slog.Warn("comms: failed to clean discord secret keys",
			"agent", ag.Name, "secret", secretName, "error", err)
		// Not fatal: the spec no longer references the channel, so no pod will
		// mount these keys again. A leaked key is cleanup, not correctness.
	}
	w.WriteHeader(http.StatusNoContent)
}

// discordChannelSpec returns the agent's two-way Discord config, or nil.
func discordChannelSpec(ag *kyberv1.Agent) *kyberv1.AgentDiscordChannel {
	if ag.Spec.Channels == nil {
		return nil
	}
	return ag.Spec.Channels.Discord
}

// discordInboundBinding builds the binding the sidecar HMAC-forwards into. The
// field set matches the envelope kyber-mcp-discord posts
// (cmd/kyber-mcp-discord/main.go): who sent it, which channel to reply in, and
// what they said. channel_id is not decoration — the agent needs it to address
// its reply.
func discordInboundBinding(secretName, action string) kyberv1.AgentInboundBinding {
	return kyberv1.AgentInboundBinding{
		Name:            agent.DiscordInboundBindingName,
		ExistingSecret:  secretName,
		SignatureHeader: "X-Kyber-Signature-256",
		SignaturePrefix: "sha256=",
		Action:          action,
		Fields: []kyberv1.AgentInboundField{
			{Label: "from", JsonPath: "$.user"},
			{Label: "channel_id", JsonPath: "$.channel_id"},
			{Label: "message_id", JsonPath: "$.message_id"},
			{Label: "message", JsonPath: "$.content"},
			{Label: "attachments", JsonPath: "$.attachments"},
			{Label: "thread_id", JsonPath: "$.thread_id"},
			{Label: "thread_name", JsonPath: "$.thread_name"},
			{Label: "parent_channel_id", JsonPath: "$.parent_channel_id"},
			{Label: "referenced_message", JsonPath: "$.referenced_message"},
			{Label: "recent_context", JsonPath: "$.recent_context"},
		},
	}
}

// The Discord action text and its legacy-detection live in the controller
// package (agent.DefaultDiscordAction / agent.IsLegacyDiscordDefaultAction).
// The reconciler needs both to self-heal agents that were wired before the
// credential moved into the sidecar, and pkg/api already depends on that
// package — so the text lives there and this package calls into it.

// --- shared plumbing ------------------------------------------------------

func (s *Server) putAgentComm(w http.ResponseWriter, r *http.Request, agentName, channel string) {
	ag, ok := s.getAgentForComms(w, r, agentName)
	if !ok {
		return
	}
	if channel == commsChannelTelegram {
		s.putTelegramComms(w, r, ag)
		return
	}
	s.putDiscordComms(w, r, ag)
}

func (s *Server) deleteAgentComm(w http.ResponseWriter, r *http.Request, agentName, channel string) {
	ag, ok := s.getAgentForComms(w, r, agentName)
	if !ok {
		return
	}
	if channel == commsChannelTelegram {
		s.deleteTelegramComms(w, r, ag)
		return
	}
	s.deleteDiscordComms(w, r, ag)
}

// decodeCommsBody decodes a PUT body, writing the error response itself.
// An empty body is valid — every field on both requests is optional-by-shape,
// and the handlers do the real validation.
func decodeCommsBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1 MB
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			writeJSONError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "request body exceeds 1MB")
			return false
		}
		if errors.Is(err, io.EOF) {
			return true
		}
		writeJSONError(w, http.StatusBadRequest, "bad_request", "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

// upsertSecretKeys merges `set` into a Secret and removes `remove`, creating
// the Secret when absent and deleting it when the last key goes away.
//
// Key-scoped on purpose: the <agent>-discord Secret is shared with the legacy
// outbound webhook path, so a wholesale write would silently break an existing
// team's Discord setup.
func (s *Server) upsertSecretKeys(ctx context.Context, name, agentName string, set map[string][]byte, remove []string) error {
	sec := &corev1.Secret{}
	key := types.NamespacedName{Name: name, Namespace: s.Namespace}
	err := s.K8sClient.Get(ctx, key, sec)

	if k8serrors.IsNotFound(err) {
		if len(set) == 0 {
			return nil // nothing to create
		}
		return s.K8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: s.Namespace,
				Labels: map[string]string{
					"app.kubernetes.io/managed-by": "kyber-api",
					"kyber.io/agent":               agentName,
				},
			},
			Type: corev1.SecretTypeOpaque,
			Data: set,
		})
	}
	if err != nil {
		return err
	}

	patch := client.MergeFrom(sec.DeepCopy())
	if sec.Data == nil {
		sec.Data = map[string][]byte{}
	}
	for k, v := range set {
		sec.Data[k] = v
	}
	for _, k := range remove {
		delete(sec.Data, k)
	}
	if len(sec.Data) == 0 {
		return s.K8sClient.Delete(ctx, sec)
	}
	return s.K8sClient.Patch(ctx, sec, patch)
}

// patchAgentForComms applies mutate() to the agent and patches it, retrying on
// optimistic-concurrency conflicts by re-reading and re-applying. Same shape as
// createInboundBinding's retry loop, and necessary for the same reason: comms
// writes touch spec.inboundBindings, which other endpoints also mutate.
func (s *Server) patchAgentForComms(ctx context.Context, ag *kyberv1.Agent, mutate func(*kyberv1.Agent)) error {
	const patchRetries = 5
	key := types.NamespacedName{Name: ag.Name, Namespace: s.Namespace}
	var err error
	for attempt := 0; attempt < patchRetries; attempt++ {
		patch := client.MergeFrom(ag.DeepCopy())
		mutate(ag)
		err = s.K8sClient.Patch(ctx, ag, patch)
		if err == nil || !k8serrors.IsConflict(err) {
			return err
		}
		// Re-read and replay the mutation against the current object; the local
		// copy is stale, so re-applying to it would compound the divergence.
		if getErr := s.K8sClient.Get(ctx, key, ag); getErr != nil {
			return getErr
		}
	}
	return err
}

func containerHasEnv(pod *corev1.Pod, containerName, envName string) bool {
	for _, c := range pod.Spec.Containers {
		if c.Name != containerName {
			continue
		}
		for _, e := range c.Env {
			if e.Name == envName {
				return true
			}
		}
	}
	return false
}

func podHasContainer(pod *corev1.Pod, name string) bool {
	for _, c := range pod.Spec.Containers {
		if c.Name == name {
			return true
		}
	}
	return false
}

// splitCSV parses the comma-separated allowlist keys the sidecar reads,
// dropping empty entries so a trailing comma doesn't become a phantom ID.
func splitCSV(v string) []string {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// Command kyber-mcp-telegram bridges Telegram Bot API updates into Kyber's
// signed inbound-binding rail, and serves an MCP tool surface (reply, react,
// edit_message, download_attachment) back to the agent.
//
// It is runtime-neutral (kyber#684): both Codex and Claude Code agents get this
// sidecar. Claude Code's native Telegram channel plugin is disabled wherever
// this runs — two pollers on one bot token means constant 409s, the failure
// this platform already ate once in kyber#678/#679.
package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/matty-v/kyber/pkg/logging"
)

// maxUpdateAttempts bounds in-place retries of a single update before it is
// dropped, so one poison message cannot wedge the agent's whole inbox.
const maxUpdateAttempts = 5

type config struct {
	botToken      string
	allowedUsers  map[int64]bool
	inboundURL    string
	agentName     string
	bindingName   string
	hmacSecret    []byte
	healthAddr    string
	sendAddr      string
	mcpAddr       string
	downloadDir   string
	botAPIBaseURL string
	actions       *chatActionManager
	callbacks     *callbackRegistry
	albums        *albumCollector

	// chats bounds where the runtime may send. Inbound is allowlisted by user
	// id, but without this the send endpoint would accept any chat_id at all —
	// a strictly wider outbound capability than inbound, and a free primitive
	// for a prompt-injected agent. Seeded with each allowlisted user's DM chat
	// (for a private chat, chat_id == user id) and extended with any chat we
	// have actually forwarded an allowlisted message from, which is what makes
	// group replies keep working.
	chats *chatSet
	// files bounds download_attachment to file IDs observed on allowlisted
	// inbound messages. Telegram file IDs are bot-scoped but still capabilities.
	files *fileSet
}

// chatSet is a tiny concurrent-safe string set: the poll loop writes to it
// while the send handler reads.
type chatSet struct {
	mu sync.RWMutex
	m  map[string]bool
}

func newChatSet(seed map[int64]bool) *chatSet {
	s := &chatSet{m: map[string]bool{}}
	for id := range seed {
		s.m[strconv.FormatInt(id, 10)] = true
	}
	return s
}

func (s *chatSet) add(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[id] = true
}

func (s *chatSet) has(id string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.m[id]
}

type updateResponse struct {
	OK          bool             `json:"ok"`
	Description string           `json:"description"`
	Result      []telegramUpdate `json:"result"`
}

type telegramUpdate struct {
	UpdateID int64            `json:"update_id"`
	Message  *telegramMessage `json:"message"`
	// EditedMessage carries a user's correction. Without it the agent answers
	// the original wording and looks like it ignored the fix.
	EditedMessage   *telegramMessage         `json:"edited_message"`
	CallbackQuery   *telegramCallbackQuery   `json:"callback_query"`
	MessageReaction *telegramMessageReaction `json:"message_reaction"`
}

// effectiveMessage returns the message this update is about, and whether it was
// an edit — the two are handled identically except for the flag on the envelope.
func (u telegramUpdate) effectiveMessage() (*telegramMessage, bool) {
	if u.Message != nil {
		return u.Message, false
	}
	return u.EditedMessage, u.EditedMessage != nil
}

type telegramMessage struct {
	MessageID    int64        `json:"message_id"`
	Date         int64        `json:"date"`
	Text         string       `json:"text"`
	Caption      string       `json:"caption"`
	Chat         telegramChat `json:"chat"`
	From         telegramUser `json:"from"`
	MediaGroupID string       `json:"media_group_id"`

	// Attachments. Photo is an array of the same image at several resolutions,
	// ascending — the last entry is the largest, which is what an agent wants.
	Photo     []telegramPhotoSize `json:"photo"`
	Document  *telegramFile       `json:"document"`
	Voice     *telegramFile       `json:"voice"`
	Audio     *telegramFile       `json:"audio"`
	Video     *telegramFile       `json:"video"`
	Animation *telegramFile       `json:"animation"`
	VideoNote *telegramFile       `json:"video_note"`
	Sticker   *telegramFile       `json:"sticker"`
}

type telegramPhotoSize struct {
	FileID   string `json:"file_id"`
	FileSize int64  `json:"file_size"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
}

type telegramFile struct {
	FileID   string `json:"file_id"`
	FileName string `json:"file_name"`
	MimeType string `json:"mime_type"`
	FileSize int64  `json:"file_size"`
}

// attachment picks the single attachment worth telling the agent about, and
// names its kind. Telegram sends at most one per message.
func (m *telegramMessage) attachment() (kind, fileID, fileName string) {
	switch {
	case len(m.Photo) > 0:
		return "photo", m.Photo[len(m.Photo)-1].FileID, ""
	case m.Document != nil:
		return "document", m.Document.FileID, m.Document.FileName
	case m.Voice != nil:
		return "voice", m.Voice.FileID, ""
	case m.Audio != nil:
		return "audio", m.Audio.FileID, m.Audio.FileName
	case m.Video != nil:
		return "video", m.Video.FileID, m.Video.FileName
	case m.Animation != nil:
		return "animation", m.Animation.FileID, m.Animation.FileName
	case m.VideoNote != nil:
		return "video_note", m.VideoNote.FileID, m.VideoNote.FileName
	case m.Sticker != nil:
		return "sticker", m.Sticker.FileID, m.Sticker.FileName
	}
	return "", "", ""
}

type telegramChat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}

type telegramUser struct {
	ID        int64  `json:"id"`
	Username  string `json:"username"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
}

type telegramCallbackQuery struct {
	ID      string           `json:"id"`
	From    telegramUser     `json:"from"`
	Message *telegramMessage `json:"message"`
	Data    string           `json:"data"`
}

type telegramReactionType struct {
	Type          string `json:"type"`
	Emoji         string `json:"emoji"`
	CustomEmojiID string `json:"custom_emoji_id"`
}

type telegramMessageReaction struct {
	Chat        telegramChat           `json:"chat"`
	MessageID   int64                  `json:"message_id"`
	User        *telegramUser          `json:"user"`
	Date        int64                  `json:"date"`
	OldReaction []telegramReactionType `json:"old_reaction"`
	NewReaction []telegramReactionType `json:"new_reaction"`
}

type inboundEnvelope struct {
	EventType string `json:"event_type,omitempty"`
	Source    string `json:"source"`
	ChatID    string `json:"chat_id"`
	MessageID string `json:"message_id"`
	User      string `json:"user"`
	UserID    string `json:"user_id"`
	Username  string `json:"username"`
	Timestamp string `json:"ts"`
	Content   string `json:"content"`

	// Attachment metadata (kyber#684). Omitted when absent so existing bindings
	// see a byte-identical envelope for plain text. AttachmentFileID is what the
	// download_attachment tool takes.
	AttachmentType   string `json:"attachment_type,omitempty"`
	AttachmentFileID string `json:"attachment_file_id,omitempty"`
	AttachmentName   string `json:"attachment_name,omitempty"`
	Edited           bool   `json:"edited,omitempty"`
	MediaGroupID     string `json:"media_group_id,omitempty"`
	Attachments      string `json:"attachments,omitempty"`
	CallbackLabel    string `json:"callback_label,omitempty"`
	CallbackValue    string `json:"callback_value,omitempty"`
	ReactionOld      string `json:"reaction_old,omitempty"`
	ReactionNew      string `json:"reaction_new,omitempty"`
}

func main() {
	logger, err := logging.New(logging.Config{
		Component: "telegram-sidecar",
		Level:     os.Getenv("KYBER_LOG_LEVEL"),
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	slog.SetDefault(logger)

	cfg, err := loadConfig()
	if err != nil {
		slog.Error("telegram-sidecar: bad config", "error", err)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	client := &http.Client{Timeout: 40 * time.Second}
	cfg.actions = newChatActionManager(ctx, cfg, client)
	cfg.callbacks = newCallbackRegistry()
	cfg.albums = newAlbumCollector(ctx, cfg, client)
	defer cfg.actions.close()
	defer cfg.albums.close()
	health := startHealth(cfg.healthAddr)
	send := startSendServer(cfg.sendAddr, cfg, client)
	// The MCP tools get their own client: the poll client's 40s budget is sized
	// for a 30s long-poll, and a file upload should not be held to that.
	mcp := startMCPServer(cfg.mcpAddr, cfg, httpClientForTools(), cfg.downloadDir)
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = health.Shutdown(shutdownCtx)
		_ = send.Shutdown(shutdownCtx)
		_ = mcp.Shutdown(shutdownCtx)
	}()

	// Telegram forbids getUpdates while a webhook is set. A running pod
	// owns polling, so clear any webhook left registered from an older state.
	// Retry rather than exit: a Telegram outage must not crashloop this
	// container and hold the whole agent pod in Starting — the same rationale
	// the Discord sidecar applies to its Gateway connect.
	if !deleteWebhookUntilCleared(ctx, cfg, client, 5*time.Second) {
		return
	}
	slog.Info("telegram-sidecar: polling", "agent", cfg.agentName, "allowed_users", len(cfg.allowedUsers))

	var offset int64
	// A failing update must not block the queue forever. Retrying in place is
	// right for a transient control-plane blip, but an update that fails
	// deterministically (malformed field, binding rejecting the envelope) would
	// otherwise stall EVERY later message for this agent indefinitely, with no
	// dead-letter and nothing to alert on. Retry a bounded number of times, then
	// drop it loudly and move on.
	var failingUpdateID int64
	var failureCount int
	for ctx.Err() == nil {
		updates, err := getUpdates(ctx, cfg, client, offset)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			slog.Warn("telegram-sidecar: getUpdates", "error", err)
			time.Sleep(2 * time.Second)
			continue
		}
		for _, update := range updates {
			m, _ := update.effectiveMessage()
			content, attachmentKind := "", ""
			if m != nil {
				content = strings.TrimSpace(firstNonEmpty(m.Text, m.Caption))
				attachmentKind, _, _ = m.attachment()
			}
			switch {
			case update.CallbackQuery != nil && !cfg.allowedUsers[update.CallbackQuery.From.ID]:
				slog.Info("telegram-sidecar: ignored callback", "update_id", update.UpdateID,
					"reason", "user-not-allowed", "user_id", update.CallbackQuery.From.ID)
			case update.MessageReaction != nil && (update.MessageReaction.User == nil ||
				!cfg.allowedUsers[update.MessageReaction.User.ID]):
				slog.Info("telegram-sidecar: ignored reaction", "update_id", update.UpdateID,
					"reason", "user-not-allowed")
			case m == nil && update.CallbackQuery == nil && update.MessageReaction == nil:
				slog.Info("telegram-sidecar: ignored update", "update_id", update.UpdateID,
					"reason", "unsupported-update-type")
			case m != nil && !cfg.allowedUsers[m.From.ID]:
				slog.Info("telegram-sidecar: ignored update", "update_id", update.UpdateID,
					"reason", "user-not-allowed", "user_id", m.From.ID)
			case m != nil && content == "" && attachmentKind == "":
				slog.Info("telegram-sidecar: ignored update", "update_id", update.UpdateID,
					"reason", "empty-message", "user_id", m.From.ID)
			}
			if err := handleUpdate(ctx, cfg, client, update); err != nil {
				if update.UpdateID == failingUpdateID {
					failureCount++
				} else {
					failingUpdateID, failureCount = update.UpdateID, 1
				}
				if failureCount >= maxUpdateAttempts {
					slog.Error("telegram-sidecar: dropping update after repeated failures — it will NOT be retried",
						"update_id", update.UpdateID, "attempts", failureCount, "error", err)
					if update.UpdateID >= offset {
						offset = update.UpdateID + 1
					}
					failingUpdateID, failureCount = 0, 0
					continue
				}
				slog.Error("telegram-sidecar: forward update", "update_id", update.UpdateID,
					"attempt", failureCount, "error", err)
				time.Sleep(2 * time.Second)
				break // leave offset unchanged so Telegram retries this update
			}
			if update.UpdateID == failingUpdateID {
				failingUpdateID, failureCount = 0, 0
			}
			if update.UpdateID >= offset {
				offset = update.UpdateID + 1
			}
			// Log what was actually forwarded. This used to test update.Message
			// and a non-empty text/caption, which after kyber#684 no longer
			// matches what handleUpdate forwards — an edit or a caption-less
			// photo now reaches the agent while going unlogged, so the log said
			// "nothing arrived" for exactly the cases newest to this sidecar.
			if m, edited := update.effectiveMessage(); m != nil && cfg.allowedUsers[m.From.ID] {
				kind, _, _ := m.attachment()
				slog.Info("telegram-sidecar: forwarded message", "update_id", update.UpdateID,
					"message_id", m.MessageID, "user_id", m.From.ID, "attachment", kind, "edited", edited)
			}
		}
	}
}

func handleUpdate(ctx context.Context, cfg *config, client *http.Client, update telegramUpdate) error {
	if update.CallbackQuery != nil {
		return handleCallback(ctx, cfg, client, update.CallbackQuery)
	}
	if update.MessageReaction != nil {
		return handleReaction(ctx, cfg, client, update.MessageReaction)
	}
	m, edited := update.effectiveMessage()
	if m == nil || !cfg.allowedUsers[m.From.ID] {
		return nil
	}
	if m.MediaGroupID != "" && cfg.albums != nil {
		cfg.albums.add(*m)
		return nil
	}
	return forwardMessages(ctx, cfg, client, []*telegramMessage{m}, edited)
}

func forwardMessages(ctx context.Context, cfg *config, client *http.Client, messages []*telegramMessage, edited bool) error {
	if len(messages) == 0 {
		return nil
	}
	m := messages[0]
	content := strings.TrimSpace(firstNonEmpty(m.Text, m.Caption))
	kind, fileID, fileName := m.attachment()
	// A caption-less photo used to be dropped on the floor here: content was
	// empty, so the whole update was discarded and the user's picture never
	// reached the agent (kyber#684). An attachment IS content.
	if content == "" && kind == "" && len(messages) == 1 {
		return nil
	}
	if content == "" {
		content = "[" + kind + " with no caption]"
	}
	name := strings.TrimSpace(strings.TrimSpace(m.From.FirstName + " " + m.From.LastName))
	if name == "" {
		name = m.From.Username
	}
	chatID := strconv.FormatInt(m.Chat.ID, 10)
	// This chat has now demonstrably reached the agent through the inbound
	// allowlist, so replying into it is in scope (covers groups, where chat_id
	// is not any user's id).
	if cfg.chats != nil {
		cfg.chats.add(chatID)
	}
	attachments := make([]albumAttachment, 0, len(messages))
	for _, message := range messages {
		attachmentType, attachmentFileID, attachmentName := message.attachment()
		if attachmentFileID == "" {
			continue
		}
		if cfg.files != nil {
			cfg.files.add(attachmentFileID)
		}
		attachments = append(attachments, albumAttachment{Type: attachmentType, FileID: attachmentFileID,
			Name: attachmentName, MessageID: strconv.FormatInt(message.MessageID, 10)})
	}
	if cfg.actions != nil {
		cfg.actions.start(chatID)
	}
	env := inboundEnvelope{
		Source: "telegram", ChatID: chatID,
		MessageID: strconv.FormatInt(m.MessageID, 10), User: name,
		UserID: strconv.FormatInt(m.From.ID, 10), Username: m.From.Username,
		Timestamp: time.Unix(m.Date, 0).UTC().Format(time.RFC3339), Content: content,
		AttachmentType: kind, AttachmentFileID: fileID, AttachmentName: fileName,
		Edited: edited,
	}
	if m.MediaGroupID != "" {
		env.EventType = "album"
		env.MediaGroupID = m.MediaGroupID
		raw, err := json.Marshal(attachments)
		if err != nil {
			return fmt.Errorf("marshal album attachments: %w", err)
		}
		env.Attachments = string(raw)
		if strings.TrimSpace(firstNonEmpty(m.Text, m.Caption)) == "" {
			env.Content = fmt.Sprintf("[album with %d attachments and no caption]", len(attachments))
		}
	}
	if err := postInbound(ctx, cfg, client, env); err != nil {
		if cfg.actions != nil {
			cfg.actions.stop(chatID)
		}
		return err
	}
	return nil
}

func handleCallback(ctx context.Context, cfg *config, client *http.Client, query *telegramCallbackQuery) error {
	if query.Message == nil {
		return nil
	}
	if !cfg.allowedUsers[query.From.ID] {
		if err := answerCallbackQuery(ctx, cfg, client, query.ID, "Not allowed"); err != nil {
			slog.Warn("telegram-sidecar: blocked callback acknowledgement failed", "callback_id", query.ID, "error", err)
		}
		return nil
	}
	chatID := strconv.FormatInt(query.Message.Chat.ID, 10)
	entry, ok := cfg.callbacks.get(query.Data, chatID)
	ack := "This action expired"
	if ok {
		ack = "Received"
	}
	if err := answerCallbackQuery(ctx, cfg, client, query.ID, ack); err != nil {
		// Acknowledgement is UX, not delivery. Telegram rejects a second answer
		// when an inbound retry follows a control-plane failure; treating that as
		// fatal would prevent the callback from ever reaching the agent.
		slog.Warn("telegram-sidecar: callback acknowledgement failed", "callback_id", query.ID, "error", err)
	}
	if !ok {
		return nil
	}
	name := telegramDisplayName(query.From)
	env := inboundEnvelope{EventType: "callback", Source: "telegram", ChatID: chatID,
		MessageID: strconv.FormatInt(query.Message.MessageID, 10), User: name,
		UserID: strconv.FormatInt(query.From.ID, 10), Username: query.From.Username,
		Timestamp: time.Now().UTC().Format(time.RFC3339), Content: "Selected button: " + entry.Label,
		CallbackLabel: entry.Label, CallbackValue: entry.Value}
	if err := forwardEnvelope(ctx, cfg, client, env); err != nil {
		return err
	}
	// Consume only after Kyber accepted the event. If the inbound request fails,
	// Telegram retries the update and the callback must remain deliverable.
	_, _ = cfg.callbacks.consume(query.Data, chatID)
	return nil
}

func handleReaction(ctx context.Context, cfg *config, client *http.Client, reaction *telegramMessageReaction) error {
	if reaction.User == nil || !cfg.allowedUsers[reaction.User.ID] {
		return nil
	}
	chatID := strconv.FormatInt(reaction.Chat.ID, 10)
	oldReaction, newReaction := reactionText(reaction.OldReaction), reactionText(reaction.NewReaction)
	env := inboundEnvelope{EventType: "reaction", Source: "telegram", ChatID: chatID,
		MessageID: strconv.FormatInt(reaction.MessageID, 10), User: telegramDisplayName(*reaction.User),
		UserID: strconv.FormatInt(reaction.User.ID, 10), Username: reaction.User.Username,
		Timestamp:   time.Unix(reaction.Date, 0).UTC().Format(time.RFC3339),
		Content:     "Reaction changed: " + oldReaction + " -> " + newReaction,
		ReactionOld: oldReaction, ReactionNew: newReaction}
	return forwardEnvelope(ctx, cfg, client, env)
}

func forwardEnvelope(ctx context.Context, cfg *config, client *http.Client, env inboundEnvelope) error {
	if cfg.chats != nil {
		cfg.chats.add(env.ChatID)
	}
	if cfg.actions != nil {
		cfg.actions.start(env.ChatID)
	}
	if err := postInbound(ctx, cfg, client, env); err != nil {
		if cfg.actions != nil {
			cfg.actions.stop(env.ChatID)
		}
		return err
	}
	return nil
}

func telegramDisplayName(user telegramUser) string {
	name := strings.TrimSpace(user.FirstName + " " + user.LastName)
	if name == "" {
		return user.Username
	}
	return name
}

func reactionText(reactions []telegramReactionType) string {
	values := make([]string, 0, len(reactions))
	for _, reaction := range reactions {
		if reaction.Emoji != "" {
			values = append(values, reaction.Emoji)
		} else if reaction.CustomEmojiID != "" {
			values = append(values, "custom_emoji:"+reaction.CustomEmojiID)
		} else if reaction.Type != "" {
			values = append(values, reaction.Type)
		}
	}
	if len(values) == 0 {
		return "none"
	}
	return strings.Join(values, ", ")
}

func getUpdates(ctx context.Context, cfg *config, client *http.Client, offset int64) ([]telegramUpdate, error) {
	// edited_message is opt-in: Telegram omits it from the default set, so
	// without naming it here a user's correction never arrives (kyber#684).
	values := url.Values{"timeout": {"30"}, "allowed_updates": {`["message","edited_message","callback_query","message_reaction"]`}}
	if offset > 0 {
		values.Set("offset", strconv.FormatInt(offset, 10))
	}
	endpoint := fmt.Sprintf("%s/bot%s/getUpdates?%s", strings.TrimRight(cfg.botAPIBaseURL, "/"), cfg.botToken, values.Encode())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("new getUpdates request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("get updates: %w", scrubToken(err, cfg.botToken))
	}
	defer resp.Body.Close()
	var result updateResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode getUpdates response: %w", err)
	}
	if resp.StatusCode >= 300 || !result.OK {
		return nil, fmt.Errorf("getUpdates returned %d: %s", resp.StatusCode, result.Description)
	}
	return result.Result, nil
}

// deleteWebhookUntilCleared retries the webhook teardown until it succeeds or
// the process is shutting down. Returns false only when ctx was cancelled.
func deleteWebhookUntilCleared(ctx context.Context, cfg *config, client *http.Client, retryDelay time.Duration) bool {
	for {
		err := deleteWebhook(ctx, cfg, client)
		if err == nil {
			return true
		}
		slog.Warn("telegram-sidecar: delete webhook failed; retrying", "error", err, "retry_in", retryDelay)
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

func deleteWebhook(ctx context.Context, cfg *config, client *http.Client) error {
	endpoint := fmt.Sprintf("%s/bot%s/deleteWebhook", strings.TrimRight(cfg.botAPIBaseURL, "/"), cfg.botToken)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader("drop_pending_updates=false"))
	if err != nil {
		return fmt.Errorf("new deleteWebhook request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("delete webhook: %w", scrubToken(err, cfg.botToken))
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("deleteWebhook returned %d", resp.StatusCode)
	}
	return nil
}

func postInbound(ctx context.Context, cfg *config, client *http.Client, env inboundEnvelope) error {
	body, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("marshal envelope: %w", err)
	}
	mac := hmac.New(sha256.New, cfg.hmacSecret)
	_, _ = mac.Write(body)
	endpoint := fmt.Sprintf("%s/webhooks/inbound/%s/%s", strings.TrimRight(cfg.inboundURL, "/"), cfg.agentName, cfg.bindingName)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("new inbound request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Kyber-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
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

func loadConfig() (*config, error) {
	cfg := &config{
		botToken: os.Getenv("TELEGRAM_BOT_TOKEN"), allowedUsers: idSet(os.Getenv("TELEGRAM_ALLOWED_USER_IDS")),
		inboundURL: os.Getenv("KYBER_INBOUND_URL"), agentName: os.Getenv("KYBER_AGENT_NAME"),
		bindingName: envOr("KYBER_INBOUND_BINDING", "telegram"), hmacSecret: []byte(os.Getenv("KYBER_INBOUND_HMAC_SECRET")),
		healthAddr: envOr("KYBER_TELEGRAM_HEALTH_ADDR", ":14003"), sendAddr: envOr("KYBER_TELEGRAM_SEND_ADDR", "127.0.0.1:14004"),
		// Loopback only: the agent container shares this pod's network
		// namespace, so binding 127.0.0.1 makes the pod boundary the auth
		// boundary — the same one /send already relies on.
		// 14006, not 14005 — 14005 is kyber-mcp-discord's /send, and both
		// sidecars share one loopback space in the agent pod. See the port table
		// on pkgruntimes.TelegramMCPPort.
		mcpAddr: envOr("KYBER_TELEGRAM_MCP_ADDR", "127.0.0.1:14006"),
		// The controller sets this explicitly; the default only covers running
		// the binary by hand. It must be a path the AGENT container sees at the
		// same location — see pkgruntimes.TelegramAttachmentDir.
		downloadDir: envOr("KYBER_TELEGRAM_DOWNLOAD_DIR", "/persist/telegram-attachments"),

		botAPIBaseURL: envOr("TELEGRAM_API_BASE_URL", "https://api.telegram.org"),
	}
	for name, value := range map[string]string{"TELEGRAM_BOT_TOKEN": cfg.botToken, "KYBER_INBOUND_URL": cfg.inboundURL, "KYBER_AGENT_NAME": cfg.agentName} {
		if value == "" {
			return nil, fmt.Errorf("%s is required", name)
		}
	}
	// Both of these arrive from the agent's "<name>-telegram" Secret, and both
	// can legitimately be absent on an agent configured before kyber#684 — the
	// retired Claude Code plugin stored only a bot token. The controller
	// backfills them (migrateLegacyTelegramSecret), so reaching this point means
	// that backfill has not happened or could not. Name the fix: a bare
	// "X is required" sends whoever reads it hunting through the pod spec for a
	// value that is not set there.
	if len(cfg.hmacSecret) == 0 {
		return nil, fmt.Errorf("KYBER_INBOUND_HMAC_SECRET is required — it comes from the %q key on this "+
			"agent's Secret, which the control plane generates; if it is missing, the agent's Telegram "+
			"config predates kyber#684 and the control plane is too old to have healed it", "webhook-secret")
	}
	if len(cfg.allowedUsers) == 0 {
		return nil, fmt.Errorf("TELEGRAM_ALLOWED_USER_IDS must contain at least one numeric Telegram user ID — " +
			"it comes from the \"allowed-user-ids\" key on this agent's Secret. Set it for this agent through " +
			"the /comms API, or set telegram.defaultAllowedUserIds in the install's Helm values to seed every " +
			"agent migrated off the retired plugin")
	}
	cfg.chats = newChatSet(cfg.allowedUsers)
	cfg.files = newFileSet()
	return cfg, nil
}

func idSet(raw string) map[int64]bool {
	out := map[int64]bool{}
	for _, value := range strings.Split(raw, ",") {
		if id, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64); err == nil && id > 0 {
			out[id] = true
		}
	}
	return out
}

func startHealth(addr string) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	return startHTTPServer("health", addr, mux)
}

func startSendServer(addr string, cfg *config, client *http.Client) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/send", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if err := r.ParseForm(); err != nil || r.Form.Get("chat_id") == "" || r.Form.Get("text") == "" {
			http.Error(w, "chat_id and text are required", http.StatusBadRequest)
			return
		}
		if cfg.chats != nil && !cfg.chats.has(r.Form.Get("chat_id")) {
			slog.Warn("telegram-sidecar: outbound send refused — chat is not an allowlisted user's DM and has not messaged this agent",
				"chat_id", r.Form.Get("chat_id"))
			http.Error(w, "chat_id is not in scope for this agent", http.StatusForbidden)
			return
		}
		endpoint := fmt.Sprintf("%s/bot%s/sendMessage", strings.TrimRight(cfg.botAPIBaseURL, "/"), cfg.botToken)
		resp, err := client.PostForm(endpoint, url.Values{"chat_id": {r.Form.Get("chat_id")}, "text": {r.Form.Get("text")}})
		if err != nil {
			// Without this the agent sees a bare 502 and the operator sees
			// nothing at all — a failing outbound reply was previously silent.
			slog.Warn("telegram-sidecar: outbound send failed", "error", scrubToken(err, cfg.botToken), "chat_id", r.Form.Get("chat_id"))
			http.Error(w, "Telegram send failed", http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 300 {
			detail, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<10))
			slog.Warn("telegram-sidecar: outbound send rejected", "status", resp.StatusCode,
				"chat_id", r.Form.Get("chat_id"), "detail", strings.TrimSpace(string(detail)))
			http.Error(w, "Telegram send failed", http.StatusBadGateway)
			return
		}
		if cfg.actions != nil {
			cfg.actions.stop(r.Form.Get("chat_id"))
		}
		w.WriteHeader(http.StatusNoContent)
	})
	return startHTTPServer("send", addr, mux)
}

func startHTTPServer(name, addr string, handler http.Handler) *http.Server {
	srv := &http.Server{Addr: addr, Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("telegram-sidecar: "+name+" server", "error", err)
		}
	}()
	return srv
}

// scrubToken strips the bot token out of an error message. net/http wraps
// request failures in *url.Error, whose Error() embeds the full URL — and the
// Telegram Bot API carries the token in the path, so an unscrubbed error would
// print the live credential into pod logs on any transient network failure.
func scrubToken(err error, token string) error {
	if err == nil || token == "" {
		return err
	}
	if msg := err.Error(); strings.Contains(msg, token) {
		return errors.New(strings.ReplaceAll(msg, token, "<redacted>"))
	}
	return err
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

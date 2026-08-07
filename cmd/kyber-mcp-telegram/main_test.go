package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestHandleUpdateForwardsAllowlistedMessage(t *testing.T) {
	secret := []byte("test-secret")
	var got inboundEnvelope
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mac := hmac.New(sha256.New, secret)
		_, _ = mac.Write(body)
		wantSig := "sha256=" + hex.EncodeToString(mac.Sum(nil))
		if r.Header.Get("X-Kyber-Signature-256") != wantSig {
			t.Errorf("bad signature")
		}
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatal(err)
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()
	cfg := &config{allowedUsers: map[int64]bool{42: true}, inboundURL: server.URL, agentName: "dave", bindingName: "telegram", hmacSecret: secret}
	err := handleUpdate(context.Background(), cfg, server.Client(), telegramUpdate{UpdateID: 1, Message: &telegramMessage{
		MessageID: 7, Date: 1, Text: "hello", Chat: telegramChat{ID: 99}, From: telegramUser{ID: 42, FirstName: "Matt"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if got.Content != "hello" || got.ChatID != "99" || got.UserID != "42" || got.MessageID != "7" {
		t.Fatalf("envelope=%+v", got)
	}
}

func TestHandleUpdateStartsTypingAndStopsOnInboundFailure(t *testing.T) {
	bot := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
	}))
	defer bot.Close()
	inbound := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer inbound.Close()

	cfg := &config{botToken: "TOK", botAPIBaseURL: bot.URL, allowedUsers: map[int64]bool{42: true},
		inboundURL: inbound.URL, agentName: "dave", bindingName: "telegram", hmacSecret: []byte("s")}
	cfg.actions = newChatActionManager(context.Background(), cfg, bot.Client())
	defer cfg.actions.close()

	err := handleUpdate(context.Background(), cfg, inbound.Client(), telegramUpdate{Message: &telegramMessage{
		Text: "hello", Chat: telegramChat{ID: 99}, From: telegramUser{ID: 42},
	}})
	if err == nil {
		t.Fatal("expected inbound failure")
	}
	if cfg.actions.active("99") {
		t.Fatal("typing lease remained active after inbound rejection")
	}
}

// The bot token sits in the Telegram API path, and net/http wraps transport
// failures in *url.Error — whose message carries the full URL. Without
// scrubbing, one flaky poll prints the live credential into pod logs.
func TestGetUpdatesErrorDoesNotLeakBotToken(t *testing.T) {
	const token = "123456:SUPER-SECRET-BOT-TOKEN"
	// Point at a closed port so client.Do fails at the transport layer.
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := server.URL
	server.Close()

	cfg := &config{botToken: token, botAPIBaseURL: deadURL}
	_, err := getUpdates(context.Background(), cfg, &http.Client{Timeout: time.Second}, 0)
	if err == nil {
		t.Fatal("expected a transport error")
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("bot token leaked into error: %v", err)
	}
	if !strings.Contains(err.Error(), "<redacted>") {
		t.Fatalf("expected redaction marker, got: %v", err)
	}
}

func TestScrubTokenLeavesUnrelatedErrorsAlone(t *testing.T) {
	err := errors.New("dial tcp: connection refused")
	if got := scrubToken(err, "tok"); got != err {
		t.Fatalf("expected the original error back, got %v", got)
	}
	if got := scrubToken(nil, "tok"); got != nil {
		t.Fatalf("expected nil, got %v", got)
	}
}

func TestHandleUpdateDropsNonAllowlistedUser(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	defer server.Close()
	cfg := &config{allowedUsers: map[int64]bool{42: true}, inboundURL: server.URL, agentName: "dave", bindingName: "telegram", hmacSecret: []byte("s")}
	if err := handleUpdate(context.Background(), cfg, server.Client(), telegramUpdate{Message: &telegramMessage{Text: "no", From: telegramUser{ID: 43}}}); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("non-allowlisted update was forwarded")
	}
}

func TestHandleCallbackAcknowledgesAndForwardsValue(t *testing.T) {
	secret := []byte("s")
	var got inboundEnvelope
	var acknowledged bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/answerCallbackQuery") {
			acknowledged = true
			_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()
	cfg := &config{botToken: "t", botAPIBaseURL: server.URL, allowedUsers: map[int64]bool{42: true},
		inboundURL: server.URL, agentName: "dave", bindingName: "telegram", hmacSecret: secret,
		callbacks: newCallbackRegistry(), chats: newChatSet(map[int64]bool{42: true})}
	_, tokens, err := cfg.callbacks.register("42", []callbackButton{{Text: "Deploy", Value: "prod"}})
	if err != nil {
		t.Fatal(err)
	}
	update := telegramUpdate{CallbackQuery: &telegramCallbackQuery{ID: "q1", Data: tokens[0],
		From: telegramUser{ID: 42}, Message: &telegramMessage{MessageID: 9, Chat: telegramChat{ID: 42}}}}
	if err := handleUpdate(context.Background(), cfg, server.Client(), update); err != nil {
		t.Fatal(err)
	}
	// MessageID is the button-bearing message, which is what the agent has to
	// edit to clear the keyboard after handling the callback.
	if !acknowledged || got.EventType != "callback" || got.CallbackValue != "prod" ||
		got.CallbackLabel != "Deploy" || got.MessageID != "9" {
		t.Fatalf("acknowledged=%v envelope=%+v", acknowledged, got)
	}
	if err := handleUpdate(context.Background(), cfg, server.Client(), update); err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.callbacks.consume(tokens[0], "42"); ok {
		t.Fatal("callback token was not one-shot")
	}
}

func TestHandleCallbackRetainsTokenWhenInboundFails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/answerCallbackQuery") {
			_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	cfg := &config{botToken: "t", botAPIBaseURL: server.URL, allowedUsers: map[int64]bool{42: true},
		inboundURL: server.URL, agentName: "dave", bindingName: "telegram", hmacSecret: []byte("s"),
		callbacks: newCallbackRegistry()}
	_, tokens, err := cfg.callbacks.register("42", []callbackButton{{Text: "Deploy", Value: "prod"}})
	if err != nil {
		t.Fatal(err)
	}
	err = handleUpdate(context.Background(), cfg, server.Client(), telegramUpdate{CallbackQuery: &telegramCallbackQuery{
		ID: "q1", Data: tokens[0], From: telegramUser{ID: 42}, Message: &telegramMessage{Chat: telegramChat{ID: 42}},
	}})
	if err == nil {
		t.Fatal("expected inbound failure")
	}
	if _, ok := cfg.callbacks.get(tokens[0], "42"); !ok {
		t.Fatal("callback token was consumed before inbound delivery succeeded")
	}
}

func TestHandleCallbackForwardsWhenAcknowledgementFails(t *testing.T) {
	forwarded := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/answerCallbackQuery") {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"ok":false,"description":"already answered"}`))
			return
		}
		forwarded = true
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()
	cfg := &config{botToken: "t", botAPIBaseURL: server.URL, allowedUsers: map[int64]bool{42: true},
		inboundURL: server.URL, agentName: "dave", bindingName: "telegram", hmacSecret: []byte("s"),
		callbacks: newCallbackRegistry()}
	_, tokens, err := cfg.callbacks.register("42", []callbackButton{{Text: "Deploy", Value: "prod"}})
	if err != nil {
		t.Fatal(err)
	}
	err = handleUpdate(context.Background(), cfg, server.Client(), telegramUpdate{CallbackQuery: &telegramCallbackQuery{
		ID: "q1", Data: tokens[0], From: telegramUser{ID: 42}, Message: &telegramMessage{Chat: telegramChat{ID: 42}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !forwarded {
		t.Fatal("callback was not forwarded after acknowledgement failure")
	}
}

func TestHandleReactionForwardsAllowlistedReaction(t *testing.T) {
	var got inboundEnvelope
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()
	cfg := &config{allowedUsers: map[int64]bool{42: true}, inboundURL: server.URL, agentName: "dave",
		bindingName: "telegram", hmacSecret: []byte("s")}
	user := telegramUser{ID: 42}
	err := handleUpdate(context.Background(), cfg, server.Client(), telegramUpdate{MessageReaction: &telegramMessageReaction{
		Chat: telegramChat{ID: 42}, MessageID: 7, User: &user,
		OldReaction: []telegramReactionType{{Type: "emoji", Emoji: "👍"}},
		NewReaction: []telegramReactionType{{Type: "emoji", Emoji: "❤️"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if got.EventType != "reaction" || got.ReactionOld != "👍" || got.ReactionNew != "❤️" || got.MessageID != "7" {
		t.Fatalf("envelope=%+v", got)
	}
}

func TestReactionTextPreservesCustomEmojiIdentity(t *testing.T) {
	got := reactionText([]telegramReactionType{{Type: "custom_emoji", CustomEmojiID: "emoji-123"}})
	if got != "custom_emoji:emoji-123" {
		t.Fatalf("reactionText=%q", got)
	}
}

// Outbound must not be a wider capability than inbound. The send endpoint only
// accepts chats the agent legitimately talks to: an allowlisted user's DM, or a
// chat that has actually reached it through the inbound allowlist.
func TestSendServerChatScope(t *testing.T) {
	var sent url.Values
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		sent = r.Form
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	cfg := &config{botToken: "t", botAPIBaseURL: upstream.URL, allowedUsers: map[int64]bool{42: true}}
	cfg.chats = newChatSet(cfg.allowedUsers)
	srv := startSendServer("127.0.0.1:0", cfg, upstream.Client())
	defer func() { _ = srv.Close() }()
	handler := srv.Handler

	post := func(chatID string) int {
		body := strings.NewReader(url.Values{"chat_id": {chatID}, "text": {"hi"}}.Encode())
		req := httptest.NewRequest(http.MethodPost, "/send", body)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec.Code
	}

	// 42 is an allowlisted user, so their DM chat is in scope from the start.
	if got := post("42"); got != http.StatusNoContent {
		t.Fatalf("DM to allowlisted user: status = %d, want 204", got)
	}
	if sent.Get("chat_id") != "42" {
		t.Fatalf("upstream chat_id = %q", sent.Get("chat_id"))
	}
	// A group chat nobody has messaged from is out of scope.
	if got := post("-100987"); got != http.StatusForbidden {
		t.Fatalf("unknown group: status = %d, want 403", got)
	}
	// Once an allowlisted user posts in that group, replying there is fine.
	cfg.chats.add("-100987")
	if got := post("-100987"); got != http.StatusNoContent {
		t.Fatalf("group after inbound: status = %d, want 204", got)
	}
}

// handleUpdate must record the originating chat so a group reply is allowed.
func TestHandleUpdateRecordsChatScope(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()
	cfg := &config{allowedUsers: map[int64]bool{42: true}, inboundURL: server.URL, agentName: "dave",
		bindingName: "telegram", hmacSecret: []byte("s")}
	cfg.chats = newChatSet(cfg.allowedUsers)

	if cfg.chats.has("-100987") {
		t.Fatal("group chat should not be in scope before any inbound message")
	}
	if err := handleUpdate(context.Background(), cfg, server.Client(), telegramUpdate{UpdateID: 1,
		Message: &telegramMessage{MessageID: 7, Date: 1, Text: "hi", Chat: telegramChat{ID: -100987},
			From: telegramUser{ID: 42, FirstName: "Matt"}}}); err != nil {
		t.Fatal(err)
	}
	if !cfg.chats.has("-100987") {
		t.Fatal("group chat was not recorded after an allowlisted inbound message")
	}
}

func TestHandleUpdateRecordsAllowlistedAttachmentFileID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()
	cfg := &config{allowedUsers: map[int64]bool{42: true}, inboundURL: server.URL, agentName: "dave",
		bindingName: "telegram", hmacSecret: []byte("s"), files: newFileSet()}

	if err := handleUpdate(context.Background(), cfg, server.Client(), telegramUpdate{Message: &telegramMessage{
		Chat: telegramChat{ID: 42}, From: telegramUser{ID: 42},
		Document: &telegramFile{FileID: "observed-file", FileName: "report.pdf"},
	}}); err != nil {
		t.Fatal(err)
	}
	if !cfg.files.has("observed-file") {
		t.Fatal("allowlisted inbound attachment was not added to file scope")
	}
}

func TestHandleUpdateDoesNotScopeBlockedUsersAttachment(t *testing.T) {
	cfg := &config{allowedUsers: map[int64]bool{42: true}, files: newFileSet()}
	if err := handleUpdate(context.Background(), cfg, http.DefaultClient, telegramUpdate{Message: &telegramMessage{
		From: telegramUser{ID: 43}, Document: &telegramFile{FileID: "blocked-file"},
	}}); err != nil {
		t.Fatal(err)
	}
	if cfg.files.has("blocked-file") {
		t.Fatal("blocked user's attachment entered file scope")
	}
}

func TestFileScopeIsBounded(t *testing.T) {
	files := newFileSet()
	for i := 0; i <= maxScopedTelegramFiles; i++ {
		files.add(strconv.Itoa(i))
	}
	if files.has("0") {
		t.Fatal("oldest file ID was not evicted when the scope reached its bound")
	}
	if !files.has(strconv.Itoa(maxScopedTelegramFiles)) {
		t.Fatal("newest file ID was not retained")
	}
}

// A message from a non-allowlisted user must not put its chat in send scope.
func TestHandleUpdateDoesNotScopeChatForBlockedUser(t *testing.T) {
	cfg := &config{allowedUsers: map[int64]bool{42: true}, inboundURL: "http://unused", agentName: "dave",
		bindingName: "telegram", hmacSecret: []byte("s")}
	cfg.chats = newChatSet(cfg.allowedUsers)
	if err := handleUpdate(context.Background(), cfg, http.DefaultClient, telegramUpdate{UpdateID: 1,
		Message: &telegramMessage{Text: "no", Chat: telegramChat{ID: -55}, From: telegramUser{ID: 43}}}); err != nil {
		t.Fatal(err)
	}
	if cfg.chats.has("-55") {
		t.Fatal("chat of a non-allowlisted user entered send scope")
	}
}

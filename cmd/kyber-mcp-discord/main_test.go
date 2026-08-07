package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

func TestOpenGatewayUntilConnectedRetries(t *testing.T) {
	calls := 0
	connected := openGatewayUntilConnected(context.Background(), func() error {
		calls++
		if calls < 3 {
			return fmt.Errorf("not yet")
		}
		return nil
	}, time.Millisecond)
	if !connected || calls != 3 {
		t.Fatalf("connected=%v calls=%d, want true/3", connected, calls)
	}
}

func TestDiscordGatewayIntentsIncludeThreadState(t *testing.T) {
	if discordGatewayIntents&discordgo.IntentsGuilds == 0 || discordGatewayIntents&discordgo.IntentsGuildMessages == 0 || discordGatewayIntents&discordgo.IntentsMessageContent == 0 {
		t.Fatalf("gateway intents %d must include guild/thread state, guild messages, and message content", discordGatewayIntents)
	}
}

func TestOpenGatewayUntilConnectedStopsWithContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	connected := openGatewayUntilConnected(ctx, func() error {
		calls++
		return fmt.Errorf("still unavailable")
	}, time.Hour)
	if connected || calls != 1 {
		t.Fatalf("connected=%v calls=%d, want false/1", connected, calls)
	}
}

type fakeDiscordSender struct {
	channelID string
	message   *discordgo.MessageSend
	messages  []*discordgo.MessageSend
	err       error
	errorAt   int
	sendCalls int
	typing    []string
	added     []string
	removed   []string
	edited    *discordgo.MessageEdit
}

func (f *fakeDiscordSender) ChannelMessageEditComplex(message *discordgo.MessageEdit, _ ...discordgo.RequestOption) (*discordgo.Message, error) {
	f.edited = message
	if f.err != nil {
		return nil, f.err
	}
	return &discordgo.Message{ID: message.ID}, nil
}

func (f *fakeDiscordSender) ChannelTyping(channelID string, _ ...discordgo.RequestOption) error {
	f.typing = append(f.typing, channelID)
	return f.err
}

func (f *fakeDiscordSender) MessageReactionAdd(channelID, messageID, emojiID string, _ ...discordgo.RequestOption) error {
	f.added = append(f.added, channelID+"/"+messageID+"/"+emojiID)
	return f.err
}

func (f *fakeDiscordSender) MessageReactionRemove(channelID, messageID, emojiID, userID string, _ ...discordgo.RequestOption) error {
	f.removed = append(f.removed, channelID+"/"+messageID+"/"+emojiID+"/"+userID)
	return f.err
}

func (f *fakeDiscordSender) ChannelMessageSendComplex(channelID string, message *discordgo.MessageSend, _ ...discordgo.RequestOption) (*discordgo.Message, error) {
	f.sendCalls++
	f.channelID = channelID
	f.message = message
	f.messages = append(f.messages, message)
	if f.err != nil && (f.errorAt == 0 || f.errorAt == f.sendCalls) {
		return nil, f.err
	}
	return &discordgo.Message{ID: fmt.Sprintf("%d", 788+f.sendCalls)}, nil
}

func TestSendHandler(t *testing.T) {
	tests := []struct {
		name       string
		method     string
		body       string
		sendErr    error
		allowed    map[string]bool
		wantStatus int
	}{
		{name: "sends message", method: http.MethodPost, body: `{"channel_id":"123","content":"hello"}`, wantStatus: http.StatusNoContent},
		{name: "sends reply", method: http.MethodPost, body: `{"channel_id":"123","content":"hello","message_id":"456"}`, wantStatus: http.StatusNoContent},
		{name: "rejects missing content", method: http.MethodPost, body: `{"channel_id":"123"}`, wantStatus: http.StatusBadRequest},
		{name: "rejects unknown field", method: http.MethodPost, body: `{"channel_id":"123","content":"hello","token":"secret"}`, wantStatus: http.StatusBadRequest},
		{name: "rejects other methods", method: http.MethodGet, wantStatus: http.StatusMethodNotAllowed},
		{name: "maps Discord failure", method: http.MethodPost, body: `{"channel_id":"123","content":"hello"}`, sendErr: fmt.Errorf("upstream failed"), wantStatus: http.StatusBadGateway},
		// Outbound must not be wider than inbound: when the channel allowlist is
		// set, the runtime may only reply into those channels.
		{name: "allows an allowlisted channel", method: http.MethodPost, body: `{"channel_id":"123","content":"hello"}`, allowed: map[string]bool{"123": true}, wantStatus: http.StatusNoContent},
		{name: "refuses a channel outside the allowlist", method: http.MethodPost, body: `{"channel_id":"999","content":"hello"}`, allowed: map[string]bool{"123": true}, wantStatus: http.StatusForbidden},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sender := &fakeDiscordSender{err: tc.sendErr}
			req := httptest.NewRequest(tc.method, "/send", bytes.NewBufferString(tc.body))
			resp := httptest.NewRecorder()
			sendHandler(sender, tc.allowed, nil).ServeHTTP(resp, req)
			if resp.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", resp.Code, tc.wantStatus, resp.Body.String())
			}
			if tc.wantStatus != http.StatusNoContent {
				return
			}
			if sender.channelID != "123" || sender.message == nil || sender.message.Content != "hello" {
				t.Fatalf("send = channel %q message %+v", sender.channelID, sender.message)
			}
			if tc.name == "sends reply" {
				if sender.message.Reference == nil || sender.message.Reference.MessageID != "456" || sender.message.Reference.ChannelID != "123" {
					t.Fatalf("reference = %+v", sender.message.Reference)
				}
			} else if sender.message.Reference != nil {
				t.Fatalf("unexpected reference = %+v", sender.message.Reference)
			}
		})
	}
}

func TestCSVSet(t *testing.T) {
	got := csvSet(" a, b ,,c,")
	for _, want := range []string{"a", "b", "c"} {
		if !got[want] {
			t.Errorf("csvSet missing %q", want)
		}
	}
	if len(got) != 3 {
		t.Errorf("csvSet size = %d, want 3 (empties trimmed)", len(got))
	}
	if len(csvSet("")) != 0 {
		t.Error("csvSet(\"\") should be empty (fail-closed allowlist)")
	}
}

// makeMsg builds a MessageCreate from an allowlisted-or-not user in a channel.
func makeMsg(userID, channelID, guildID, content string, bot bool) *discordgo.MessageCreate {
	return &discordgo.MessageCreate{Message: &discordgo.Message{
		ID:        "m1",
		ChannelID: channelID,
		GuildID:   guildID,
		Content:   content,
		Timestamp: time.Now(),
		Author:    &discordgo.User{ID: userID, Username: "tester", Bot: bot},
	}}
}

func TestHandleMessage_Allowlist(t *testing.T) {
	var delivered int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		delivered++
		// Verify the HMAC the sidecar sent matches what the inbound binding would compute.
		body, _ := io.ReadAll(r.Body)
		mac := hmac.New(sha256.New, []byte("s3cr3t"))
		mac.Write(body)
		want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
		if got := r.Header.Get("X-Kyber-Signature-256"); got != want {
			t.Errorf("signature = %q, want %q", got, want)
		}
		var env inboundEnvelope
		if err := json.Unmarshal(body, &env); err != nil {
			t.Fatalf("envelope not JSON: %v", err)
		}
		if env.Source != "discord" || env.UserID != "matt" || env.Content != "hi barf" {
			t.Errorf("bad envelope: %+v", env)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := &config{
		allowedUsers:    map[string]bool{"matt": true, "steve": true},
		allowedChannels: map[string]bool{"chan1": true},
		inboundURL:      srv.URL,
		agentName:       "barf",
		bindingName:     "discord",
		hmacSecret:      []byte("s3cr3t"),
		sigHeader:       "X-Kyber-Signature-256",
		sigPrefix:       "sha256=",
		eventValue:      "message",
		requestTimeout:  5 * time.Second,
	}
	sess := &discordgo.Session{State: discordgo.NewState()}
	sess.State.User = &discordgo.User{ID: "barfbot"}
	client := srv.Client()

	cases := []struct {
		name      string
		msg       *discordgo.MessageCreate
		wantDeliv bool
	}{
		{"allowlisted user in allowed channel", makeMsg("matt", "chan1", "g1", "hi barf", false), true},
		{"non-allowlisted user", makeMsg("randomer", "chan1", "g1", "hi barf", false), false},
		{"allowlisted user wrong channel", makeMsg("matt", "chan2", "g1", "hi barf", false), false},
		{"bot author ignored", makeMsg("matt", "chan1", "g1", "hi barf", true), false},
		{"self ignored", makeMsg("barfbot", "chan1", "g1", "hi barf", false), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			delivered = 0
			handleMessage(t.Context(), cfg, client, sess, tc.msg)
			if (delivered == 1) != tc.wantDeliv {
				t.Errorf("delivered=%d, wantDelivered=%v", delivered, tc.wantDeliv)
			}
		})
	}
}

func TestHandleMessage_AllowsThreadUnderAllowlistedParent(t *testing.T) {
	var got inboundEnvelope
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	parents := &sync.Map{}
	cfg := &config{allowedUsers: map[string]bool{"matt": true}, allowedChannels: map[string]bool{"parent": true},
		inboundURL: srv.URL, agentName: "barf", bindingName: "discord", hmacSecret: []byte("x"),
		sigHeader: "X-Kyber-Signature-256", sigPrefix: "sha256=", requestTimeout: time.Second, threadParents: parents,
		contextLoader: func(_, _ string, limit int) ([]*discordgo.Message, error) {
			if limit != 5 {
				t.Fatalf("context limit = %d", limit)
			}
			return []*discordgo.Message{{ID: "old", Content: "earlier", Author: &discordgo.User{ID: "steve", Username: "Steve"}}}, nil
		}}
	sess := &discordgo.Session{State: discordgo.NewState()}
	sess.State.User = &discordgo.User{ID: "barfbot"}
	if err := sess.State.GuildAdd(&discordgo.Guild{ID: "g1"}); err != nil {
		t.Fatal(err)
	}
	if err := sess.State.ChannelAdd(&discordgo.Channel{ID: "thread", GuildID: "g1", Name: "release-work", ParentID: "parent", Type: discordgo.ChannelTypeGuildPublicThread}); err != nil {
		t.Fatal(err)
	}
	msg := makeMsg("matt", "thread", "g1", "hi barf", false)
	msg.ReferencedMessage = &discordgo.Message{ID: "ref", Content: "the referenced point", Author: &discordgo.User{ID: "steve", Username: "Steve"}}
	handleMessage(t.Context(), cfg, srv.Client(), sess, msg)
	if got.ThreadID != "thread" || got.ThreadName != "release-work" || got.ParentChannelID != "parent" || got.ChannelID != "thread" {
		t.Fatalf("thread envelope = %+v", got)
	}
	if parent, ok := parents.Load("thread"); !ok || parent != "parent" {
		t.Fatalf("observed thread parent = %v, %v", parent, ok)
	}
	if got.ReferencedMessage == nil || got.ReferencedMessage.MessageID != "ref" || len(got.RecentContext) != 1 || got.RecentContext[0].MessageID != "old" {
		t.Fatalf("message context = referenced=%+v recent=%+v", got.ReferencedMessage, got.RecentContext)
	}
}

func TestDiscordThreadContextFallsBackToRESTOnCacheMiss(t *testing.T) {
	sess, err := discordgo.New("Bot token")
	if err != nil {
		t.Fatal(err)
	}
	sess.State = discordgo.NewState()
	sess.Client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if !strings.Contains(req.URL.Path, "/channels/thread") {
			t.Fatalf("unexpected Discord request path %q", req.URL.Path)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(
			`{"id":"thread","guild_id":"g1","name":"release-work","parent_id":"parent","type":11}`,
		))}, nil
	})}
	threadID, name, parentID := discordThreadContext(sess, "thread")
	if threadID != "thread" || name != "release-work" || parentID != "parent" {
		t.Fatalf("thread context = %q, %q, %q", threadID, name, parentID)
	}
}

func TestBoundedDiscordContextTruncatesContent(t *testing.T) {
	got := boundedDiscordContext(&discordgo.Message{Content: strings.Repeat("😀", 300)})
	if utf16Units(got.Content) > 501 || !strings.HasSuffix(got.Content, "…") {
		t.Fatalf("bounded content has %d units and suffix %q", utf16Units(got.Content), got.Content[len(got.Content)-3:])
	}
}

func TestTruthy(t *testing.T) {
	for _, s := range []string{"1", "true", "TRUE", " yes ", "on"} {
		if !truthy(s) {
			t.Errorf("truthy(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"", "0", "false", "no", "maybe"} {
		if truthy(s) {
			t.Errorf("truthy(%q) = true, want false", s)
		}
	}
}

func TestStripBotMentions(t *testing.T) {
	botRole := map[string]bool{"barfrole": true}
	cases := []struct {
		in, botID string
		roles     map[string]bool
		want      string
	}{
		{"<@barfbot> what's up?", "barfbot", nil, "what's up?"},
		{"<@!barfbot> what's up?", "barfbot", nil, "what's up?"},
		{"hey <@barfbot>", "barfbot", nil, "hey"},
		{"ask <@someoneelse> about it", "barfbot", nil, "ask <@someoneelse> about it"},
		{"no mention", "barfbot", nil, "no mention"},
		{"<@barfbot> tell <@steve> hi", "barfbot", nil, "tell <@steve> hi"},
		{"unchanged when identity unknown", "", nil, "unchanged when identity unknown"},
		// A bare ping must not become an empty prompt.
		{"<@barfbot>", "barfbot", nil, "<@barfbot>"},
		{"  <@barfbot>  ", "barfbot", nil, "  <@barfbot>  "},
		// The bot's own managed role reads as "@Barf" too, so it is plumbing.
		{"<@&barfrole> what repos do you have?", "barfbot", botRole, "what repos do you have?"},
		{"hey <@&barfrole>", "barfbot", botRole, "hey"},
		{"<@&barfrole>", "barfbot", botRole, "<@&barfrole>"},
		// Someone else's role is content, not plumbing.
		{"ping <@&adminrole> about it", "barfbot", botRole, "ping <@&adminrole> about it"},
		// Both forms in one message.
		{"<@barfbot> and <@&barfrole> status?", "barfbot", botRole, "and  status?"},
	}
	for _, tc := range cases {
		if got := stripBotMentions(tc.in, tc.botID, tc.roles); got != tc.want {
			t.Errorf("stripBotMentions(%q, %q, %v) = %q, want %q", tc.in, tc.botID, tc.roles, got, tc.want)
		}
	}
}

// mention-only is the shared-channel mode: the allowlist still says WHO may
// drive the agent, this says WHICH of their messages count.
func TestHandleMessage_MentionOnly(t *testing.T) {
	var delivered int
	var lastContent string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		delivered++
		body, _ := io.ReadAll(r.Body)
		var env inboundEnvelope
		_ = json.Unmarshal(body, &env)
		lastContent = env.Content
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// The bot holds one role in g1 — Discord's auto-created managed role, the
	// one an author gets when they pick "@Barf" from role autocomplete.
	var roleLookups int
	newCfg := func(mentionOnly bool) *config {
		return &config{
			allowedUsers:   map[string]bool{"matt": true},
			inboundURL:     srv.URL,
			agentName:      "barf",
			bindingName:    "discord",
			hmacSecret:     []byte("s3cr3t"),
			sigHeader:      "X-Kyber-Signature-256",
			sigPrefix:      "sha256=",
			requestTimeout: 5 * time.Second,
			mentionOnly:    mentionOnly,
			botRoles: func(guildID, botID string) map[string]bool {
				roleLookups++
				if guildID == "g1" && botID == "barfbot" {
					return map[string]bool{"barfrole": true}
				}
				return nil
			},
		}
	}
	sess := &discordgo.Session{State: discordgo.NewState()}
	sess.State.User = &discordgo.User{ID: "barfbot"}

	withMention := makeMsg("matt", "chan1", "g1", "<@barfbot> status?", false)
	withMention.Mentions = []*discordgo.User{{ID: "barfbot"}}

	mentionsSomeoneElse := makeMsg("matt", "chan1", "g1", "<@steve> you around?", false)
	mentionsSomeoneElse.Mentions = []*discordgo.User{{ID: "steve"}}

	everyonePing := makeMsg("matt", "chan1", "g1", "@everyone standup", false)
	everyonePing.MentionEveryone = true

	replyToBot := makeMsg("matt", "chan1", "g1", "thanks", false)
	replyToBot.ReferencedMessage = &discordgo.Message{Author: &discordgo.User{ID: "barfbot"}}

	replyToHuman := makeMsg("matt", "chan1", "g1", "agreed", false)
	replyToHuman.ReferencedMessage = &discordgo.Message{Author: &discordgo.User{ID: "steve"}}

	// Tagging the bot's own managed role: identical to "@Barf" on screen, and
	// the form Discord's autocomplete offers alongside the user. Dropping these
	// is what made the agent look like it was answering selectively.
	roleMention := makeMsg("matt", "chan1", "g1", "<@&barfrole> what repos do you have?", false)
	roleMention.MentionRoles = []string{"barfrole"}

	otherRoleMention := makeMsg("matt", "chan1", "g1", "<@&adminrole> can you help?", false)
	otherRoleMention.MentionRoles = []string{"adminrole"}

	// @everyone arrives as MentionEveryone; the @everyone role (ID == guild ID)
	// must never be treated as one of the bot's roles.
	everyoneRole := makeMsg("matt", "chan1", "g1", "@everyone standup", false)
	everyoneRole.MentionEveryone = true
	everyoneRole.MentionRoles = []string{"g1"}

	cases := []struct {
		name        string
		mentionOnly bool
		msg         *discordgo.MessageCreate
		wantDeliv   bool
	}{
		{"off: plain chatter still forwarded", false, makeMsg("matt", "chan1", "g1", "hi", false), true},
		{"on: plain chatter dropped", true, makeMsg("matt", "chan1", "g1", "hi", false), false},
		{"on: @bot forwarded", true, withMention, true},
		{"on: mention of someone else dropped", true, mentionsSomeoneElse, false},
		{"on: @everyone does not count", true, everyonePing, false},
		{"on: reply to the bot forwarded", true, replyToBot, true},
		{"on: reply to a human dropped", true, replyToHuman, false},
		{"on: the bot's own role forwarded", true, roleMention, true},
		{"on: someone else's role dropped", true, otherRoleMention, false},
		{"on: @everyone role does not count", true, everyoneRole, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			delivered = 0
			handleMessage(t.Context(), newCfg(tc.mentionOnly), srv.Client(), sess, tc.msg)
			if (delivered == 1) != tc.wantDeliv {
				t.Errorf("delivered=%d, wantDelivered=%v", delivered, tc.wantDeliv)
			}
		})
	}

	// The agent should see the prompt, not the plumbing.
	delivered, lastContent = 0, ""
	handleMessage(t.Context(), newCfg(true), srv.Client(), sess, withMention)
	if lastContent != "status?" {
		t.Errorf("forwarded content = %q, want %q (bot mention stripped)", lastContent, "status?")
	}

	// Same for the role form — the agent shouldn't have to parse <@&…> either.
	delivered, lastContent = 0, ""
	handleMessage(t.Context(), newCfg(true), srv.Client(), sess, roleMention)
	if lastContent != "what repos do you have?" {
		t.Errorf("forwarded content = %q, want %q (bot role mention stripped)",
			lastContent, "what repos do you have?")
	}

	// Resolving the bot's roles costs a Discord API call, so it must not happen
	// for the overwhelmingly common message that mentions no role at all.
	roleLookups = 0
	handleMessage(t.Context(), newCfg(true), srv.Client(), sess, withMention)
	handleMessage(t.Context(), newCfg(true), srv.Client(), sess, makeMsg("matt", "chan1", "g1", "hi", false))
	if roleLookups != 0 {
		t.Errorf("role lookups on messages with no role mention = %d, want 0", roleLookups)
	}
}

// A message that arrives before the Gateway READY handshake populates the
// bot's identity must not be silently swallowed by mention-only.
func TestHandleMessage_MentionOnlyFailsOpenWithoutIdentity(t *testing.T) {
	var delivered int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		delivered++
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	cfg := &config{
		allowedUsers:   map[string]bool{"matt": true},
		inboundURL:     srv.URL,
		agentName:      "barf",
		bindingName:    "discord",
		hmacSecret:     []byte("s"),
		sigHeader:      "X-Kyber-Signature-256",
		requestTimeout: 5 * time.Second,
		mentionOnly:    true,
	}
	sess := &discordgo.Session{State: discordgo.NewState()} // no State.User yet
	handleMessage(t.Context(), cfg, srv.Client(), sess, makeMsg("matt", "chan1", "g1", "hi", false))
	if delivered != 1 {
		t.Errorf("unknown bot identity should fail open; delivered=%d, want 1", delivered)
	}
}

func TestHandleMessage_EmptyAllowlistDeniesAll(t *testing.T) {
	var delivered int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		delivered++
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	cfg := &config{
		allowedUsers:   map[string]bool{}, // empty = deny-all
		inboundURL:     srv.URL,
		agentName:      "barf",
		bindingName:    "discord",
		hmacSecret:     []byte("s"),
		sigHeader:      "X-Kyber-Signature-256",
		requestTimeout: 5 * time.Second,
	}
	sess := &discordgo.Session{State: discordgo.NewState()}
	sess.State.User = &discordgo.User{ID: "barfbot"}
	handleMessage(t.Context(), cfg, srv.Client(), sess, makeMsg("matt", "chan1", "g1", "hi", false))
	if delivered != 0 {
		t.Errorf("empty allowlist should deny all; delivered=%d", delivered)
	}
}

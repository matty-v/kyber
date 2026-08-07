package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// kyber#684 turned this binary into a real MCP server. Before that it was "mcp"
// in name only — /healthz and a bare /send — so a Claude Code agent moved onto
// it would have lost the tool interface its plugin provided and had to curl.
// These tests pin the tool surface and, more importantly, the guards on it.

// fakeBotAPI stands in for api.telegram.org, recording the last method and form
// it received so a test can assert on what we actually sent.
type fakeBotAPI struct {
	srv        *httptest.Server
	lastMethod string
	lastForm   url.Values
	status     int
	body       string
}

func newFakeBotAPI(t *testing.T) *fakeBotAPI {
	t.Helper()
	f := &fakeBotAPI{status: http.StatusOK, body: `{"ok":true,"result":{"message_id":123,"chat":{"id":99}}}`}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		f.lastMethod = parts[len(parts)-1]
		_ = r.ParseForm()
		f.lastForm = r.Form
		w.WriteHeader(f.status)
		_, _ = w.Write([]byte(f.body))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func newTestMCP(t *testing.T, bot *fakeBotAPI, chats ...string) *mcpServer {
	t.Helper()
	cfg := &config{
		botToken: "TOK", botAPIBaseURL: bot.srv.URL,
		allowedUsers: map[int64]bool{42: true},
		chats:        newChatSet(map[int64]bool{42: true}),
	}
	for _, c := range chats {
		cfg.chats.add(c)
	}
	cfg.callbacks = newCallbackRegistry()
	return &mcpServer{cfg: cfg, client: bot.srv.Client(), downloadDir: t.TempDir()}
}

func TestMCPReplyUsesOpaqueInlineButtonCallback(t *testing.T) {
	bot := newFakeBotAPI(t)
	s := newTestMCP(t, bot, "99")
	result := callTool(t, s, "reply", map[string]any{"chat_id": "99", "text": "Choose", "buttons": []any{
		map[string]any{"text": "Deploy", "value": "production-secret-value"},
	}})
	if result.IsError {
		t.Fatalf("reply failed: %s", result.text())
	}
	markup := bot.lastForm.Get("reply_markup")
	if markup == "" || strings.Contains(markup, "production-secret-value") {
		t.Fatalf("reply markup did not contain an opaque callback: %q", markup)
	}
}

func TestMCPEditMessageCanClearAndReplaceButtons(t *testing.T) {
	bot := newFakeBotAPI(t)
	s := newTestMCP(t, bot, "99")
	callTool(t, s, "reply", map[string]any{"chat_id": "99", "text": "Choose", "buttons": []any{
		map[string]any{"text": "Old", "value": "old-value"},
	}})
	var oldMarkup struct {
		InlineKeyboard [][]struct {
			CallbackData string `json:"callback_data"`
		} `json:"inline_keyboard"`
	}
	if err := json.Unmarshal([]byte(bot.lastForm.Get("reply_markup")), &oldMarkup); err != nil {
		t.Fatal(err)
	}
	oldToken := oldMarkup.InlineKeyboard[0][0].CallbackData

	result := callTool(t, s, "edit_message", map[string]any{
		"chat_id": "99", "message_id": "123", "text": "Selected", "buttons": []any{},
	})
	if result.IsError {
		t.Fatalf("clear failed: %s", result.text())
	}
	if got := bot.lastForm.Get("reply_markup"); got != `{"inline_keyboard":[]}` {
		t.Fatalf("clear reply_markup=%q", got)
	}
	if _, ok := s.cfg.callbacks.consume(oldToken, "99"); ok {
		t.Fatal("cleared button callback remained active")
	}

	result = callTool(t, s, "edit_message", map[string]any{
		"chat_id": "99", "message_id": "123", "text": "Choose again", "buttons": []any{
			map[string]any{"text": "New", "value": "new-value"},
		},
	})
	if result.IsError || !strings.Contains(bot.lastForm.Get("reply_markup"), `"text":"New"`) {
		t.Fatalf("replace failed: result=%s markup=%q", result.text(), bot.lastForm.Get("reply_markup"))
	}

	result = callTool(t, s, "edit_message", map[string]any{
		"chat_id": "99", "message_id": "123", "text": "Text only",
	})
	if result.IsError {
		t.Fatalf("preserving edit failed: %s", result.text())
	}
	if _, present := bot.lastForm["reply_markup"]; present {
		t.Fatal("omitted buttons unexpectedly changed reply_markup")
	}
}

// rpc drives one JSON-RPC request through the handler and returns the response.
func rpc(t *testing.T, s *mcpServer, method string, params any) jsonRPCResponse {
	t.Helper()
	body := map[string]any{"jsonrpc": "2.0", "id": 1, "method": method}
	if params != nil {
		body["params"] = params
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	s.handle(rr, httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(raw)))
	var resp jsonRPCResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response %q: %v", rr.Body.String(), err)
	}
	return resp
}

// callTool drives tools/call and returns the tool result.
func callTool(t *testing.T, s *mcpServer, name string, args map[string]any) toolResult {
	t.Helper()
	resp := rpc(t, s, "tools/call", map[string]any{"name": name, "arguments": args})
	if resp.Error != nil {
		t.Fatalf("tools/call returned an RPC error: %+v", resp.Error)
	}
	raw, err := json.Marshal(resp.Result)
	if err != nil {
		t.Fatal(err)
	}
	var out toolResult
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func (r toolResult) text() string {
	var b strings.Builder
	for _, c := range r.Content {
		b.WriteString(c.Text)
	}
	return b.String()
}

func TestMCP_InitializeAndToolsList(t *testing.T) {
	s := newTestMCP(t, newFakeBotAPI(t))

	init := rpc(t, s, "initialize", map[string]any{"protocolVersion": protocolVersion})
	if init.Error != nil {
		t.Fatalf("initialize failed: %+v", init.Error)
	}

	list := rpc(t, s, "tools/list", nil)
	raw, _ := json.Marshal(list.Result)
	var out struct {
		Tools []toolDef `json:"tools"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	// The parity contract: these four names are what the Claude Code plugin
	// gave agents, so migrated identity instructions keep meaning the same.
	want := map[string]bool{"reply": false, "react": false, "edit_message": false, "download_attachment": false}
	for _, tool := range out.Tools {
		if _, expected := want[tool.Name]; expected {
			want[tool.Name] = true
		}
		if tool.InputSchema == nil {
			t.Errorf("tool %q has no inputSchema — clients cannot call it", tool.Name)
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("tool %q missing — an agent migrated off the plugin loses this capability", name)
		}
	}
}

// A notification (no id) must produce no body. Returning one makes strict
// clients treat the response as unsolicited.
func TestMCP_NotificationGetsNoBody(t *testing.T) {
	s := newTestMCP(t, newFakeBotAPI(t))
	rr := httptest.NewRecorder()
	s.handle(rr, httptest.NewRequest(http.MethodPost, "/mcp",
		strings.NewReader(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)))
	if body := strings.TrimSpace(rr.Body.String()); body != "" {
		t.Errorf("notification produced a response body: %q", body)
	}
}

func TestMCP_ReplySendsMessage(t *testing.T) {
	bot := newFakeBotAPI(t)
	s := newTestMCP(t, bot)

	res := callTool(t, s, "reply", map[string]any{"chat_id": "42", "text": "hello"})
	if res.IsError {
		t.Fatalf("reply reported an error: %s", res.text())
	}
	if bot.lastMethod != "sendMessage" {
		t.Errorf("called %q, want sendMessage", bot.lastMethod)
	}
	if got := bot.lastForm.Get("text"); got != "hello" {
		t.Errorf("text = %q", got)
	}
	if got := bot.lastForm.Get("parse_mode"); got != "" {
		t.Errorf("parse_mode should be unset for plain text, got %q", got)
	}
}

func TestMCP_ReplyStopsTypingLease(t *testing.T) {
	bot := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/sendMessage") {
			_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":123,"chat":{"id":42}}}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
	}))
	defer bot.Close()
	cfg := &config{botToken: "TOK", botAPIBaseURL: bot.URL, chats: newChatSet(map[int64]bool{42: true})}
	s := &mcpServer{cfg: cfg, client: bot.Client(), downloadDir: t.TempDir()}
	s.cfg.actions = newChatActionManager(context.Background(), s.cfg, bot.Client())
	t.Cleanup(s.cfg.actions.close)
	s.cfg.actions.start("42")

	res := callTool(t, s, "reply", map[string]any{"chat_id": "42", "text": "done"})
	if res.IsError {
		t.Fatalf("reply reported an error: %s", res.text())
	}
	if s.cfg.actions.active("42") {
		t.Fatal("reply left the typing lease active")
	}
}

func TestMCP_ReplyMarkdownAndThreading(t *testing.T) {
	bot := newFakeBotAPI(t)
	s := newTestMCP(t, bot)

	callTool(t, s, "reply", map[string]any{
		"chat_id": "42", "text": "*bold*", "format": "markdownv2", "reply_to": "7",
	})
	if got := bot.lastForm.Get("parse_mode"); got != "MarkdownV2" {
		t.Errorf("parse_mode = %q, want MarkdownV2", got)
	}
	if got := bot.lastForm.Get("reply_to_message_id"); got != "7" {
		t.Errorf("reply_to_message_id = %q, want 7", got)
	}
	// Threading must not become a delivery risk: if the quoted message was
	// deleted, the reply should still land.
	if got := bot.lastForm.Get("allow_sending_without_reply"); got != "true" {
		t.Errorf("allow_sending_without_reply = %q — a deleted parent would drop the whole reply", got)
	}
}

// A model may send numeric ids as JSON numbers. Failing on that is a pointless
// type technicality that would surface as a broken agent.
func TestMCP_AcceptsNumericIDs(t *testing.T) {
	bot := newFakeBotAPI(t)
	s := newTestMCP(t, bot)

	res := callTool(t, s, "reply", map[string]any{"chat_id": float64(42), "text": "hi"})
	if res.IsError {
		t.Fatalf("numeric chat_id rejected: %s", res.text())
	}
	if got := bot.lastForm.Get("chat_id"); got != "42" {
		t.Errorf("chat_id = %q, want 42", got)
	}
}

// The outbound scope guard: an agent must not be able to message arbitrary
// chats, which would be a free primitive for a prompt-injected model.
func TestMCP_RefusesOutOfScopeChat(t *testing.T) {
	bot := newFakeBotAPI(t)
	s := newTestMCP(t, bot)

	for _, tool := range []string{"reply", "react", "edit_message"} {
		args := map[string]any{"chat_id": "999999", "text": "x", "message_id": "1", "emoji": "👍"}
		res := callTool(t, s, tool, args)
		if !res.IsError {
			t.Errorf("%s accepted an out-of-scope chat — a prompt-injected agent could message anyone", tool)
		}
		if bot.lastMethod != "" {
			t.Errorf("%s reached Telegram despite being out of scope (method=%s)", tool, bot.lastMethod)
		}
	}
}

func TestMCP_ReactAndEdit(t *testing.T) {
	bot := newFakeBotAPI(t)
	s := newTestMCP(t, bot)

	if res := callTool(t, s, "react", map[string]any{"chat_id": "42", "message_id": "7", "emoji": "👍"}); res.IsError {
		t.Fatalf("react failed: %s", res.text())
	}
	if bot.lastMethod != "setMessageReaction" {
		t.Errorf("called %q, want setMessageReaction", bot.lastMethod)
	}
	var reactions []map[string]string
	if err := json.Unmarshal([]byte(bot.lastForm.Get("reaction")), &reactions); err != nil {
		t.Fatalf("reaction payload is not valid JSON: %v", err)
	}
	if len(reactions) != 1 || reactions[0]["emoji"] != "👍" {
		t.Errorf("reaction payload = %v", reactions)
	}

	if res := callTool(t, s, "edit_message", map[string]any{"chat_id": "42", "message_id": "7", "text": "fixed"}); res.IsError {
		t.Fatalf("edit failed: %s", res.text())
	}
	if bot.lastMethod != "editMessageText" {
		t.Errorf("called %q, want editMessageText", bot.lastMethod)
	}
}

// An empty emoji clears the reaction rather than sending a malformed one.
func TestMCP_ReactWithEmptyEmojiClears(t *testing.T) {
	bot := newFakeBotAPI(t)
	s := newTestMCP(t, bot)

	callTool(t, s, "react", map[string]any{"chat_id": "42", "message_id": "7", "emoji": ""})
	if got := bot.lastForm.Get("reaction"); got != "[]" {
		t.Errorf("reaction = %q, want [] to clear", got)
	}
}

// A Telegram rejection must reach the MODEL as a readable tool error, not an
// opaque RPC failure — otherwise the agent cannot tell a bad parse_mode from a
// missing message and just retries the same broken send.
func TestMCP_TelegramErrorSurfacesToModel(t *testing.T) {
	bot := newFakeBotAPI(t)
	bot.status = http.StatusBadRequest
	bot.body = `{"ok":false,"description":"Bad Request: can't parse entities"}`
	s := newTestMCP(t, bot)

	res := callTool(t, s, "reply", map[string]any{"chat_id": "42", "text": "*oops"})
	if !res.IsError {
		t.Fatal("a rejected send reported success")
	}
	if !strings.Contains(res.text(), "parse entities") {
		t.Errorf("error text does not carry Telegram's reason: %q", res.text())
	}
}

// The bot token must never reach a tool result — those go into the model's
// context and from there into transcripts.
func TestMCP_ErrorsDoNotLeakBotToken(t *testing.T) {
	bot := newFakeBotAPI(t)
	bot.srv.Close() // force a transport error, whose *url.Error embeds the full URL
	s := newTestMCP(t, bot)

	res := callTool(t, s, "reply", map[string]any{"chat_id": "42", "text": "hi"})
	if !res.IsError {
		t.Fatal("expected a transport failure")
	}
	if strings.Contains(res.text(), "TOK") {
		t.Errorf("tool error leaked the bot token: %q", res.text())
	}
}

func TestMCP_DownloadAttachment(t *testing.T) {
	const payload = "file-bytes"
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/getFile") {
			_, _ = w.Write([]byte(`{"ok":true,"result":{"file_path":"photos/img.jpg","file_size":10}}`))
			return
		}
		_, _ = w.Write([]byte(payload))
	}))
	defer srv.Close()

	dir := t.TempDir()
	files := newFileSet()
	files.add("abc")
	s := &mcpServer{
		cfg:    &config{botToken: "TOK", botAPIBaseURL: srv.URL, chats: newChatSet(nil), files: files},
		client: srv.Client(), downloadDir: dir,
	}

	res := callTool(t, s, "download_attachment", map[string]any{"file_id": "abc"})
	if res.IsError {
		t.Fatalf("download failed: %s", res.text())
	}
	got, err := os.ReadFile(filepath.Join(dir, "img.jpg"))
	if err != nil {
		t.Fatalf("reading downloaded file: %v", err)
	}
	if string(got) != payload {
		t.Errorf("downloaded %q, want %q", got, payload)
	}
	if !strings.Contains(res.text(), dir) {
		t.Errorf("result does not tell the agent where the file is: %q", res.text())
	}
	_ = context.Background()
}

// Telegram's file_path is server-controlled. A crafted traversal must not let a
// download escape the directory the agent was told about.
func TestMCP_DownloadRejectsPathTraversal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/getFile") {
			_, _ = w.Write([]byte(`{"ok":true,"result":{"file_path":"../../../../etc/pwned","file_size":4}}`))
			return
		}
		_, _ = w.Write([]byte("evil"))
	}))
	defer srv.Close()

	dir := t.TempDir()
	files := newFileSet()
	files.add("abc")
	s := &mcpServer{
		cfg:    &config{botToken: "TOK", botAPIBaseURL: srv.URL, chats: newChatSet(nil), files: files},
		client: srv.Client(), downloadDir: dir,
	}

	res := callTool(t, s, "download_attachment", map[string]any{"file_id": "abc"})
	if res.IsError {
		t.Fatalf("download failed: %s", res.text())
	}
	// The write must have landed inside dir, flattened to a base name.
	if _, err := os.Stat(filepath.Join(dir, "pwned")); err != nil {
		t.Errorf("expected the traversal to be flattened into %s: %v", dir, err)
	}
	if !strings.HasPrefix(strings.TrimPrefix(res.text(), "downloaded to "), dir) {
		t.Errorf("download escaped the destination directory: %q", res.text())
	}
}

func TestMCP_DownloadRefusesUnobservedFileID(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	defer srv.Close()
	s := &mcpServer{
		cfg:    &config{botToken: "TOK", botAPIBaseURL: srv.URL, chats: newChatSet(nil), files: newFileSet()},
		client: srv.Client(), downloadDir: t.TempDir(),
	}

	res := callTool(t, s, "download_attachment", map[string]any{"file_id": "not-observed"})
	if !res.IsError || !strings.Contains(res.text(), "not in scope") {
		t.Fatalf("unobserved file ID result = %+v", res)
	}
	if called {
		t.Fatal("unobserved file ID reached Telegram")
	}
}

func TestMCP_UnknownToolIsAnErrorNotACrash(t *testing.T) {
	s := newTestMCP(t, newFakeBotAPI(t))
	res := callTool(t, s, "definitely_not_a_tool", map[string]any{})
	if !res.IsError {
		t.Error("unknown tool reported success")
	}
}

func TestMCP_RejectsNonPOST(t *testing.T) {
	s := newTestMCP(t, newFakeBotAPI(t))
	rr := httptest.NewRecorder()
	s.handle(rr, httptest.NewRequest(http.MethodGet, "/mcp", nil))
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /mcp returned %d, want 405 — we never push server-initiated messages", rr.Code)
	}
}

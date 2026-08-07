package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// Bot API surface used by the MCP tools. Kept in one file so the set of
// Telegram capabilities the sidecar exposes is auditable in a single place —
// every one of these is reachable by the agent, so widening this file widens
// what a prompt-injected agent can do.

// maxDownloadBytes caps what getFile will pull into the pod. Telegram allows
// bot downloads up to 20 MB; we hold the whole body in memory to write it
// atomically, and an agent pod's memory limit is measured in hundreds of MB.
const maxDownloadBytes = 20 << 20

// maxUploadBytes is deliberately explicit even though Telegram's limits vary
// by method and Bot API version. Rejecting before upload is safer than the old
// behavior, which silently sent only the first 20 MB of a larger file.
const maxUploadBytes = 20 << 20

// apiResult is the envelope every Bot API method returns.
type apiResult struct {
	OK          bool            `json:"ok"`
	Description string          `json:"description"`
	Result      json.RawMessage `json:"result"`
}

// callAPI POSTs form values to a Bot API method and returns its `result`.
// Errors carry the Telegram description when there is one — a bare status code
// is nearly useless for diagnosing a rejected send (bad parse_mode, message not
// found, bot lacks reaction rights all look identical otherwise).
func callAPI(ctx context.Context, cfg *config, client *http.Client, method string, values url.Values) (json.RawMessage, error) {
	endpoint := fmt.Sprintf("%s/bot%s/%s", strings.TrimRight(cfg.botAPIBaseURL, "/"), cfg.botToken, method)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(values.Encode()))
	if err != nil {
		return nil, fmt.Errorf("new %s request: %w", method, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", method, scrubToken(err, cfg.botToken))
	}
	defer resp.Body.Close()
	var out apiResult
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("%s returned %d with an undecodable body", method, resp.StatusCode)
	}
	if resp.StatusCode >= 300 || !out.OK {
		return nil, fmt.Errorf("%s returned %d: %s", method, resp.StatusCode, strings.TrimSpace(out.Description))
	}
	return out.Result, nil
}

// callAPIMultipart uploads a local file alongside form fields (sendPhoto,
// sendDocument). The body is bounded at 20 MB so one tool call cannot consume
// unbounded sidecar memory.
func callAPIMultipart(ctx context.Context, cfg *config, client *http.Client, method, fileField, path string, values url.Values) (json.RawMessage, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("statting %s: %w", filepath.Base(path), err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", filepath.Base(path))
	}
	if info.Size() > maxUploadBytes {
		return nil, fmt.Errorf("%s is %d bytes, over the %d byte upload limit", filepath.Base(path), info.Size(), maxUploadBytes)
	}
	f, err := os.Open(path) //nolint:gosec // path is validated by the caller against the allowed roots
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", filepath.Base(path), err)
	}
	defer f.Close()

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for key, vals := range values {
		for _, v := range vals {
			if err := mw.WriteField(key, v); err != nil {
				return nil, fmt.Errorf("writing field %s: %w", key, err)
			}
		}
	}
	part, err := mw.CreateFormFile(fileField, filepath.Base(path))
	if err != nil {
		return nil, fmt.Errorf("creating file part: %w", err)
	}
	n, err := io.Copy(part, io.LimitReader(f, maxUploadBytes+1))
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", filepath.Base(path), err)
	}
	if n > maxUploadBytes {
		return nil, fmt.Errorf("%s grew beyond the %d byte upload limit while reading", filepath.Base(path), maxUploadBytes)
	}
	if err := mw.Close(); err != nil {
		return nil, fmt.Errorf("closing multipart writer: %w", err)
	}

	endpoint := fmt.Sprintf("%s/bot%s/%s", strings.TrimRight(cfg.botAPIBaseURL, "/"), cfg.botToken, method)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, &buf)
	if err != nil {
		return nil, fmt.Errorf("new %s request: %w", method, err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", method, scrubToken(err, cfg.botToken))
	}
	defer resp.Body.Close()
	var out apiResult
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("%s returned %d with an undecodable body", method, resp.StatusCode)
	}
	if resp.StatusCode >= 300 || !out.OK {
		return nil, fmt.Errorf("%s returned %d: %s", method, resp.StatusCode, strings.TrimSpace(out.Description))
	}
	return out.Result, nil
}

func sendMediaGroup(ctx context.Context, cfg *config, client *http.Client, chatID string, paths []string) error {
	if len(paths) < 2 || len(paths) > 10 {
		return fmt.Errorf("media groups require 2 to 10 files")
	}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	media := make([]map[string]string, 0, len(paths))
	var totalSize int64
	for i, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("statting %s: %w", filepath.Base(path), err)
		}
		if !info.Mode().IsRegular() || info.Size() > maxUploadBytes {
			return fmt.Errorf("%s is not an uploadable regular file", filepath.Base(path))
		}
		totalSize += info.Size()
		if totalSize > maxUploadBytes {
			return fmt.Errorf("media group is over the %d byte upload limit", maxUploadBytes)
		}
		mediaType := nativeAlbumMediaType(path)
		if mediaType == "" {
			return fmt.Errorf("%s cannot be sent in a native media group", filepath.Base(path))
		}
		field := fmt.Sprintf("file%d", i)
		part, err := mw.CreateFormFile(field, filepath.Base(path))
		if err != nil {
			return fmt.Errorf("creating media part: %w", err)
		}
		f, err := os.Open(path) //nolint:gosec // validated against upload roots by the caller
		if err != nil {
			return fmt.Errorf("opening %s: %w", filepath.Base(path), err)
		}
		remaining := maxUploadBytes - (totalSize - info.Size())
		copied, copyErr := io.Copy(part, io.LimitReader(f, remaining+1))
		closeErr := f.Close()
		if copyErr != nil {
			return fmt.Errorf("reading %s: %w", filepath.Base(path), copyErr)
		}
		if closeErr != nil {
			return fmt.Errorf("closing %s: %w", filepath.Base(path), closeErr)
		}
		if copied > remaining {
			return fmt.Errorf("media group grew beyond the %d byte upload limit while reading", maxUploadBytes)
		}
		media = append(media, map[string]string{"type": mediaType, "media": "attach://" + field})
	}
	rawMedia, err := json.Marshal(media)
	if err != nil {
		return fmt.Errorf("encoding media group: %w", err)
	}
	if err := mw.WriteField("chat_id", chatID); err != nil {
		return fmt.Errorf("writing chat id: %w", err)
	}
	if err := mw.WriteField("media", string(rawMedia)); err != nil {
		return fmt.Errorf("writing media group: %w", err)
	}
	if err := mw.Close(); err != nil {
		return fmt.Errorf("closing multipart writer: %w", err)
	}
	endpoint := fmt.Sprintf("%s/bot%s/sendMediaGroup", strings.TrimRight(cfg.botAPIBaseURL, "/"), cfg.botToken)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, &buf)
	if err != nil {
		return fmt.Errorf("new sendMediaGroup request: %w", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("sendMediaGroup: %w", scrubToken(err, cfg.botToken))
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var out apiResult
	if err := json.Unmarshal(body, &out); err != nil {
		return fmt.Errorf("sendMediaGroup returned %d with an undecodable body", resp.StatusCode)
	}
	if resp.StatusCode >= 300 || !out.OK {
		return fmt.Errorf("sendMediaGroup returned %d: %s", resp.StatusCode, strings.TrimSpace(out.Description))
	}
	return nil
}

func nativeAlbumMediaType(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".jpg", ".jpeg", ".png", ".webp":
		return "photo"
	case ".mp4", ".mov", ".webm":
		return "video"
	default:
		return ""
	}
}

func validateOutboundFile(path string, roots []string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("path must be absolute")
	}
	resolved, err := filepath.EvalSymlinks(filepath.Clean(path))
	if err != nil {
		return "", fmt.Errorf("resolving path: %w", err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("statting path: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("path is not a regular file")
	}
	if info.Size() > maxUploadBytes {
		return "", fmt.Errorf("file is %d bytes, over the %d byte upload limit", info.Size(), maxUploadBytes)
	}
	for _, root := range roots {
		resolvedRoot, err := filepath.EvalSymlinks(filepath.Clean(root))
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(resolvedRoot, resolved)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
			return resolved, nil
		}
	}
	return "", fmt.Errorf("path is outside the allowed upload roots")
}

// sentMessage is the subset of the returned Message the tools echo back, so an
// agent can thread a follow-up or edit what it just sent.
type sentMessage struct {
	MessageID int64 `json:"message_id"`
	Chat      struct {
		ID int64 `json:"id"`
	} `json:"chat"`
}

func sendMessage(ctx context.Context, cfg *config, client *http.Client, chatID, text, parseMode, replyTo, replyMarkup string) (*sentMessage, error) {
	values := url.Values{"chat_id": {chatID}, "text": {text}}
	if parseMode != "" {
		values.Set("parse_mode", parseMode)
	}
	if replyTo != "" {
		values.Set("reply_to_message_id", replyTo)
		// Without this, replying to a message that has since been deleted fails
		// the whole send. The reply context is a nicety; delivering the message
		// is the point.
		values.Set("allow_sending_without_reply", "true")
	}
	if replyMarkup != "" {
		values.Set("reply_markup", replyMarkup)
	}
	raw, err := callAPI(ctx, cfg, client, "sendMessage", values)
	if err != nil {
		return nil, err
	}
	var msg sentMessage
	_ = json.Unmarshal(raw, &msg)
	return &msg, nil
}

func answerCallbackQuery(ctx context.Context, cfg *config, client *http.Client, callbackID, text string) error {
	values := url.Values{"callback_query_id": {callbackID}}
	if text != "" {
		values.Set("text", text)
	}
	_, err := callAPI(ctx, cfg, client, "answerCallbackQuery", values)
	return err
}

func editMessageText(ctx context.Context, cfg *config, client *http.Client, chatID, messageID, text, parseMode, replyMarkup string, setReplyMarkup bool) error {
	values := url.Values{"chat_id": {chatID}, "message_id": {messageID}, "text": {text}}
	if parseMode != "" {
		values.Set("parse_mode", parseMode)
	}
	if setReplyMarkup {
		values.Set("reply_markup", replyMarkup)
	}
	_, err := callAPI(ctx, cfg, client, "editMessageText", values)
	return err
}

func sendChatAction(ctx context.Context, cfg *config, client *http.Client, chatID, action string) error {
	_, err := callAPI(ctx, cfg, client, "sendChatAction", url.Values{"chat_id": {chatID}, "action": {action}})
	return err
}

// setMessageReaction sets (or with an empty emoji, clears) the bot's reaction.
// Telegram replaces the bot's previous reaction rather than accumulating.
func setMessageReaction(ctx context.Context, cfg *config, client *http.Client, chatID, messageID, emoji string) error {
	values := url.Values{"chat_id": {chatID}, "message_id": {messageID}}
	if emoji == "" {
		values.Set("reaction", "[]")
	} else {
		reaction, err := json.Marshal([]map[string]string{{"type": "emoji", "emoji": emoji}})
		if err != nil {
			return fmt.Errorf("encoding reaction: %w", err)
		}
		values.Set("reaction", string(reaction))
	}
	_, err := callAPI(ctx, cfg, client, "setMessageReaction", values)
	return err
}

// sendFile picks the native Telegram media method by extension so clients get
// the expected player, waveform, gallery, or inline preview.
func sendFile(ctx context.Context, cfg *config, client *http.Client, chatID, path, caption, parseMode string) (*sentMessage, error) {
	values := url.Values{"chat_id": {chatID}}
	if caption != "" {
		values.Set("caption", caption)
		if parseMode != "" {
			values.Set("parse_mode", parseMode)
		}
	}
	method, field := "sendDocument", "document"
	switch strings.ToLower(filepath.Ext(path)) {
	case ".jpg", ".jpeg", ".png", ".webp":
		method, field = "sendPhoto", "photo"
	case ".gif":
		method, field = "sendAnimation", "animation"
	case ".mp4", ".mov", ".webm":
		method, field = "sendVideo", "video"
	case ".mp3", ".m4a", ".flac", ".wav":
		method, field = "sendAudio", "audio"
	case ".ogg", ".oga", ".opus":
		method, field = "sendVoice", "voice"
	}
	raw, err := callAPIMultipart(ctx, cfg, client, method, field, path, values)
	if err != nil {
		return nil, err
	}
	var msg sentMessage
	_ = json.Unmarshal(raw, &msg)
	return &msg, nil
}

// downloadFile resolves a file_id to a temporary path inside the pod.
//
// Two calls: getFile maps the id to a short-lived server path, then a plain GET
// on the /file/ endpoint fetches the bytes. The token is in that URL too, so
// failures here go through scrubToken like everything else.
func downloadFile(ctx context.Context, cfg *config, client *http.Client, fileID, destDir string) (string, error) {
	raw, err := callAPI(ctx, cfg, client, "getFile", url.Values{"file_id": {fileID}})
	if err != nil {
		return "", err
	}
	var meta struct {
		FilePath string `json:"file_path"`
		FileSize int64  `json:"file_size"`
	}
	if err := json.Unmarshal(raw, &meta); err != nil {
		return "", fmt.Errorf("decoding getFile result: %w", err)
	}
	if meta.FilePath == "" {
		return "", fmt.Errorf("getFile returned no file_path for %s", fileID)
	}
	if meta.FileSize > maxDownloadBytes {
		return "", fmt.Errorf("file is %d bytes, over the %d byte limit", meta.FileSize, maxDownloadBytes)
	}

	// Telegram's file_path is server-controlled. Join it as a base name only —
	// a crafted "../.." must not let a download escape destDir.
	name := filepath.Base(meta.FilePath)
	if name == "." || name == string(os.PathSeparator) {
		name = fileID
	}
	dest := filepath.Join(destDir, name)

	fileURL := fmt.Sprintf("%s/file/bot%s/%s", strings.TrimRight(cfg.botAPIBaseURL, "/"), cfg.botToken, meta.FilePath)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fileURL, nil)
	if err != nil {
		return "", fmt.Errorf("new file request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("downloading attachment: %w", scrubToken(err, cfg.botToken))
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("downloading attachment returned %d", resp.StatusCode)
	}
	// 0755/0644, not 0700/0600: this sidecar writes as root and the AGENT — a
	// different container, different uid — is the reader. The tool hands the
	// model this path and the model reads it, so a mode only the writer can use
	// means every downloaded attachment is invisible to the one process that
	// wanted it. The directory lives on the agent's own private PVC, which no
	// other pod mounts, so the pod boundary is still the trust boundary.
	if err := os.MkdirAll(destDir, 0o755); err != nil { //nolint:gosec // cross-container read is the point; see above
		return "", fmt.Errorf("creating %s: %w", destDir, err)
	}
	f, err := os.CreateTemp(destDir, ".telegram-download-*")
	if err != nil {
		return "", fmt.Errorf("creating temporary attachment: %w", err)
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()
	if err := f.Chmod(0o644); err != nil { //nolint:gosec // cross-container read is required
		_ = f.Close()
		return "", fmt.Errorf("setting attachment permissions: %w", err)
	}
	written, err := io.Copy(f, io.LimitReader(resp.Body, maxDownloadBytes+1))
	if err != nil {
		_ = f.Close()
		return "", fmt.Errorf("writing %s: %w", dest, err)
	}
	if written > maxDownloadBytes {
		_ = f.Close()
		return "", fmt.Errorf("download exceeded the %d byte limit", maxDownloadBytes)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("closing %s: %w", dest, err)
	}
	// Rename publishes the complete file atomically and replaces, rather than
	// follows, an agent-created symlink at the destination.
	if err := os.Rename(tmp, dest); err != nil {
		return "", fmt.Errorf("publishing %s: %w", dest, err)
	}
	return dest, nil
}

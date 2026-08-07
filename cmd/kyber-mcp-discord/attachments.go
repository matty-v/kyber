package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
)

const maxDiscordAttachmentBytes int64 = 10 << 20

func isAllowedDiscordCDNURL(raw string) bool {
	parsed, err := url.Parse(raw)
	return err == nil && parsed.Scheme == "https" &&
		(parsed.Hostname() == "cdn.discordapp.com" || parsed.Hostname() == "media.discordapp.net")
}

func newDiscordAttachmentClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		CheckRedirect: func(req *http.Request, _ []*http.Request) error {
			if !isAllowedDiscordCDNURL(req.URL.String()) {
				return fmt.Errorf("attachment redirect is not an allowed Discord CDN URL")
			}
			return nil
		},
	}
}

type inboundAttachment struct {
	ID          string `json:"id"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type,omitempty"`
	Size        int    `json:"size"`
	Width       int    `json:"width,omitempty"`
	Height      int    `json:"height,omitempty"`
}

func inboundAttachments(items []*discordgo.MessageAttachment) []inboundAttachment {
	out := make([]inboundAttachment, 0, len(items))
	for _, item := range items {
		if item == nil || item.ID == "" {
			continue
		}
		out = append(out, inboundAttachment{ID: item.ID, Filename: item.Filename, ContentType: item.ContentType,
			Size: item.Size, Width: item.Width, Height: item.Height})
	}
	return out
}

type observedAttachment struct {
	ID       string
	URL      string
	Filename string
	Size     int
}

type attachmentStore struct {
	mu    sync.Mutex
	max   int
	order []string
	items map[string]observedAttachment
}

func newAttachmentStore(max int) *attachmentStore {
	return &attachmentStore{max: max, items: map[string]observedAttachment{}}
}

func (s *attachmentStore) observe(items []*discordgo.MessageAttachment) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, item := range items {
		if item == nil || item.ID == "" || item.URL == "" {
			continue
		}
		if _, exists := s.items[item.ID]; !exists {
			s.order = append(s.order, item.ID)
		}
		s.items[item.ID] = observedAttachment{ID: item.ID, URL: item.URL, Filename: item.Filename, Size: item.Size}
	}
	for len(s.order) > s.max {
		delete(s.items, s.order[0])
		s.order = s.order[1:]
	}
}

func (s *attachmentStore) get(id string) (observedAttachment, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	item, ok := s.items[id]
	return item, ok
}

func downloadDiscordAttachment(ctx context.Context, client *http.Client, item observedAttachment, dir string) (string, error) {
	if !isAllowedDiscordCDNURL(item.URL) {
		return "", fmt.Errorf("attachment URL is not an allowed Discord CDN URL")
	}
	if item.Size > int(maxDiscordAttachmentBytes) {
		return "", fmt.Errorf("attachment is %d bytes, over the %d byte download limit", item.Size, maxDiscordAttachmentBytes)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("creating download directory: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, item.URL, nil)
	if err != nil {
		return "", fmt.Errorf("creating attachment request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("downloading attachment: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("downloading attachment: Discord CDN returned %d", resp.StatusCode)
	}
	name := filepath.Base(strings.TrimSpace(item.Filename))
	if name == "." || name == "" {
		name = item.ID
	}
	temp, err := os.CreateTemp(dir, ".discord-attachment-*")
	if err != nil {
		return "", fmt.Errorf("creating attachment file: %w", err)
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	written, copyErr := io.Copy(temp, io.LimitReader(resp.Body, maxDiscordAttachmentBytes+1))
	closeErr := temp.Close()
	if copyErr != nil {
		return "", fmt.Errorf("writing attachment: %w", copyErr)
	}
	if closeErr != nil {
		return "", fmt.Errorf("closing attachment: %w", closeErr)
	}
	if written > maxDiscordAttachmentBytes {
		return "", fmt.Errorf("attachment exceeds the %d byte download limit", maxDiscordAttachmentBytes)
	}
	destination := filepath.Join(dir, item.ID+"-"+name)
	if err := os.Chmod(tempName, 0o644); err != nil {
		return "", fmt.Errorf("setting attachment permissions: %w", err)
	}
	if err := os.Rename(tempName, destination); err != nil {
		return "", fmt.Errorf("saving attachment: %w", err)
	}
	return destination, nil
}

func validateDiscordOutboundFile(path string, roots []string) (string, error) {
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
	if info.Size() > maxDiscordAttachmentBytes {
		return "", fmt.Errorf("file is %d bytes, over the %d byte upload limit", info.Size(), maxDiscordAttachmentBytes)
	}
	for _, candidate := range roots {
		root, err := filepath.EvalSymlinks(filepath.Clean(candidate))
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(root, resolved)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
			return resolved, nil
		}
	}
	return "", fmt.Errorf("path is outside the allowed upload roots")
}

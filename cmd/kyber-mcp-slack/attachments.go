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
)

const maxSlackFileBytes int64 = 10 << 20

type slackFile struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	MIMEType string `json:"mimetype"`
	Size     int64  `json:"size"`
	URL      string `json:"url_private_download"`
}
type inboundSlackFile struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	MIMEType string `json:"mimetype,omitempty"`
	Size     int64  `json:"size,omitempty"`
}
type observedSlackFile struct {
	ID, Name, URL string
	Size          int64
}
type slackAttachmentStore struct {
	mu    sync.Mutex
	max   int
	order []string
	items map[string]observedSlackFile
}

func newSlackAttachmentStore(max int) *slackAttachmentStore {
	return &slackAttachmentStore{max: max, items: map[string]observedSlackFile{}}
}
func (s *slackAttachmentStore) observe(files []slackFile) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, f := range files {
		if f.ID == "" || f.URL == "" {
			continue
		}
		if _, ok := s.items[f.ID]; !ok {
			s.order = append(s.order, f.ID)
		}
		s.items[f.ID] = observedSlackFile{f.ID, f.Name, f.URL, f.Size}
	}
	for len(s.order) > s.max {
		delete(s.items, s.order[0])
		s.order = s.order[1:]
	}
}
func (s *slackAttachmentStore) get(id string) (observedSlackFile, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.items[id]
	return v, ok
}
func inboundSlackFiles(files []slackFile) []inboundSlackFile {
	out := make([]inboundSlackFile, 0, len(files))
	for _, file := range files {
		if file.ID == "" {
			continue
		}
		out = append(out, inboundSlackFile{ID: file.ID, Name: file.Name, MIMEType: file.MIMEType, Size: file.Size})
	}
	return out
}
func allowedSlackFileURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" {
		return false
	}
	return u.Hostname() == "files.slack.com" || u.Hostname() == "downloads.slack-edge.com"
}
func newSlackFileClient(token string) *http.Client {
	return &http.Client{Timeout: 60 * time.Second, CheckRedirect: func(req *http.Request, _ []*http.Request) error {
		if !allowedSlackFileURL(req.URL.String()) {
			return fmt.Errorf("file redirect is not an allowed Slack host")
		}
		req.Header.Set("Authorization", "Bearer "+token)
		return nil
	}}
}
func downloadSlackFile(ctx context.Context, client *http.Client, token string, item observedSlackFile, dir string) (string, error) {
	if !allowedSlackFileURL(item.URL) {
		return "", fmt.Errorf("file URL is not an allowed Slack host")
	}
	if item.Size > maxSlackFileBytes {
		return "", fmt.Errorf("file exceeds the %d byte limit", maxSlackFileBytes)
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", fmt.Errorf("creating download directory: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, "GET", item.URL, nil)
	if err != nil {
		return "", fmt.Errorf("creating file request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("downloading file: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		return "", fmt.Errorf("Slack file host returned %d", res.StatusCode)
	}
	tmp, err := os.CreateTemp(dir, ".slack-file-*")
	if err != nil {
		return "", fmt.Errorf("creating file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	n, copyErr := io.Copy(tmp, io.LimitReader(res.Body, maxSlackFileBytes+1))
	closeErr := tmp.Close()
	if copyErr != nil {
		return "", fmt.Errorf("writing file: %w", copyErr)
	}
	if closeErr != nil {
		return "", fmt.Errorf("closing file: %w", closeErr)
	}
	if n > maxSlackFileBytes {
		return "", fmt.Errorf("file exceeds the %d byte limit", maxSlackFileBytes)
	}
	name := filepath.Base(strings.TrimSpace(item.Name))
	if name == "" || name == "." {
		name = item.ID
	}
	dest := filepath.Join(dir, item.ID+"-"+name)
	if err := os.Rename(tmpName, dest); err != nil {
		return "", fmt.Errorf("saving file: %w", err)
	}
	return dest, nil
}

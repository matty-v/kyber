package main

import (
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const maxSlackFileBytes int64 = 10 << 20
const maxSlackUploadBytes int64 = 20 << 20

var slackPersistRoot = "/persist"

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
	fileID := filepath.Base(strings.TrimSpace(item.ID))
	if fileID == "" || fileID == "." || fileID != item.ID {
		return "", fmt.Errorf("file ID is invalid")
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", fmt.Errorf("creating download directory: %w", err)
	}
	resolvedDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", fmt.Errorf("resolving download directory: %w", err)
	}
	persist, err := filepath.EvalSymlinks(slackPersistRoot)
	if err != nil {
		return "", fmt.Errorf("resolving /persist: %w", err)
	}
	rel, err := filepath.Rel(persist, resolvedDir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("download directory is outside /persist")
	}
	dir = resolvedDir
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
		name = fileID
	}
	dest := filepath.Join(dir, fileID+"-"+name)
	if err := os.Rename(tmpName, dest); err != nil {
		return "", fmt.Errorf("saving file: %w", err)
	}
	return dest, nil
}

func openSlackOutboundFile(path string) (*os.File, os.FileInfo, error) {
	if !filepath.IsAbs(path) {
		return nil, nil, fmt.Errorf("path must be absolute")
	}
	rel, err := filepath.Rel(slackPersistRoot, filepath.Clean(path))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return nil, nil, fmt.Errorf("path is outside /persist")
	}
	// OpenInRoot performs the containment check and the open as one operation,
	// so a symlink swap cannot race a validate-then-open sequence.
	file, err := os.OpenInRoot(slackPersistRoot, rel)
	if err != nil {
		return nil, nil, fmt.Errorf("opening path beneath /persist: %w", err)
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, nil, fmt.Errorf("statting file: %w", err)
	}
	if !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, nil, fmt.Errorf("path is not a regular file")
	}
	if info.Size() > maxSlackFileBytes {
		_ = file.Close()
		return nil, nil, fmt.Errorf("file exceeds the %d byte limit", maxSlackFileBytes)
	}
	return file, info, nil
}

func uploadSlackFile(ctx context.Context, client *http.Client, uploadURL string, file *os.File, filename string) error {
	if !allowedSlackFileURL(uploadURL) {
		return fmt.Errorf("upload URL is not an allowed Slack host")
	}
	reader, writer := io.Pipe()
	multipartWriter := multipart.NewWriter(writer)
	go func() {
		part, partErr := multipartWriter.CreateFormFile("file", filename)
		if partErr == nil {
			_, partErr = io.Copy(part, file)
		}
		if closeErr := multipartWriter.Close(); partErr == nil {
			partErr = closeErr
		}
		_ = writer.CloseWithError(partErr)
	}()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, uploadURL, reader)
	if err != nil {
		return fmt.Errorf("creating upload request: %w", err)
	}
	req.Header.Set("Content-Type", multipartWriter.FormDataContentType())
	uploadClient := &http.Client{Transport: client.Transport, Timeout: client.Timeout, CheckRedirect: func(req *http.Request, _ []*http.Request) error {
		if !allowedSlackFileURL(req.URL.String()) {
			return fmt.Errorf("upload redirect is not an allowed Slack host")
		}
		return nil
	}}
	res, err := uploadClient.Do(req)
	if err != nil {
		return fmt.Errorf("uploading file: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		return fmt.Errorf("Slack upload host returned %d", res.StatusCode)
	}
	return nil
}

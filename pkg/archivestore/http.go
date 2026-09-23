package archivestore

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Server exposes a FilesystemStore to the control plane. It is reachable only
// on the cluster network and requires the shared bearer token on every
// request except /healthz. It never interprets what it stores; everything it
// holds is already sealed by the control plane.
type Server struct {
	Store *FilesystemStore
	Token string
}

const objectsPrefix = "/v1/objects/"

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/healthz" {
		w.WriteHeader(http.StatusOK)
		return
	}
	if !s.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.URL.Path == "/v1/capacity" && r.Method == http.MethodGet {
		c, err := s.Store.Capacity(r.Context())
		if err != nil {
			http.Error(w, "capacity unavailable", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(c)
		return
	}
	key, ok := strings.CutPrefix(r.URL.Path, objectsPrefix)
	if !ok || ValidateKey(key) != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	// Archives can be many gigabytes; the per-request deadline is set by the
	// caller's context, not a server-wide timeout.
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Time{})
	_ = rc.SetWriteDeadline(time.Time{})
	switch r.Method {
	case http.MethodPut:
		n, err := s.Store.Put(r.Context(), key, r.Body)
		if err != nil {
			slog.Warn("archive store: put failed", "key", key, "error", err)
			http.Error(w, "write failed", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]int64{"size": n})
	case http.MethodHead:
		size, err := s.Store.Size(r.Context(), key)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	case http.MethodGet:
		off, n, err := parseRange(r.Header.Get("Range"))
		if err != nil {
			http.Error(w, "bad range", http.StatusRequestedRangeNotSatisfiable)
			return
		}
		body, err := s.Store.Open(r.Context(), key, off, n)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		defer body.Close()
		w.Header().Set("Content-Type", "application/octet-stream")
		if r.Header.Get("Range") != "" {
			w.WriteHeader(http.StatusPartialContent)
		}
		_, _ = io.Copy(w, body)
	case http.MethodDelete:
		if err := s.Store.Delete(r.Context(), key); err != nil {
			http.Error(w, "delete failed", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) authorized(r *http.Request) bool {
	got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return ok && s.Token != "" && subtle.ConstantTimeCompare([]byte(got), []byte(s.Token)) == 1
}

func writeStoreError(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrNotFound) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	http.Error(w, "read failed", http.StatusInternalServerError)
}

// parseRange accepts the single "bytes=start-end" form the client sends.
func parseRange(h string) (int64, int64, error) {
	if h == "" {
		return 0, -1, nil
	}
	spec, ok := strings.CutPrefix(h, "bytes=")
	start, end, ok2 := strings.Cut(spec, "-")
	if !ok || !ok2 {
		return 0, 0, errors.New("unsupported range")
	}
	a, err := strconv.ParseInt(start, 10, 64)
	if err != nil || a < 0 {
		return 0, 0, errors.New("bad range start")
	}
	if end == "" {
		return a, -1, nil
	}
	b, err := strconv.ParseInt(end, 10, 64)
	if err != nil || b < a {
		return 0, 0, errors.New("bad range end")
	}
	return a, b - a + 1, nil
}

// HTTPStore is the control plane's client for the built-in archive store.
type HTTPStore struct {
	BaseURL string
	Token   string
	Client  *http.Client
}

func (s *HTTPStore) Name() string { return "kyber-archive-store" }

func (s *HTTPStore) client() *http.Client {
	if s.Client != nil {
		return s.Client
	}
	return http.DefaultClient
}

func (s *HTTPStore) do(ctx context.Context, method, path string, body io.Reader, hdr map[string]string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(s.BaseURL, "/")+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+s.Token)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := s.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("archive store %s: %w", method, err)
	}
	if resp.StatusCode == http.StatusNotFound {
		resp.Body.Close()
		return nil, ErrNotFound
	}
	if resp.StatusCode >= 300 {
		resp.Body.Close()
		return nil, fmt.Errorf("archive store %s: HTTP %d", method, resp.StatusCode)
	}
	return resp, nil
}

func (s *HTTPStore) Put(ctx context.Context, key string, body io.Reader) (int64, error) {
	if err := ValidateKey(key); err != nil {
		return 0, err
	}
	resp, err := s.do(ctx, http.MethodPut, objectsPrefix+key, io.NopCloser(body), nil)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	var out struct {
		Size int64 `json:"size"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&out); err != nil {
		return 0, fmt.Errorf("archive store put: %w", err)
	}
	return out.Size, nil
}

func (s *HTTPStore) Open(ctx context.Context, key string, offset, length int64) (io.ReadCloser, error) {
	if err := ValidateKey(key); err != nil {
		return nil, err
	}
	rng := fmt.Sprintf("bytes=%d-", offset)
	if length >= 0 {
		if length == 0 {
			return io.NopCloser(strings.NewReader("")), nil
		}
		rng = fmt.Sprintf("bytes=%d-%d", offset, offset+length-1)
	}
	resp, err := s.do(ctx, http.MethodGet, objectsPrefix+key, nil, map[string]string{"Range": rng})
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

func (s *HTTPStore) Size(ctx context.Context, key string) (int64, error) {
	if err := ValidateKey(key); err != nil {
		return 0, err
	}
	resp, err := s.do(ctx, http.MethodHead, objectsPrefix+key, nil, nil)
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	return resp.ContentLength, nil
}

func (s *HTTPStore) Delete(ctx context.Context, key string) error {
	if err := ValidateKey(key); err != nil {
		return err
	}
	resp, err := s.do(ctx, http.MethodDelete, objectsPrefix+key, nil, nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func (s *HTTPStore) Capacity(ctx context.Context) (Capacity, error) {
	resp, err := s.do(ctx, http.MethodGet, "/v1/capacity", nil, nil)
	if err != nil {
		return Capacity{}, err
	}
	defer resp.Body.Close()
	var c Capacity
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&c); err != nil {
		return Capacity{}, fmt.Errorf("archive store capacity: %w", err)
	}
	return c, nil
}

package handshake

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type receipt struct {
	TaskID    string `json:"taskId"`
	AttemptID string `json:"attemptId"`
	Runtime   string `json:"runtime"`
	SessionID string `json:"sessionId"`
	TurnID    string `json:"turnId,omitempty"`
}

func (r receipt) valid() bool {
	return strings.HasPrefix(r.TaskID, "task_") &&
		strings.HasPrefix(r.AttemptID, "attempt_") &&
		r.Runtime != "" && r.SessionID != ""
}

type store struct {
	mu       sync.Mutex
	receipts map[string]receipt
}

func newStore() *store { return &store{receipts: make(map[string]receipt)} }

func (s *store) put(r receipt) (receipt, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !r.valid() {
		return receipt{}, false, errors.New("invalid receipt")
	}
	prior, ok := s.receipts[r.AttemptID]
	if ok && prior != r {
		return prior, false, errors.New("attempt conflict")
	}
	if ok {
		return prior, false, nil
	}
	s.receipts[r.AttemptID] = r
	return r, true, nil
}

func (s *store) get(attemptID string) (receipt, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.receipts[attemptID]
	return r, ok
}

// controlPlane implements the proposed durable, idempotent internal API.
// POST is create-or-read by attempt ID. A replay is successful only when the
// complete immutable receipt matches. GET is the recovery path after an
// ambiguous POST result.
func controlPlane(s *store) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /internal/agents/{agent}/task-receipts", func(w http.ResponseWriter, r *http.Request) {
		var got receipt
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&got); err != nil {
			http.Error(w, "invalid body", http.StatusBadRequest)
			return
		}
		stored, created, err := s.put(got)
		if err != nil {
			if stored.AttemptID != "" {
				writeJSON(w, http.StatusConflict, stored)
				return
			}
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		status := http.StatusOK
		if created {
			status = http.StatusCreated
		}
		writeJSON(w, status, stored)
	})
	mux.HandleFunc("GET /internal/agents/{agent}/task-receipts/{attempt}", func(w http.ResponseWriter, r *http.Request) {
		got, ok := s.get(r.PathValue("attempt"))
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		writeJSON(w, http.StatusOK, got)
	})
	return mux
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

// sidecar preserves the existing trust boundary: the hook can reach only this
// loopback handler, while the sidecar owns the pod credential used upstream.
func sidecar(agent, upstream string, client *http.Client) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /task-receipts", func(w http.ResponseWriter, r *http.Request) {
		forward(w, r, client, upstream+"/internal/agents/"+agent+"/task-receipts")
	})
	mux.HandleFunc("GET /task-receipts/{attempt}", func(w http.ResponseWriter, r *http.Request) {
		forward(w, r, client, upstream+"/internal/agents/"+agent+"/task-receipts/"+r.PathValue("attempt"))
	})
	return mux
}

func forward(w http.ResponseWriter, inbound *http.Request, client *http.Client, target string) {
	req, err := http.NewRequestWithContext(inbound.Context(), inbound.Method, target, inbound.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer pod-scoped-token")
	resp, err := client.Do(req)
	if err != nil {
		http.Error(w, "upstream unavailable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

type hookClient struct {
	base   string
	client *http.Client
}

// accept returns success only after an exact receipt is proven by either the
// POST response or the recovery GET. A hook maps every error to exit code 2 so
// the harness blocks model processing.
func (h hookClient) accept(ctx context.Context, want receipt) error {
	body, _ := json.Marshal(want)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, h.base+"/task-receipts", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if got, status, err := h.do(req); err == nil && (status == http.StatusCreated || status == http.StatusOK) {
		return match(want, got)
	}

	query, _ := http.NewRequestWithContext(ctx, http.MethodGet, h.base+"/task-receipts/"+want.AttemptID, nil)
	got, status, err := h.do(query)
	if err != nil || status != http.StatusOK {
		return errors.New("receipt unproven: block prompt")
	}
	return match(want, got)
}

func (h hookClient) do(req *http.Request) (receipt, int, error) {
	resp, err := h.client.Do(req)
	if err != nil {
		return receipt{}, 0, err
	}
	defer resp.Body.Close()
	var got receipt
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		return receipt{}, resp.StatusCode, err
	}
	return got, resp.StatusCode, nil
}

func match(want, got receipt) error {
	if want != got {
		return fmt.Errorf("receipt mismatch: want %+v, got %+v", want, got)
	}
	return nil
}

func testReceipt() receipt {
	return receipt{
		TaskID:    "task_11111111111111111111111111111111",
		AttemptID: "attempt_22222222222222222222222222222222",
		Runtime:   "codex",
		SessionID: "session-native",
		TurnID:    "turn-native",
	}
}

func TestHandshake(t *testing.T) {
	t.Run("create and identical replay", func(t *testing.T) {
		cp := httptest.NewServer(controlPlane(newStore()))
		defer cp.Close()
		sc := httptest.NewServer(sidecar("sol-test", cp.URL, cp.Client()))
		defer sc.Close()
		hook := hookClient{base: sc.URL, client: sc.Client()}
		if err := hook.accept(context.Background(), testReceipt()); err != nil {
			t.Fatal(err)
		}
		if err := hook.accept(context.Background(), testReceipt()); err != nil {
			t.Fatalf("identical replay: %v", err)
		}
	})

	t.Run("committed POST with lost response reconciles by GET", func(t *testing.T) {
		s := newStore()
		cp := httptest.NewServer(controlPlane(s))
		defer cp.Close()

		lostOnce := false
		transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
			resp, err := http.DefaultTransport.RoundTrip(req)
			if err == nil && req.Method == http.MethodPost && !lostOnce {
				lostOnce = true
				_ = resp.Body.Close()
				return nil, errors.New("simulated response loss after commit")
			}
			return resp, err
		})
		sc := httptest.NewServer(sidecar("sol-test", cp.URL, &http.Client{Transport: transport}))
		defer sc.Close()
		hook := hookClient{base: sc.URL, client: sc.Client()}
		if err := hook.accept(context.Background(), testReceipt()); err != nil {
			t.Fatal(err)
		}
		if !lostOnce {
			t.Fatal("test did not cross the response-loss cut point")
		}
	})

	t.Run("conflicting attempt never succeeds", func(t *testing.T) {
		cp := httptest.NewServer(controlPlane(newStore()))
		defer cp.Close()
		sc := httptest.NewServer(sidecar("sol-test", cp.URL, cp.Client()))
		defer sc.Close()
		hook := hookClient{base: sc.URL, client: sc.Client()}
		if err := hook.accept(context.Background(), testReceipt()); err != nil {
			t.Fatal(err)
		}
		conflict := testReceipt()
		conflict.SessionID = "different-session"
		if err := hook.accept(context.Background(), conflict); err == nil {
			t.Fatal("conflicting reuse of attempt ID was accepted")
		}
	})

	t.Run("unavailable POST and GET fails closed", func(t *testing.T) {
		dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
		}))
		dead.Close()
		hook := hookClient{base: dead.URL, client: &http.Client{Timeout: 100 * time.Millisecond}}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := hook.accept(ctx, testReceipt()); err == nil {
			t.Fatal("unproven receipt was accepted")
		}
	})
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

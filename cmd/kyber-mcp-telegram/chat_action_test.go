package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestChatActionManagerRefreshesAndStops(t *testing.T) {
	var calls atomic.Int64
	called := make(chan struct{}, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/botTOK/sendChatAction" {
			t.Errorf("path = %q, want sendChatAction", got)
		}
		_ = r.ParseForm()
		if r.Form.Get("chat_id") != "42" || r.Form.Get("action") != "typing" {
			t.Errorf("form = %v", r.Form)
		}
		calls.Add(1)
		called <- struct{}{}
		_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
	}))
	defer server.Close()

	cfg := &config{botToken: "TOK", botAPIBaseURL: server.URL}
	m := newChatActionManager(context.Background(), cfg, server.Client())
	m.refresh = 10 * time.Millisecond
	m.maxAge = time.Second
	defer m.close()

	m.start("42")
	for i := 0; i < 2; i++ {
		select {
		case <-called:
		case <-time.After(time.Second):
			t.Fatal("typing indicator was not refreshed")
		}
	}
	if !m.active("42") {
		t.Fatal("typing lease is not active")
	}

	m.stop("42")
	time.Sleep(20 * time.Millisecond) // let an in-flight request finish
	stoppedAt := calls.Load()
	time.Sleep(40 * time.Millisecond)
	if got := calls.Load(); got != stoppedAt {
		t.Fatalf("typing calls continued after stop: %d -> %d", stoppedAt, got)
	}
	if m.active("42") {
		t.Fatal("typing lease remained active after stop")
	}
}

func TestChatActionManagerExpires(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
	}))
	defer server.Close()

	cfg := &config{botToken: "TOK", botAPIBaseURL: server.URL}
	m := newChatActionManager(context.Background(), cfg, server.Client())
	m.refresh = 5 * time.Millisecond
	m.maxAge = 20 * time.Millisecond
	defer m.close()
	m.start("42")

	deadline := time.Now().Add(time.Second)
	for m.active("42") && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if m.active("42") {
		t.Fatal("typing lease did not expire")
	}
}

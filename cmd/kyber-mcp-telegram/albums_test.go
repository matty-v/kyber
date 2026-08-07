package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestAlbumCollectorCoalescesMediaGroup(t *testing.T) {
	var mu sync.Mutex
	var envelopes []inboundEnvelope
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var env inboundEnvelope
		_ = json.NewDecoder(r.Body).Decode(&env)
		mu.Lock()
		envelopes = append(envelopes, env)
		mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg := &config{allowedUsers: map[int64]bool{42: true}, inboundURL: server.URL, agentName: "dave",
		bindingName: "telegram", hmacSecret: []byte("s"), files: newFileSet()}
	cfg.albums = newAlbumCollector(ctx, cfg, server.Client())
	for i, fileID := range []string{"photo-a", "photo-b"} {
		if err := handleUpdate(ctx, cfg, server.Client(), telegramUpdate{Message: &telegramMessage{
			MessageID: int64(i + 1), MediaGroupID: "group-1", Chat: telegramChat{ID: 42},
			From: telegramUser{ID: 42}, Photo: []telegramPhotoSize{{FileID: fileID}},
		}}); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(albumSettleDelay + 200*time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(envelopes) != 1 {
		t.Fatalf("got %d inbound envelopes, want 1", len(envelopes))
	}
	var attachments []albumAttachment
	if err := json.Unmarshal([]byte(envelopes[0].Attachments), &attachments); err != nil {
		t.Fatal(err)
	}
	if envelopes[0].EventType != "album" || len(attachments) != 2 || !cfg.files.has("photo-a") || !cfg.files.has("photo-b") {
		t.Fatalf("envelope=%+v attachments=%+v", envelopes[0], attachments)
	}
}

func TestAlbumCollectorRetriesAsynchronousDelivery(t *testing.T) {
	var attempts int
	delivered := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		if attempts == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		delivered <- struct{}{}
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg := &config{inboundURL: server.URL, agentName: "dave", bindingName: "telegram", hmacSecret: []byte("s")}
	collector := newAlbumCollector(ctx, cfg, server.Client())
	collector.retryDelay = time.Millisecond
	collector.add(telegramMessage{MediaGroupID: "group-1", Chat: telegramChat{ID: 42},
		From: telegramUser{ID: 42}, Photo: []telegramPhotoSize{{FileID: "photo-a"}}})
	select {
	case <-delivered:
	case <-time.After(2 * time.Second):
		t.Fatal("album was not delivered after retry")
	}
	if attempts != 2 {
		t.Fatalf("delivery attempts=%d want 2", attempts)
	}
}

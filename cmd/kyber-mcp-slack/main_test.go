package main

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestForwardMentionOnly(t *testing.T) {
	var deliveries atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deliveries.Add(1)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()
	cfg := config{inboundURL: srv.URL, agentName: "boba", binding: "slack", hmacSecret: "test", botID: "UBOT", mentionOnly: true, users: map[string]bool{"UMATT": true}, channels: map[string]bool{"CONE": true}}
	event := func(text, channelType, parent string) eventEnvelope {
		var e eventEnvelope
		e.EnvelopeID = "env"
		e.Payload.Event = slackEvent{Type: "message", User: "UMATT", Channel: "CONE", ChannelType: channelType, Text: text, TS: "1.1", ParentUserID: parent}
		return e
	}
	tests := []struct {
		name, text, channelType, parent string
		want                            int32
	}{
		{"plain channel message", "hello", "channel", "", 0},
		{"direct mention", "<@UBOT> hello", "channel", "", 1},
		{"reply to bot", "hello", "channel", "UBOT", 1},
		{"direct message", "hello", "im", "", 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			deliveries.Store(0)
			if err := forward(t.Context(), cfg, event(tc.text, tc.channelType, tc.parent), srv.Client()); err != nil {
				t.Fatal(err)
			}
			if got := deliveries.Load(); got != tc.want {
				t.Fatalf("deliveries=%d want %d", got, tc.want)
			}
		})
	}
}

func TestForwardDropsBotAndSubtypeMessages(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Fatal("unexpected delivery") }))
	defer srv.Close()
	cfg := config{inboundURL: srv.URL, agentName: "boba", binding: "slack", users: map[string]bool{"U": true}, channels: map[string]bool{"C": true}}
	for _, ev := range []slackEvent{{Type: "message", User: "U", Channel: "C", Text: "bot", BotID: "B"}, {Type: "message", User: "U", Channel: "C", Text: "edit", Subtype: "message_changed"}} {
		var e eventEnvelope
		e.Payload.Event = ev
		if err := forward(t.Context(), cfg, e, srv.Client()); err != nil {
			t.Fatal(err)
		}
	}
}

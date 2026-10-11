package tokenreport

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestHTTPUsageSink_SendsContentFreeEvidenceAndWaitsForACK(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if string(body["agentUID"]) != `"uid-1"` || string(body["routeFingerprint"]) != `"route-1"` {
			t.Errorf("missing lifetime or route stamp: %v", body)
		}
		if r.URL.Path == "/usage-gap" {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	sink := &HTTPUsageSink{SidecarURL: server.URL, AgentUID: "uid-1", RouteFingerprint: "route-1", Runtime: "claude"}
	cursor := UsageAckCursor{Namespace: "session-1", Offset: 42}
	if err := sink.AcceptUsage(context.Background(), UsageEvidence{Runtime: "claude", SourceIdentity: "session-1:msg-1", Model: "claude-test"}, cursor); err != nil {
		t.Fatal(err)
	}
	if err := sink.AcceptGap(context.Background(), "session-1:malformed-42", "malformed", time.Now(), cursor); err == nil {
		t.Fatal("gap without durable ACK reported success")
	}
	if len(paths) != 2 || paths[0] != "/usage-event" || paths[1] != "/usage-gap" {
		t.Fatalf("paths=%v", paths)
	}
}

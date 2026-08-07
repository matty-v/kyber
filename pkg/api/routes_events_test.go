package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/matty-v/kyber/pkg/api"
	"github.com/matty-v/kyber/pkg/messagebuffer"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// newTestEventsServer builds a Server with an initialised EventBus (no informers needed for tests).
func newTestEventsServer(t *testing.T) *api.Server {
	t.Helper()
	scheme := mustNewScheme(t)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	bus := api.NewEventBus()
	return &api.Server{
		K8sClient:     fakeClient,
		MessageBuffer: messagebuffer.NewMemoryBuffer(),
		APIKey:        testAPIKey,
		Namespace:     "kyber-system",
		EventBus:      bus,
	}
}

// dialTestWS dials a WebSocket on an httptest server with the given path,
// including the auth header.  Caller must close the connection.
func dialTestWS(t *testing.T, srv *httptest.Server, path string) (*websocket.Conn, *http.Response) {
	t.Helper()
	u := "ws" + strings.TrimPrefix(srv.URL, "http") + path
	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer "+testAPIKey)
	conn, resp, err := websocket.DefaultDialer.Dial(u, hdr)
	if err != nil {
		t.Fatalf("websocket dial: %v (resp=%v)", err, resp)
	}
	return conn, resp
}

// TestEvents_NoAuth verifies that missing the API key returns 401 (upgrade rejected).
func TestEvents_NoAuth(t *testing.T) {
	s := newTestEventsServer(t)
	srv := httptest.NewServer(s.BuildHandler())
	defer srv.Close()

	u := "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/v1/events"
	_, resp, err := websocket.DefaultDialer.Dial(u, nil)
	if err == nil {
		t.Fatal("expected dial to fail, got nil error")
	}
	if resp == nil {
		t.Fatal("expected HTTP response with 401, got nil")
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("want 401, got %d", resp.StatusCode)
	}
}

// TestEvents_HappyPath verifies that an authenticated WebSocket client receives
// events published on the bus.
func TestEvents_HappyPath(t *testing.T) {
	s := newTestEventsServer(t)
	srv := httptest.NewServer(s.BuildHandler())
	defer srv.Close()

	conn, _ := dialTestWS(t, srv, "/api/v1/events")
	defer conn.Close()

	// Give the handler goroutine time to subscribe.
	time.Sleep(20 * time.Millisecond)

	// Publish an event directly on the bus.
	want := api.Event{
		Type:      "agent.created",
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Resource: api.EventResource{
			Kind:    "Agent",
			Name:    "dave",
			Phase:   "Starting",
			Machine: "laptop-1",
		},
	}
	s.EventBus.Publish(want)

	// Read the message from the WebSocket.
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, msg, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("reading WebSocket message: %v", err)
	}

	var got api.Event
	if err := json.Unmarshal(msg, &got); err != nil {
		t.Fatalf("unmarshal event: %v", err)
	}

	if got.Type != want.Type {
		t.Errorf("type: want %q got %q", want.Type, got.Type)
	}
	if got.Resource.Kind != want.Resource.Kind {
		t.Errorf("resource.kind: want %q got %q", want.Resource.Kind, got.Resource.Kind)
	}
	if got.Resource.Name != want.Resource.Name {
		t.Errorf("resource.name: want %q got %q", want.Resource.Name, got.Resource.Name)
	}
	if got.Resource.Machine != want.Resource.Machine {
		t.Errorf("resource.machine: want %q got %q", want.Resource.Machine, got.Resource.Machine)
	}
}

// TestEvents_MultipleClients verifies that events reach all connected clients.
func TestEvents_MultipleClients(t *testing.T) {
	s := newTestEventsServer(t)
	srv := httptest.NewServer(s.BuildHandler())
	defer srv.Close()

	conn1, _ := dialTestWS(t, srv, "/api/v1/events")
	defer conn1.Close()
	conn2, _ := dialTestWS(t, srv, "/api/v1/events")
	defer conn2.Close()

	// Allow both handlers to subscribe.
	time.Sleep(30 * time.Millisecond)

	evt := api.Event{
		Type:      "machine.updated",
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Resource:  api.EventResource{Kind: "Machine", Name: "node-1"},
	}
	s.EventBus.Publish(evt)

	for i, conn := range []*websocket.Conn{conn1, conn2} {
		conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, msg, err := conn.ReadMessage()
		if err != nil {
			t.Errorf("client %d: read error: %v", i+1, err)
			continue
		}
		var got api.Event
		if err := json.Unmarshal(msg, &got); err != nil {
			t.Errorf("client %d: unmarshal: %v", i+1, err)
			continue
		}
		if got.Type != evt.Type {
			t.Errorf("client %d: type: want %q got %q", i+1, evt.Type, got.Type)
		}
	}
}

// TestEvents_NilBus verifies the endpoint returns 503 when no EventBus is configured.
func TestEvents_NilBus(t *testing.T) {
	scheme := mustNewScheme(t)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	s := &api.Server{
		K8sClient:     fakeClient,
		MessageBuffer: messagebuffer.NewMemoryBuffer(),
		APIKey:        testAPIKey,
		Namespace:     "kyber-system",
		// EventBus intentionally nil
	}
	h := s.BuildHandler()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/events", nil)
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	// Without a WebSocket upgrade the handler short-circuits at the nil-bus check.
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("want 503, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestEvents_MethodNotAllowed verifies that non-GET requests to /api/v1/events return 405.
func TestEvents_MethodNotAllowed(t *testing.T) {
	s := newTestEventsServer(t)
	h := s.BuildHandler()

	req := httptest.NewRequest(http.MethodPost, "/api/v1/events", nil)
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("want 405, got %d: %s", rr.Code, rr.Body.String())
	}
}

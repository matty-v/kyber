package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
)

func TestWSCheckOrigin_EmptyAllowlist_AllowsAll(t *testing.T) {
	setWSAllowedOrigins(nil) // reset to default
	t.Cleanup(func() { setWSAllowedOrigins(nil) })

	r := httptest.NewRequest(http.MethodGet, "/api/v1/events", nil)
	r.Header.Set("Origin", "https://anywhere.example")
	if !wsUpgrader.CheckOrigin(r) {
		t.Error("empty allowlist should allow any origin (preserves non-browser clients)")
	}
}

func TestWSCheckOrigin_WithAllowlist_AllowsMatched(t *testing.T) {
	setWSAllowedOrigins([]string{"https://holocron.example.com"})
	t.Cleanup(func() { setWSAllowedOrigins(nil) })

	r := httptest.NewRequest(http.MethodGet, "/api/v1/events", nil)
	r.Header.Set("Origin", "https://holocron.example.com")
	if !wsUpgrader.CheckOrigin(r) {
		t.Error("matched origin should be allowed")
	}
}

func TestWSCheckOrigin_WithAllowlist_RejectsUnmatched(t *testing.T) {
	setWSAllowedOrigins([]string{"https://holocron.example.com"})
	t.Cleanup(func() { setWSAllowedOrigins(nil) })

	r := httptest.NewRequest(http.MethodGet, "/api/v1/events", nil)
	r.Header.Set("Origin", "https://attacker.example")
	if wsUpgrader.CheckOrigin(r) {
		t.Error("unmatched origin should be rejected")
	}
}

// TestWSCheckOrigin_SameOrigin_AllowedDespiteAllowlist is the kyber#657 regression.
// Reproduces a real production configuration: the allowlist names only Holocron, and
// the request comes from the PWA the control plane itself serves. Before the fix this
// returned 403 — the Shell tab and the live event stream of the embedded UI were dead
// on every install that had configured CORS at all.
func TestWSCheckOrigin_SameOrigin_AllowedDespiteAllowlist(t *testing.T) {
	setWSAllowedOrigins([]string{"https://holocron.example.com", "http://localhost:5173"})
	t.Cleanup(func() { setWSAllowedOrigins(nil) })

	r := httptest.NewRequest(http.MethodGet, "/api/v1/agents/barf/exec", nil)
	r.Host = "kyber-lonestar.example.com"
	r.Header.Set("Origin", "https://kyber-lonestar.example.com")
	if !wsUpgrader.CheckOrigin(r) {
		t.Error("the control plane must accept WebSockets from the PWA it serves itself, " +
			"without the operator listing their own URL in cors.allowedOrigins")
	}
}

// TestWSCheckOrigin_SameOrigin_WithPort covers a dev/self-hosted install reached on a
// non-default port: Origin carries the port and so does Host, so they still agree.
func TestWSCheckOrigin_SameOrigin_WithPort(t *testing.T) {
	setWSAllowedOrigins([]string{"https://holocron.example.com"})
	t.Cleanup(func() { setWSAllowedOrigins(nil) })

	r := httptest.NewRequest(http.MethodGet, "/api/v1/events", nil)
	r.Host = "localhost:8080"
	r.Header.Set("Origin", "http://localhost:8080")
	if !wsUpgrader.CheckOrigin(r) {
		t.Error("same host:port should be treated as same-origin")
	}
}

// TestWSCheckOrigin_DifferentPortIsNotSameOrigin pins that the same-origin rule is not
// a host-prefix match: a different port is a different origin and must still face the
// allowlist. Otherwise anything on the box could dial the exec socket.
func TestWSCheckOrigin_DifferentPortIsNotSameOrigin(t *testing.T) {
	setWSAllowedOrigins([]string{"https://holocron.example.com"})
	t.Cleanup(func() { setWSAllowedOrigins(nil) })

	r := httptest.NewRequest(http.MethodGet, "/api/v1/events", nil)
	r.Host = "localhost:8080"
	r.Header.Set("Origin", "http://localhost:9999")
	if wsUpgrader.CheckOrigin(r) {
		t.Error("a different port is a different origin — it must still be gated by the allowlist")
	}
}

// TestWSCheckOrigin_CrossOriginStillRejected pins that the same-origin rule did not
// widen anything: an attacker page on another host is still rejected.
func TestWSCheckOrigin_CrossOriginStillRejected(t *testing.T) {
	setWSAllowedOrigins([]string{"https://holocron.example.com"})
	t.Cleanup(func() { setWSAllowedOrigins(nil) })

	r := httptest.NewRequest(http.MethodGet, "/api/v1/agents/barf/exec", nil)
	r.Host = "kyber-lonestar.example.com"
	r.Header.Set("Origin", "https://attacker.example")
	if wsUpgrader.CheckOrigin(r) {
		t.Error("cross-origin must still be rejected when it is not on the allowlist")
	}
}

// TestWSCheckOrigin_NullOriginNotSameOrigin: a sandboxed iframe sends the literal
// string "null", which parses to an empty host. It must not be mistaken for
// same-origin — it falls through to the allowlist like any other cross-origin caller.
func TestWSCheckOrigin_NullOriginNotSameOrigin(t *testing.T) {
	setWSAllowedOrigins([]string{"https://holocron.example.com"})
	t.Cleanup(func() { setWSAllowedOrigins(nil) })

	r := httptest.NewRequest(http.MethodGet, "/api/v1/events", nil)
	r.Host = "kyber-lonestar.example.com"
	r.Header.Set("Origin", "null")
	if wsUpgrader.CheckOrigin(r) {
		t.Error(`Origin "null" must not be treated as same-origin`)
	}
}

func TestWSCheckOrigin_WithAllowlist_AllowsNoOrigin(t *testing.T) {
	// Non-browser clients (Go, curl, CLI) don't send Origin. They authenticate
	// via the api key in ?token=, so we let them through.
	setWSAllowedOrigins([]string{"https://holocron.example.com"})
	t.Cleanup(func() { setWSAllowedOrigins(nil) })

	r := httptest.NewRequest(http.MethodGet, "/api/v1/events", nil)
	if !wsUpgrader.CheckOrigin(r) {
		t.Error("missing Origin header (non-browser client) should be allowed")
	}
}

// TestWSUpgrade_SameOrigin_Returns101 goes through gorilla's Upgrade() rather than
// calling CheckOrigin directly, because the bug reported in kyber#657 was an HTTP
// status: the handshake returned 403 where it should have returned 101. A unit test
// on the predicate alone would not have caught a regression in how it is wired.
func TestWSUpgrade_SameOrigin_Returns101(t *testing.T) {
	setWSAllowedOrigins([]string{"https://holocron.example.com"})
	t.Cleanup(func() { setWSAllowedOrigins(nil) })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn := upgradeWebSocket(w, r)
		if conn == nil {
			return
		}
		_ = conn.Close()
	}))
	t.Cleanup(srv.Close)

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")

	// Same-origin: Origin's host == the server's Host. Must upgrade even though the
	// allowlist names only Holocron.
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, http.Header{
		"Origin": []string{srv.URL},
	})
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		t.Fatalf("same-origin upgrade failed (HTTP %d): %v — this is the kyber#657 regression", status, err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Errorf("want 101, got %d", resp.StatusCode)
	}
	_ = conn.Close()

	// Cross-origin off the allowlist: still rejected, with the same 403 as before.
	_, resp2, err := websocket.DefaultDialer.Dial(wsURL, http.Header{
		"Origin": []string{"https://attacker.example"},
	})
	if err == nil {
		t.Fatal("cross-origin upgrade should have been rejected")
	}
	if resp2 == nil || resp2.StatusCode != http.StatusForbidden {
		got := 0
		if resp2 != nil {
			got = resp2.StatusCode
		}
		t.Errorf("want 403 for a non-allowlisted cross origin, got %d", got)
	}
}

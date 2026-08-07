package api

import (
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// wsAllowedOrigins is the CORS allowlist applied to WebSocket upgrades.
// Browsers don't run a preflight on WS — the server has to enforce origin in
// the upgrade handshake. Non-browser clients don't send Origin and are
// always allowed; the api key in ?token=<key> is the auth.
var (
	wsAllowedOrigins   map[string]struct{}
	wsAllowedOriginsMu sync.RWMutex
)

// setWSAllowedOrigins replaces the WebSocket origin allowlist atomically.
// Pass nil or an empty slice to disable origin checks (any origin allowed,
// preserving current non-browser-client behavior). Called from
// Server.buildTopHandler so both Start() and BuildHandler() (test path)
// wire it.
func setWSAllowedOrigins(allowed []string) {
	m := make(map[string]struct{}, len(allowed))
	for _, o := range allowed {
		m[strings.ToLower(strings.TrimSpace(o))] = struct{}{}
	}
	wsAllowedOriginsMu.Lock()
	wsAllowedOrigins = m
	wsAllowedOriginsMu.Unlock()
}

// isSameOriginWS reports whether an Origin header refers to the same host the
// request was made to. This is gorilla's own default CheckOrigin test, compared on
// host (including port) and case-insensitively; scheme is deliberately not compared,
// matching that default.
//
// Correct behind a proxy or tunnel: the browser derives Origin and the Host header
// from the same request URL, and cloudflared/ingress preserve Host — so the two
// still agree. A sandboxed iframe's literal "null" Origin parses to an empty host
// and therefore never matches a real one.
func isSameOriginWS(origin, host string) bool {
	if origin == "" || host == "" {
		return false
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	return strings.EqualFold(u.Host, host)
}

// wsUpgrader is the shared WebSocket upgrader for the events and exec endpoints.
// CheckOrigin, in order:
//   - Missing Origin header (non-browser clients): always allowed — these authenticate
//     via the api key in ?token=<key>.
//   - Same-origin (Origin's host == this request's Host): always allowed. The
//     control plane serves the embedded PWA, so that PWA's WebSockets are
//     same-origin by construction; the allowlist governs OTHER origins and must
//     never gate the server's own. Before kyber#657 it did: any install that set
//     cors.allowedOrigins (which every install must, to admit Holocron) started
//     403-ing the Shell tab and the live event stream of its own UI, while REST
//     kept working — same-origin fetch needs no CORS — so it read as "the shell is
//     broken", not "CORS is misconfigured".
//   - Empty allowlist: any origin allowed (today's behavior, preserved for installs
//     that haven't opted into origin checks).
//   - Non-empty allowlist: origin must match (case-insensitive exact string match)
//     or the upgrade is rejected with 403.
var wsUpgrader = websocket.Upgrader{
	HandshakeTimeout: 10 * time.Second,
	ReadBufferSize:   4096,
	WriteBufferSize:  4096,
	CheckOrigin: func(r *http.Request) bool {
		origin := r.Header.Get("Origin")
		if origin == "" {
			return true
		}
		if isSameOriginWS(origin, r.Host) {
			return true
		}
		wsAllowedOriginsMu.RLock()
		defer wsAllowedOriginsMu.RUnlock()
		if len(wsAllowedOrigins) == 0 {
			return true
		}
		_, ok := wsAllowedOrigins[strings.ToLower(origin)]
		return ok
	},
}

// upgradeWebSocket upgrades the HTTP connection to WebSocket and returns the
// connection. On error it writes the appropriate HTTP error and returns nil —
// the caller should return immediately when nil is returned.
func upgradeWebSocket(w http.ResponseWriter, r *http.Request) *websocket.Conn {
	conn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		// wsUpgrader.Upgrade already wrote an HTTP error response; nothing left to do.
		return nil
	}
	return conn
}

// wsSafeWriter serialises all writes to a WebSocket connection.
// gorilla/websocket requires that only one goroutine writes at a time.
// Embed a conn and write via the exported methods below.
type wsSafeWriter struct {
	conn *websocket.Conn
	send chan wsMessage
	done chan struct{}
}

type wsMessage struct {
	msgType int
	data    []byte
}

// newWSSafeWriter starts a background goroutine that drains the send channel
// and writes to conn. Call close() to shut it down.
func newWSSafeWriter(conn *websocket.Conn) *wsSafeWriter {
	sw := &wsSafeWriter{
		conn: conn,
		send: make(chan wsMessage, 64),
		done: make(chan struct{}),
	}
	go sw.loop()
	return sw
}

func (sw *wsSafeWriter) loop() {
	defer close(sw.done)
	for msg := range sw.send {
		if err := sw.conn.WriteMessage(msg.msgType, msg.data); err != nil {
			// Drop remaining messages — connection is broken.
			return
		}
	}
}

// WriteText enqueues a text WebSocket frame. Non-blocking — drops the message
// if the buffer is full (prevents a slow client from blocking event broadcast).
func (sw *wsSafeWriter) WriteText(data []byte) {
	select {
	case sw.send <- wsMessage{websocket.TextMessage, data}:
	default:
	}
}

// WriteBinary enqueues a binary WebSocket frame.
func (sw *wsSafeWriter) WriteBinary(data []byte) {
	select {
	case sw.send <- wsMessage{websocket.BinaryMessage, data}:
	default:
	}
}

// Close shuts down the writer goroutine and closes the connection.
func (sw *wsSafeWriter) Close() {
	close(sw.send)
	<-sw.done
	_ = sw.conn.Close()
}

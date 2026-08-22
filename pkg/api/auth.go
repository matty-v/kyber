package api

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Scope is a named capability a caller may hold (kyber#474). Scopes are the
// authorization vocabulary checked at mutation chokepoints (see authz.go).
type Scope string

const (
	// ScopeLifecycleWrite permits the fail-safe lifecycle verbs
	// (start/stop/restart) and the OAuth re-auth resume to Running.
	ScopeLifecycleWrite Scope = "lifecycle:write"
	// ScopeLifecycleAdmin permits the impactful, less-fail-safe verb
	// (force-needs-auth). It is strictly higher than
	// ScopeLifecycleWrite (admin ⊃ write), so an admin caller also satisfies a
	// write requirement — this nesting is what guarantees the impactful verbs are
	// never less-protected than fail-safe Stop (the #474 privilege-ordering AC).
	ScopeLifecycleAdmin Scope = "lifecycle:admin"
)

// ScopeSet is the set of scopes a Caller holds. A full-scope set (the legacy
// shared key) satisfies every check; otherwise membership is explicit, with the
// admin ⊃ write nesting applied in Has.
type ScopeSet struct {
	full   bool
	scopes map[Scope]bool
}

// newFullScopeSet returns a ScopeSet that satisfies every scope check — used for
// the legacy shared API key so single-key installs are unaffected by authz.
func newFullScopeSet() ScopeSet { return ScopeSet{full: true} }

// newScopeSet builds an explicit scope set.
func newScopeSet(scopes ...Scope) ScopeSet {
	m := make(map[Scope]bool, len(scopes))
	for _, s := range scopes {
		m[s] = true
	}
	return ScopeSet{scopes: m}
}

// Has reports whether the set satisfies the required scope, applying the
// admin ⊃ write nesting: holding ScopeLifecycleAdmin satisfies a
// ScopeLifecycleWrite requirement. A full-scope set satisfies everything.
func (s ScopeSet) Has(required Scope) bool {
	if s.full {
		return true
	}
	if s.scopes[required] {
		return true
	}
	// Nesting: admin implies write.
	if required == ScopeLifecycleWrite && s.scopes[ScopeLifecycleAdmin] {
		return true
	}
	return false
}

// Caller is the authenticated principal behind a request (kyber#474). Name is
// for audit logging only (never the key material); Scopes drives authorization.
type Caller struct {
	Name   string
	Scopes ScopeSet
}

// SecretKeyRef points a ScopedCaller at a Secret data key in the control
// plane's own namespace (kyber#557). Same-namespace-only by design: the cp's
// ClusterRole is cluster-wide on Secrets, so this app-level pin is what keeps
// the callers doc from designating Secrets outside the cp namespace.
type SecretKeyRef struct {
	Secret string `json:"secret"`
	Key    string `json:"key"`
}

// ScopedCaller is the operator-facing config shape for a scoped API key,
// parsed from the `callers` JSON document on the kyber-api-credentials Secret.
// Key is the 32-byte-hex secret the caller presents as its Bearer token.
// KeyFrom (kyber#557) sources that value from a Secret reference instead, so
// the shared callers doc carries no value — exactly one of Key/KeyFrom must
// be set; KeyFrom entries are filled in by ResolveScopedCallers at startup.
type ScopedCaller struct {
	Name    string        `json:"name"`
	Key     string        `json:"key,omitempty"`
	KeyFrom *SecretKeyRef `json:"keyFrom,omitempty"`
	Scopes  []string      `json:"scopes"`
}

// ParseScopedCallers parses the `callers` JSON document (a list of scoped keys)
// from the Secret. Returns an error on malformed JSON, an unknown scope, a
// caller missing a name, or an entry not carrying exactly one of key/keyFrom —
// fail-closed: a bad config must not silently grant. Parsing does no I/O;
// keyFrom references are resolved separately (ResolveScopedCallers).
func ParseScopedCallers(raw string) ([]ScopedCaller, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var callers []ScopedCaller
	if err := json.Unmarshal([]byte(raw), &callers); err != nil {
		return nil, fmt.Errorf("parsing callers JSON: %w", err)
	}
	for i, c := range callers {
		if c.Name == "" {
			return nil, fmt.Errorf("caller[%d]: name is required", i)
		}
		switch {
		case c.Key == "" && c.KeyFrom == nil:
			return nil, fmt.Errorf("caller %q: exactly one of key/keyFrom is required (neither set)", c.Name)
		case c.Key != "" && c.KeyFrom != nil:
			return nil, fmt.Errorf("caller %q: exactly one of key/keyFrom is required (both set)", c.Name)
		case c.KeyFrom != nil && (c.KeyFrom.Secret == "" || c.KeyFrom.Key == ""):
			return nil, fmt.Errorf("caller %q: keyFrom requires both secret and key names", c.Name)
		}
		for _, sc := range c.Scopes {
			if sc != string(ScopeLifecycleWrite) && sc != string(ScopeLifecycleAdmin) {
				return nil, fmt.Errorf("caller %q: unknown scope %q", c.Name, sc)
			}
		}
	}
	return callers, nil
}

// Authenticator validates an HTTP request and returns the caller's identity.
// V1 ships APIKeyAuthenticator. V2 can swap in OIDCAuthenticator without
// changing any route handler.
type Authenticator interface {
	Authenticate(r *http.Request) (*Caller, error)
}

// scopedKey binds a secret key to the Caller it authenticates.
type scopedKey struct {
	key    string
	caller Caller
}

// APIKeyAuthenticator validates the Bearer token in the Authorization header
// against the legacy shared key (full scope, mutable via SetKey for rotation
// #143) and an optional set of named scoped keys (kyber#474).
type APIKeyAuthenticator struct {
	mu       sync.RWMutex
	key      string // legacy full-scope key (rotatable)
	callers  []scopedKey
	sessions map[string]browserSession
}

type browserSession struct {
	caller    Caller
	expiresAt time.Time
}

// NewAPIKeyAuthenticator returns an Authenticator that accepts the legacy key
// (resolving to a full-scope caller) plus any scoped callers. Existing callers
// pass no scoped callers and get exactly the prior behavior.
func NewAPIKeyAuthenticator(key string, callers ...ScopedCaller) *APIKeyAuthenticator {
	a := &APIKeyAuthenticator{key: key, sessions: make(map[string]browserSession)}
	for _, c := range callers {
		scopes := make([]Scope, 0, len(c.Scopes))
		for _, sc := range c.Scopes {
			scopes = append(scopes, Scope(sc))
		}
		a.callers = append(a.callers, scopedKey{
			key:    c.Key,
			caller: Caller{Name: c.Name, Scopes: newScopeSet(scopes...)},
		})
	}
	return a
}

const browserSessionCookie = "kyber_browser_session"

const (
	browserSessionTTL  = 12 * time.Hour
	maxBrowserSessions = 256
)

// CreateBrowserSession issues an opaque, process-local browser credential.
// The API key never becomes the cookie value and sessions disappear when the
// control plane restarts.
func (a *APIKeyAuthenticator) CreateBrowserSession(caller Caller) (string, error) {
	token, err := generateBrowserSessionToken()
	if err != nil {
		return "", err
	}
	a.storeBrowserSession(token, caller)
	return token, nil
}

func generateBrowserSessionToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generating browser session: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

func (a *APIKeyAuthenticator) storeBrowserSession(token string, caller Caller) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	for existing, session := range a.sessions {
		if now.After(session.expiresAt) {
			delete(a.sessions, existing)
		}
	}
	if len(a.sessions) >= maxBrowserSessions {
		for existing := range a.sessions {
			delete(a.sessions, existing)
			break
		}
	}
	a.sessions[token] = browserSession{caller: caller, expiresAt: now.Add(browserSessionTTL)}
}

// ReplaceBrowserSessions revokes all browser sessions and installs one
// pre-generated replacement for the caller that initiated a key rotation.
func (a *APIKeyAuthenticator) ReplaceBrowserSessions(token string, caller Caller) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sessions = map[string]browserSession{
		token: {caller: caller, expiresAt: time.Now().Add(browserSessionTTL)},
	}
}

// ClearBrowserSessions revokes every browser session, used when the legacy
// key rotates so other signed-in browsers lose access immediately.
func (a *APIKeyAuthenticator) ClearBrowserSessions() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sessions = make(map[string]browserSession)
}

// SetKey atomically replaces the accepted legacy key. After this returns, the
// OLD key produces 401 and the NEW key authenticates as full-scope. Used by the
// API-key rotation endpoint (#143). Scoped callers are unaffected.
func (a *APIKeyAuthenticator) SetKey(key string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.key = key
}

// currentKey returns a snapshot of the active legacy key under read-lock.
func (a *APIKeyAuthenticator) currentKey() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.key
}

// Authenticate returns the resolved *Caller when the request carries a valid
// key, or an error (mapped to 401 Unauthorized) otherwise.
//
// Auth is accepted from three sources (first present wins):
//  1. Authorization: Bearer <key> header — used by REST clients.
//  2. Opaque HttpOnly browser-session cookie — used by the embedded PWA.
//  3. ?token=<key> query parameter — accepted ONLY on WebSocket upgrade
//     requests, because browsers cannot set custom headers during the HTTP
//     upgrade handshake. Plain REST requests must use the Bearer header:
//     keys in URLs leak into proxy/ingress access logs and browser history.
//
// Resolution: the presented key is compared (constant-time) against every scoped
// caller key AND the legacy key. A scoped match resolves to that caller's
// scopes; the legacy key resolves to a full-scope caller. To avoid a timing
// oracle on WHICH key matched, all comparisons run before a decision is made.
func (a *APIKeyAuthenticator) Authenticate(r *http.Request) (*Caller, error) {
	if r.Header.Get("Authorization") == "" {
		if cookie, err := r.Cookie(browserSessionCookie); err == nil && cookie.Value != "" {
			a.mu.RLock()
			session, ok := a.sessions[cookie.Value]
			a.mu.RUnlock()
			if ok && time.Now().Before(session.expiresAt) {
				return &session.caller, nil
			}
			return nil, errSessionExpired("browser session expired or no longer valid — sign in again")
		}
	}
	presented, err := presentedKey(r)
	if err != nil {
		return nil, err
	}

	a.mu.RLock()
	legacy := a.key
	callers := a.callers
	a.mu.RUnlock()

	var matched *Caller
	for i := range callers {
		if subtle.ConstantTimeCompare([]byte(presented), []byte(callers[i].key)) == 1 {
			c := callers[i].caller
			matched = &c
		}
	}
	if subtle.ConstantTimeCompare([]byte(presented), []byte(legacy)) == 1 {
		matched = &Caller{Name: "legacy", Scopes: newFullScopeSet()}
	}
	if matched == nil {
		return nil, errUnauthorized("invalid API key")
	}
	return matched, nil
}

// presentedKey extracts the API key from the Bearer header, or — on WebSocket
// upgrade requests only — the ?token= param. Returns a 401-mapped error when
// neither is present / the header is malformed.
func presentedKey(r *http.Request) (string, error) {
	if auth := r.Header.Get("Authorization"); auth != "" {
		parts := strings.SplitN(auth, " ", 2)
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
			return "", errUnauthorized("Authorization header must use Bearer scheme")
		}
		return parts[1], nil
	}
	if websocket.IsWebSocketUpgrade(r) {
		if token := r.URL.Query().Get("token"); token != "" {
			return token, nil
		}
	}
	return "", errUnauthorized("missing Authorization header")
}

// ErrCodeSessionExpired is the error code returned when a request carried a
// browser-session cookie that is no longer valid — expired, evicted, or
// (the common one) wiped by a control-plane restart, since sessions are
// process-local.
//
// It exists as its OWN code rather than sharing "unauthorized" because the
// client's response differs: an expired session is recoverable in place by
// re-authenticating, whereas a bad API key means the caller's credential is
// wrong. The embedded PWA keys its re-auth prompt on this code, and matching
// on the human-readable message instead would break the moment the wording
// changes.
const ErrCodeSessionExpired = "session_expired"

// authError is a sentinel type for authentication failures. code carries the
// machine-readable error code for the response body; empty means the generic
// "unauthorized".
type authError struct {
	msg  string
	code string
}

func (e *authError) Error() string { return e.msg }

// Code returns the response error code, defaulting to "unauthorized".
func (e *authError) Code() string {
	if e.code == "" {
		return "unauthorized"
	}
	return e.code
}

func errUnauthorized(msg string) error { return &authError{msg: msg} }

// errSessionExpired is errUnauthorized for the specific recoverable case of
// a dead browser session.
func errSessionExpired(msg string) error {
	return &authError{msg: msg, code: ErrCodeSessionExpired}
}

// callerCtxKey is the unexported context key under which the authenticated
// Caller is stashed by authMiddleware.
type callerCtxKey struct{}

// callerFrom returns the authenticated Caller stashed in ctx by authMiddleware,
// or nil if none (which should not happen behind the protected mux).
func callerFrom(ctx context.Context) *Caller {
	c, _ := ctx.Value(callerCtxKey{}).(*Caller)
	return c
}

// authMiddleware wraps next with API key authentication. Requests that pass are
// forwarded with the resolved Caller stashed in the request context; requests
// that fail receive 401.
func authMiddleware(auth Authenticator, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" && r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions {
			if _, err := r.Cookie(browserSessionCookie); err == nil && !sameOriginRequest(r) {
				writeJSONError(w, http.StatusForbidden, "forbidden_origin", "browser session requests require a same-origin Origin header")
				return
			}
		}
		caller, err := auth.Authenticate(r)
		if err != nil {
			// Preserve the specific code when the authenticator supplied one
			// (session_expired); everything else stays "unauthorized".
			code := "unauthorized"
			var aErr *authError
			if errors.As(err, &aErr) {
				code = aErr.Code()
			}
			writeJSONError(w, http.StatusUnauthorized, code, err.Error())
			return
		}
		ctx := context.WithValue(r.Context(), callerCtxKey{}, caller)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func sameOriginRequest(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return false
	}
	u, err := url.Parse(origin)
	if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return false
	}
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	return strings.EqualFold(u.Scheme, scheme) && strings.EqualFold(u.Host, r.Host)
}

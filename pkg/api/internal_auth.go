package api

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/matty-v/kyber/pkg/podtoken"
)

// Internal-API authentication + per-agent authorization (kyber#566, C1).
//
// The internal API (:8082) used to be unauthenticated by design — it trusted
// the cluster-internal network boundary. That trust was unfounded: every agent
// pod runs privileged and can dial :8082, so any agent could overwrite another
// agent's OAuth Secret, read another agent's session brief, or spoof another
// agent's telemetry through the {name} path segment. This is the load-bearing
// control that closes that gap: every :8082 request must present a
// control-plane-signed pod-token (pkg/podtoken) that resolves to a single
// identity, and each handler enforces act-on-self-only — a request to
// /internal/agents/{name}/* is admitted only when the caller's identity == name.
// The /internal/machines/* and /internal/nodes/* routes admit only the reserved
// node-agent identity.
//
// Authentication answers "is this a validly-signed token?"; authorization
// answers "may THIS identity act on THIS path?". The two failures map to
// distinct statuses: no/invalid token → 401 (unauthenticated); valid token but
// wrong identity → 403 (forbidden). The split matters for the grace-mode
// migration below.

// InternalIdentity is the principal behind an internal-API request — an agent
// name, or podtoken.NodeAgentIdentity. It is recovered from the verified token;
// it is the agent's name, never key material, so it is safe to audit-log.
type InternalIdentity string

// InternalAuthenticator validates an internal-API request and returns the
// caller's identity, or an error (mapped to 401) when no valid token is present.
// HMACInternalAuthenticator is the V1 implementation; the interface lets tests
// inject stubs and lets a future mechanism (e.g. SA-projected tokens) swap in
// without touching the handlers.
type InternalAuthenticator interface {
	Authenticate(r *http.Request) (InternalIdentity, error)
}

// HMACInternalAuthenticator resolves the Bearer pod-token on a request to its
// signed identity using the cluster signing key (Option A).
type HMACInternalAuthenticator struct {
	key []byte
}

// NewHMACInternalAuthenticator returns an authenticator backed by the cluster
// signing key. The key is the shared secret the reconciler also uses to mint
// per-agent tokens (see cmd/control-plane/main.go).
func NewHMACInternalAuthenticator(key []byte) *HMACInternalAuthenticator {
	return &HMACInternalAuthenticator{key: key}
}

// Authenticate extracts the Authorization: Bearer token and verifies it against
// the cluster key. Returns the embedded identity on success, or an error (→401)
// when the header is absent/malformed or the token fails verification.
func (a *HMACInternalAuthenticator) Authenticate(r *http.Request) (InternalIdentity, error) {
	auth := r.Header.Get("Authorization")
	if auth == "" {
		return "", errUnauthorized("missing Authorization header")
	}
	parts := strings.SplitN(auth, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return "", errUnauthorized("Authorization header must use Bearer scheme")
	}
	identity, err := podtoken.Parse(parts[1], a.key)
	if err != nil {
		return "", errUnauthorized("invalid pod token")
	}
	return InternalIdentity(identity), nil
}

// WithInternalAuth wires an authenticator into the InternalServer so every
// internal route enforces per-identity authorization. graceMode selects the
// one-release migration posture (kyber#566 rollout, Matt-locked): when true, a
// request with NO/invalid token is accepted-and-logged instead of rejected, so
// agent pods that have not yet re-rolled onto a mounted token keep working
// during the roll. graceMode never softens a cross-identity denial — a request
// bearing a VALID token for a different identity is always 403, because a valid
// token only exists post-roll (there is no migration reason to grace it). Ship
// graceMode=false (enforce) in the steady-state config; AC-7.
//
// When this option is not applied, the server is unauthenticated (pre-#566
// behavior) — used by the bulk of the existing tests and by the migration's
// first phase before the signing key is delivered.
func WithInternalAuth(authn InternalAuthenticator, graceMode bool) InternalServerOption {
	return func(s *InternalServer) {
		s.internalAuth = authn
		s.internalAuthGrace = graceMode
	}
}

// WithInternalAuthFailClosed puts the internal API into the fail-closed posture:
// every :8082 route refuses to serve (503) because authentication is required
// but no signing key was delivered (kyber#566 revision, Matt's security call).
// The alternative — serving :8082 unauthenticated when the key is missing —
// would silently re-open the cross-agent hole this whole change closes, so a
// misconfigured deploy must fail closed, not open. The gate is scoped to the
// :8082 handlers: the control plane's other surfaces (public API, health,
// metrics, controllers) keep running, so this refuses the internal API without
// crashlooping the process. Wired by cmd/control-plane/main.go only when
// KYBER_INTERNAL_SIGNING_KEY is absent; the normal path uses WithInternalAuth.
//
// Note this is NOT the same as omitting both options — that leaves the server
// intentionally unauthenticated (the back-compat mode used by tests and the
// migration's first phase). Fail-closed is an explicit, opt-in refusal.
func WithInternalAuthFailClosed() InternalServerOption {
	return func(s *InternalServer) {
		s.internalAuthFailClosed = true
	}
}

// authorizeAgentSelf enforces act-on-self-only for an /internal/agents/{name}/*
// request: the caller must present a valid token whose identity == agentName.
// Returns true if the request may proceed. The node-agent identity is NOT a
// valid caller on agent routes (it fails the identity == agentName check).
func (s *InternalServer) authorizeAgentSelf(w http.ResponseWriter, r *http.Request, agentName string) bool {
	return s.authorizeInternal(w, r, agentName)
}

// authorizeNodeAgent enforces that the caller of a machine/node route presents
// the reserved node-agent identity. Returns true if the request may proceed.
func (s *InternalServer) authorizeNodeAgent(w http.ResponseWriter, r *http.Request) bool {
	return s.authorizeInternal(w, r, podtoken.NodeAgentIdentity)
}

// authorizeInternal is the shared enforcement core. want is the identity the
// caller must hold to act on this path (the agent name, or node-agent). It
// returns true if the request may proceed.
//
//   - No authenticator wired → no enforcement (returns true). Back-compat.
//   - Valid token, identity == want → allow.
//   - Valid token, identity != want → 403 ALWAYS (even in grace mode).
//   - No/invalid token → 401 (enforce) or allow-and-log (grace mode).
func (s *InternalServer) authorizeInternal(w http.ResponseWriter, r *http.Request, want string) bool {
	if s.internalAuth == nil {
		if s.internalAuthFailClosed {
			// Auth is required but no signing key was delivered. Refuse to serve
			// rather than silently re-open the cross-agent hole (kyber#566). The
			// misconfiguration is loud: this logs on every refused request, on
			// top of the one-shot startup error in main.go.
			slog.Error("internal-auth fail-closed: signing key not configured; refusing internal API request",
				"route", r.URL.Path)
			http.Error(w, "internal API unavailable: authentication not configured", http.StatusServiceUnavailable)
			return false
		}
		return true
	}
	identity, err := s.internalAuth.Authenticate(r)
	if err != nil {
		// Unauthenticated: no or invalid token.
		if s.internalAuthGrace {
			slog.Warn("internal-auth would-deny (grace mode)",
				"want", want, "route", r.URL.Path,
				"decision", "would-deny-unauthenticated", "enforce", false)
			return true
		}
		slog.Warn("internal-auth deny",
			"want", want, "route", r.URL.Path,
			"decision", "deny-unauthenticated", "enforce", true)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return false
	}
	if string(identity) != want {
		// Authenticated but acting on someone else's resource. Never graced:
		// a valid token means the caller's pod has already re-rolled, so this
		// is a genuine cross-identity attempt, not a migration straggler.
		slog.Warn("internal-auth deny",
			"caller", string(identity), "want", want, "route", r.URL.Path,
			"decision", "deny-cross-identity", "enforce", true)
		http.Error(w, "forbidden", http.StatusForbidden)
		return false
	}
	slog.Info("internal-auth allow",
		"caller", string(identity), "route", r.URL.Path, "decision", "allow")
	return true
}

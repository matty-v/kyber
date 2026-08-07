// Package mockserver provides an in-process stand-in for Anthropic's OAuth
// token endpoint. It speaks the same shape the real endpoint does, validates
// PKCE challenge/verifier, and supports controlled failure injection. Tests
// point Kyber at it via the ANTHROPIC_TOKEN_URL env var.
package mockserver

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
)

type Server struct {
	mu            sync.Mutex
	pendingCodes  map[string]string // code -> challenge
	refreshTokens map[string]bool   // live refresh tokens
	rotateRefresh bool              // when true, refresh returns a new refresh_token
	failMode      string            // "", "expired_code", "invalid_verifier", "server_error", "expired_refresh"
}

func New() *Server {
	return &Server{
		pendingCodes:  map[string]string{},
		refreshTokens: map[string]bool{},
	}
}

// IssueCode registers a code bound to the given PKCE challenge. Tests call this
// to simulate the user having completed the authorize step in the browser.
func (s *Server) IssueCode(challenge string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	code := randomToken("code")
	s.pendingCodes[code] = challenge
	return code
}

// SetFailMode forces the next token exchange to return a specific failure.
func (s *Server) SetFailMode(mode string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failMode = mode
}

// SetRotateRefresh toggles whether refresh_token returns a new refresh token
// on grant_type=refresh_token. Used to exercise the rotation code path.
func (s *Server) SetRotateRefresh(v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rotateRefresh = v
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/v1/oauth/token" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	grant := r.FormValue("grant_type")
	if grant == "" {
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err == nil {
			grant = body["grant_type"]
			for k, v := range body {
				r.Form.Set(k, v)
			}
		}
	}
	s.mu.Lock()
	mode := s.failMode
	s.mu.Unlock()
	if mode == "server_error" {
		s.mu.Lock()
		s.failMode = ""
		s.mu.Unlock()
		writeErr(w, 500, "server_error", "injected failure")
		return
	}
	switch grant {
	case "authorization_code":
		s.handleCode(w, r, mode)
	case "refresh_token":
		s.handleRefresh(w, r, mode)
	default:
		writeErr(w, 400, "invalid_grant", "unsupported grant_type")
	}
}

func (s *Server) handleCode(w http.ResponseWriter, r *http.Request, mode string) {
	code := r.Form.Get("code")
	verifier := r.Form.Get("code_verifier")
	s.mu.Lock()
	challenge, ok := s.pendingCodes[code]
	if ok {
		delete(s.pendingCodes, code)
	}
	s.mu.Unlock()
	if !ok || mode == "expired_code" {
		if mode == "expired_code" {
			s.mu.Lock()
			s.failMode = ""
			s.mu.Unlock()
		}
		writeErr(w, 400, "invalid_grant", "code expired or invalid")
		return
	}
	expected := pkceChallenge(verifier)
	if expected != challenge || mode == "invalid_verifier" {
		if mode == "invalid_verifier" {
			s.mu.Lock()
			s.failMode = ""
			s.mu.Unlock()
		}
		writeErr(w, 400, "invalid_grant", "PKCE verifier mismatch")
		return
	}
	access := randomToken("at")
	refresh := randomToken("rt")
	s.mu.Lock()
	s.refreshTokens[refresh] = true
	s.mu.Unlock()
	writeTokens(w, access, refresh, 3600)
}

func (s *Server) handleRefresh(w http.ResponseWriter, r *http.Request, mode string) {
	rt := r.Form.Get("refresh_token")
	s.mu.Lock()
	valid := s.refreshTokens[rt]
	rotate := s.rotateRefresh
	s.mu.Unlock()
	if !valid || mode == "expired_refresh" {
		if mode == "expired_refresh" {
			s.mu.Lock()
			s.failMode = ""
			s.mu.Unlock()
		}
		writeErr(w, 400, "invalid_grant", "refresh token invalid")
		return
	}
	access := randomToken("at")
	newRT := rt
	if rotate {
		newRT = randomToken("rt")
		s.mu.Lock()
		delete(s.refreshTokens, rt)
		s.refreshTokens[newRT] = true
		s.mu.Unlock()
	}
	writeTokens(w, access, newRT, 3600)
}

func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func randomToken(prefix string) string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(errors.New("rand: " + err.Error()))
	}
	return prefix + "-" + hex.EncodeToString(b)
}

func writeTokens(w http.ResponseWriter, access, refresh string, expiresIn int) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token":  access,
		"refresh_token": refresh,
		"expires_in":    expiresIn,
		"token_type":    "Bearer",
	})
}

func writeErr(w http.ResponseWriter, status int, code, desc string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error":             code,
		"error_description": desc,
	})
}

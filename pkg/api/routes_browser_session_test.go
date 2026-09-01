package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBrowserSessionExchangeAndAuthentication(t *testing.T) {
	s := &Server{APIKey: "legacy-key"}
	h := s.BuildHandler()

	req := httptest.NewRequest(http.MethodPost, "/api/v1/browser-session", nil)
	req.Header.Set("Authorization", "Bearer legacy-key")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("exchange status = %d, want 204: %s", rr.Code, rr.Body.String())
	}
	result := rr.Result()
	cookies := result.Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookies = %d, want 1", len(cookies))
	}
	cookie := cookies[0]
	if cookie.Name != browserSessionCookie || !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("unsafe browser cookie: %+v", cookie)
	}
	if cookie.Value == "legacy-key" {
		t.Fatal("browser cookie must be opaque, not the API key")
	}

	authed := httptest.NewRequest(http.MethodGet, "/api/v1/config", nil)
	authed.AddCookie(cookie)
	authedRR := httptest.NewRecorder()
	h.ServeHTTP(authedRR, authed)
	if authedRR.Code == http.StatusUnauthorized {
		t.Fatal("opaque browser session did not authenticate")
	}
}

func TestBrowserSessionRejectsMissingOrInvalidBearer(t *testing.T) {
	s := &Server{APIKey: "legacy-key"}
	h := s.BuildHandler()
	for _, key := range []string{"", "wrong"} {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/browser-session", nil)
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusUnauthorized {
			t.Errorf("key %q status = %d, want 401", key, rr.Code)
		}
	}
}

func TestCookieMutationRequiresSameOrigin(t *testing.T) {
	a := NewAPIKeyAuthenticator("legacy-key")
	token, err := a.CreateBrowserSession(Caller{Name: "legacy", Scopes: newFullScopeSet()})
	if err != nil {
		t.Fatal(err)
	}
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	h := authMiddleware(a, next)

	for name, tc := range map[string]struct {
		origin string
		want   int
	}{
		"missing origin": {want: http.StatusForbidden},
		"cross origin":   {origin: "https://evil.example", want: http.StatusForbidden},
		"wrong scheme":   {origin: "http://kyber.example", want: http.StatusForbidden},
		"same origin":    {origin: "https://kyber.example", want: http.StatusNoContent},
	} {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "https://kyber.example/api/v1/x", nil)
			req.Header.Set("X-Forwarded-Proto", "https")
			req.Header.Set("Origin", tc.origin)
			req.AddCookie(&http.Cookie{Name: browserSessionCookie, Value: token})
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			if rr.Code != tc.want {
				var body any
				_ = json.Unmarshal(rr.Body.Bytes(), &body)
				t.Errorf("status = %d, want %d: %v", rr.Code, tc.want, body)
			}
		})
	}
}

func TestEncodeExecExitPayloadUsesJSONEncoding(t *testing.T) {
	message := "bad \"quote\"\\newline\n</script>"
	payload, err := encodeExecExitPayload(message)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Type  string `json:"type"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Fatalf("payload is not JSON: %v (%s)", err, payload)
	}
	if got.Type != "exit" || got.Error != message {
		t.Errorf("decoded payload = %+v", got)
	}
}

// TestExpiredBrowserSessionReturnsDistinctCode: a dead session cookie must
// answer with its own error code, not the generic "unauthorized".
//
// The distinction is what lets the PWA re-prompt for the key in place
// instead of rendering a dead-end error. Sessions no longer die on a restart
// (MAT-38), but they still lapse at 30 days and are all invalidated by an
// API-key rotation — both fully recoverable by pasting the key again.
func TestExpiredBrowserSessionReturnsDistinctCode(t *testing.T) {
	s := &Server{APIKey: "legacy-key"}
	h := s.BuildHandler()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/config", nil)
	// A cookie value this server would never have signed — the same 401 an
	// operator gets from a lapsed session or one minted before a rotation.
	req.AddCookie(&http.Cookie{Name: browserSessionCookie, Value: "stale-token-from-a-previous-process"})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401: %s", rr.Code, rr.Body.String())
	}
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v (%s)", err, rr.Body.String())
	}
	if body.Error.Code != ErrCodeSessionExpired {
		t.Errorf("code = %q, want %q", body.Error.Code, ErrCodeSessionExpired)
	}
	// The message is what a human reads when the prompt fails to appear, so
	// it should name the remedy rather than just the symptom.
	if !strings.Contains(body.Error.Message, "sign in again") {
		t.Errorf("message = %q, want it to name the remedy", body.Error.Message)
	}
}

// TestBadAPIKeyKeepsGenericUnauthorized: the new code must NOT leak onto
// the wrong failure. A bad bearer key is not recoverable by re-prompting
// for a session — the caller's credential itself is wrong — and a PWA that
// re-prompted on it would loop.
func TestBadAPIKeyKeepsGenericUnauthorized(t *testing.T) {
	s := &Server{APIKey: "legacy-key"}
	h := s.BuildHandler()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/config", nil)
	req.Header.Set("Authorization", "Bearer not-the-key")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Error.Code == ErrCodeSessionExpired {
		t.Errorf("bad API key must not report %q", ErrCodeSessionExpired)
	}
	if body.Error.Code != "unauthorized" {
		t.Errorf("code = %q, want \"unauthorized\"", body.Error.Code)
	}
}

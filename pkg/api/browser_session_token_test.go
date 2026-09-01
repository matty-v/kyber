package api

import (
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestBrowserSessionTokenRoundTrip(t *testing.T) {
	now := time.Unix(1_780_000_000, 0)

	for name, caller := range map[string]Caller{
		"full scope":   {Name: "legacy", Scopes: newFullScopeSet()},
		"scoped":       {Name: "ci", Scopes: newScopeSet(ScopeLifecycleWrite, ScopeRequestsRead)},
		"no scopes":    {Name: "reader", Scopes: newScopeSet()},
		"admin scoped": {Name: "ops", Scopes: newScopeSet(ScopeLifecycleAdmin)},
	} {
		t.Run(name, func(t *testing.T) {
			token, err := signBrowserSession("legacy-key", caller, now, browserSessionTTL)
			if err != nil {
				t.Fatal(err)
			}
			claims, err := verifyBrowserSession("legacy-key", token, now)
			if err != nil {
				t.Fatalf("verify: %v", err)
			}
			if claims.Name != caller.Name {
				t.Errorf("name = %q, want %q", claims.Name, caller.Name)
			}
			if claims.FullScope != caller.Scopes.full {
				t.Errorf("fullScope = %v, want %v", claims.FullScope, caller.Scopes.full)
			}
			if want := now.Add(browserSessionTTL); !claims.expiresAt().Equal(want) {
				t.Errorf("expiresAt = %v, want %v", claims.expiresAt(), want)
			}
		})
	}
}

// A token must not carry authority. It names a principal; the scopes come from
// live configuration at verify time, so narrowing or removing a caller takes
// effect on the next request instead of whenever the cookie expires.
func TestBrowserSessionScopesResolveAgainstLiveConfig(t *testing.T) {
	const key = "legacy-key"
	issuer := NewAPIKeyAuthenticator(key, ScopedCaller{
		Name: "ci", Key: "ci-key", Scopes: []string{string(ScopeLifecycleAdmin), string(ScopeRequestsWrite)},
	})
	token, err := issuer.CreateBrowserSession(Caller{Name: "ci", Scopes: newScopeSet(ScopeLifecycleAdmin, ScopeRequestsWrite)})
	if err != nil {
		t.Fatal(err)
	}
	withCookie := func() *http.Request {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/config", nil)
		req.AddCookie(&http.Cookie{Name: browserSessionCookie, Value: token})
		return req
	}

	t.Run("narrowed scopes apply immediately", func(t *testing.T) {
		narrowed := NewAPIKeyAuthenticator(key, ScopedCaller{
			Name: "ci", Key: "ci-key", Scopes: []string{string(ScopeRequestsRead)},
		})
		caller, err := narrowed.Authenticate(withCookie())
		if err != nil {
			t.Fatalf("authenticate: %v", err)
		}
		if caller.Scopes.Has(ScopeLifecycleAdmin) {
			t.Error("session still holds lifecycle:admin after it was removed from config")
		}
		if !caller.Scopes.Has(ScopeRequestsRead) {
			t.Error("session did not pick up the caller's current scopes")
		}
	})

	t.Run("removed caller is signed out", func(t *testing.T) {
		removed := NewAPIKeyAuthenticator(key)
		_, err := removed.Authenticate(withCookie())
		if err == nil {
			t.Fatal("a caller removed from config still authenticates")
		}
		var authErr *authError
		if !errors.As(err, &authErr) || authErr.Code() != ErrCodeSessionExpired {
			t.Errorf("err = %v, want code %q", err, ErrCodeSessionExpired)
		}
	})

	t.Run("a scoped caller cannot borrow the shared key's name", func(t *testing.T) {
		// A full-scope claim resolves only from the legacy key, never from the
		// callers list — so naming a scoped caller "legacy" grants it nothing.
		impostor := NewAPIKeyAuthenticator("", ScopedCaller{
			Name: "legacy", Key: "impostor-key", Scopes: []string{string(ScopeRequestsRead)},
		})
		full, err := signBrowserSession("some-key", Caller{Name: "legacy", Scopes: newFullScopeSet()}, time.Now(), browserSessionTTL)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodGet, "/api/v1/config", nil)
		req.AddCookie(&http.Cookie{Name: browserSessionCookie, Value: full})
		if _, err := impostor.Authenticate(req); err == nil {
			t.Fatal("full-scope session accepted with no shared key configured")
		}
	})
}

// Tokens are a pure function of (key, caller, issuedAt): signing is HMAC, not
// a randomized signature, so the same inputs must always produce the same
// cookie value. Guards against a future claims field with nondeterministic
// encoding.
func TestBrowserSessionTokenIsDeterministic(t *testing.T) {
	now := time.Unix(1_780_000_000, 0)
	caller := Caller{Name: "ci", Scopes: newScopeSet(ScopeRequestsRead, ScopeLifecycleAdmin, ScopeRequestsWrite, ScopeLifecycleWrite)}
	first, err := signBrowserSession("legacy-key", caller, now, browserSessionTTL)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		again, err := signBrowserSession("legacy-key", caller, now, browserSessionTTL)
		if err != nil {
			t.Fatal(err)
		}
		if again != first {
			t.Fatalf("token %d differs from the first: %q vs %q", i, again, first)
		}
	}
}

func TestBrowserSessionTokenRejections(t *testing.T) {
	now := time.Unix(1_780_000_000, 0)
	caller := Caller{Name: "legacy", Scopes: newFullScopeSet()}
	valid, err := signBrowserSession("legacy-key", caller, now, browserSessionTTL)
	if err != nil {
		t.Fatal(err)
	}
	version, rest, _ := strings.Cut(valid, ".")
	claims, mac, _ := strings.Cut(rest, ".")

	// Re-sign the same claims under a different key: a well-formed token whose
	// signature belongs to somebody else.
	foreign, err := signBrowserSession("other-key", caller, now, browserSessionTTL)
	if err != nil {
		t.Fatal(err)
	}
	_, foreignRest, _ := strings.Cut(foreign, ".")
	_, foreignMAC, _ := strings.Cut(foreignRest, ".")

	// Claims that grant more than the signature covers — the attack the MAC
	// exists to stop.
	escalated := base64.RawURLEncoding.EncodeToString([]byte(`{"n":"legacy","f":true,"iat":1780000000,"exp":9999999999}`))

	for name, token := range map[string]string{
		"empty":                           "",
		"no separator":                    "v1",
		"two segments":                    version + "." + claims,
		"four segments":                   valid + ".extra",
		"unknown version":                 "v2." + claims + "." + mac,
		"undecodable mac":                 version + "." + claims + ".!!!not-base64!!!",
		"undecodable claims":              version + ".!!!not-base64!!!." + mac,
		"truncated mac":                   version + "." + claims + "." + mac[:len(mac)-4],
		"signature from a different key":  version + "." + claims + "." + foreignMAC,
		"claims swapped under a kept mac": version + "." + escalated + "." + mac,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := verifyBrowserSession("legacy-key", token, now); err == nil {
				t.Fatal("token verified, want rejection")
			}
		})
	}
}

func TestBrowserSessionTokenExpires(t *testing.T) {
	issued := time.Unix(1_780_000_000, 0)
	token, err := signBrowserSession("legacy-key", Caller{Name: "legacy", Scopes: newFullScopeSet()}, issued, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := verifyBrowserSession("legacy-key", token, issued.Add(59*time.Minute)); err != nil {
		t.Fatalf("token rejected before expiry: %v", err)
	}
	// Exactly at expiry counts as expired — the boundary belongs to the dead
	// side, so a token can never be accepted at a moment its own claims say
	// it is finished.
	if _, err := verifyBrowserSession("legacy-key", token, issued.Add(time.Hour)); err == nil {
		t.Fatal("token accepted at its expiry instant")
	}
	if _, err := verifyBrowserSession("legacy-key", token, issued.Add(2*time.Hour)); err == nil {
		t.Fatal("token accepted after expiry")
	}
}

// An empty API key must not derive a usable signing key. Otherwise the
// derivation is a public constant and anyone could mint a full-scope session.
func TestBrowserSessionRequiresAnAPIKey(t *testing.T) {
	if _, err := signBrowserSession("", Caller{Name: "legacy", Scopes: newFullScopeSet()}, time.Now(), browserSessionTTL); !errors.Is(err, errNoBrowserSessionKey) {
		t.Errorf("sign err = %v, want errNoBrowserSessionKey", err)
	}
	if _, err := verifyBrowserSession("", "v1.x.y", time.Now()); !errors.Is(err, errNoBrowserSessionKey) {
		t.Errorf("verify err = %v, want errNoBrowserSessionKey", err)
	}
}

// The point of the whole change (MAT-38): a cookie issued by one process must
// still authenticate against a fresh one holding the same API key. Two
// authenticators standing in for before/after a control-plane restart.
func TestBrowserSessionSurvivesAControlPlaneRestart(t *testing.T) {
	before := NewAPIKeyAuthenticator("legacy-key")
	token, err := before.CreateBrowserSession(Caller{Name: "legacy", Scopes: newFullScopeSet()})
	if err != nil {
		t.Fatal(err)
	}

	after := NewAPIKeyAuthenticator("legacy-key")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/config", nil)
	req.AddCookie(&http.Cookie{Name: browserSessionCookie, Value: token})
	caller, err := after.Authenticate(req)
	if err != nil {
		t.Fatalf("session did not survive the restart: %v", err)
	}
	if caller.Name != "legacy" || !caller.Scopes.Has(ScopeLifecycleAdmin) {
		t.Errorf("caller = %+v, want the full-scope legacy caller", caller)
	}
}

// Rotation is the revocation lever. Because the signing key is derived from
// the API key, swapping the key must invalidate cookies minted under the old
// one — this is what ClearBrowserSessions used to do explicitly.
func TestRotatingTheAPIKeyInvalidatesExistingSessions(t *testing.T) {
	a := NewAPIKeyAuthenticator("legacy-key")
	token, err := a.CreateBrowserSession(Caller{Name: "legacy", Scopes: newFullScopeSet()})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/config", nil)
	req.AddCookie(&http.Cookie{Name: browserSessionCookie, Value: token})
	if _, err := a.Authenticate(req); err != nil {
		t.Fatalf("pre-rotation: %v", err)
	}

	a.SetKey("rotated-key")

	rotated := httptest.NewRequest(http.MethodGet, "/api/v1/config", nil)
	rotated.AddCookie(&http.Cookie{Name: browserSessionCookie, Value: token})
	_, err = a.Authenticate(rotated)
	if err == nil {
		t.Fatal("pre-rotation cookie still authenticates after rotation")
	}
	var authErr *authError
	if !errors.As(err, &authErr) || authErr.Code() != ErrCodeSessionExpired {
		t.Errorf("err = %v, want code %q so the PWA re-prompts", err, ErrCodeSessionExpired)
	}
}

func TestBrowserSessionRenewal(t *testing.T) {
	const key = "legacy-key"
	caller := Caller{Name: "legacy", Scopes: newFullScopeSet()}

	// issuedAgo controls how much lifetime is left: a token issued more than
	// half a TTL ago is inside the renewal window.
	tokenIssuedAgo := func(t *testing.T, d time.Duration) string {
		t.Helper()
		token, err := signBrowserSession(key, caller, time.Now().Add(-d), browserSessionTTL)
		if err != nil {
			t.Fatal(err)
		}
		return token
	}

	renewedCookie := func(t *testing.T, req *http.Request) *http.Cookie {
		t.Helper()
		a := NewAPIKeyAuthenticator(key)
		next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
		rr := httptest.NewRecorder()
		authMiddleware(a, next).ServeHTTP(rr, req)
		if rr.Code == http.StatusUnauthorized {
			t.Fatalf("request did not authenticate: %s", rr.Body.String())
		}
		for _, c := range rr.Result().Cookies() {
			if c.Name == browserSessionCookie {
				return c
			}
		}
		return nil
	}

	cookieRequest := func(token string) *http.Request {
		req := httptest.NewRequest(http.MethodGet, "https://kyber.example/api/v1/config", nil)
		req.Header.Set("X-Forwarded-Proto", "https")
		req.AddCookie(&http.Cookie{Name: browserSessionCookie, Value: token})
		return req
	}

	t.Run("fresh session is not re-issued", func(t *testing.T) {
		if c := renewedCookie(t, cookieRequest(tokenIssuedAgo(t, time.Hour))); c != nil {
			t.Fatalf("cookie re-issued for a fresh session: %+v", c)
		}
	})

	t.Run("half-spent session slides forward", func(t *testing.T) {
		old := tokenIssuedAgo(t, browserSessionTTL-browserSessionRenewAfter+time.Hour)
		c := renewedCookie(t, cookieRequest(old))
		if c == nil {
			t.Fatal("no cookie re-issued for a half-spent session")
		}
		if c.Value == old {
			t.Fatal("re-issued the same token")
		}
		claims, err := verifyBrowserSession(key, c.Value, time.Now())
		if err != nil {
			t.Fatalf("renewed token does not verify: %v", err)
		}
		if time.Until(claims.expiresAt()) <= browserSessionRenewAfter {
			t.Errorf("renewed token expires in %v, want more than %v", time.Until(claims.expiresAt()), browserSessionRenewAfter)
		}
	})

	t.Run("bearer requests are never given a cookie", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "https://kyber.example/api/v1/config", nil)
		req.Header.Set("Authorization", "Bearer "+key)
		if c := renewedCookie(t, req); c != nil {
			t.Fatalf("bearer request got a session cookie: %+v", c)
		}
	})

	t.Run("websocket upgrades are skipped", func(t *testing.T) {
		req := cookieRequest(tokenIssuedAgo(t, browserSessionTTL-browserSessionRenewAfter+time.Hour))
		req.Header.Set("Connection", "Upgrade")
		req.Header.Set("Upgrade", "websocket")
		if c := renewedCookie(t, req); c != nil {
			t.Fatalf("websocket upgrade got a Set-Cookie: %+v", c)
		}
	})
}

// A single request can stage a session cookie twice: authMiddleware renews a
// half-spent one before the handler runs, and /api/v1/rotate-api-key then
// issues a replacement signed under the NEW key. The response must carry only
// the live cookie — appending would also hand the browser the pre-rotation one,
// which no longer verifies, and leave the outcome to header ordering.
func TestSessionCookieIsReplacedNotAppended(t *testing.T) {
	const key = "legacy-key"
	a := NewAPIKeyAuthenticator(key)
	halfSpent, err := signBrowserSession(key, Caller{Name: "legacy", Scopes: newFullScopeSet()},
		time.Now().Add(-(browserSessionTTL - browserSessionRenewAfter + time.Hour)), browserSessionTTL)
	if err != nil {
		t.Fatal(err)
	}

	// Stands in for handleRotateAPIKey: swap the key, then re-cookie the
	// initiating browser under it.
	rotate := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.SetKey("rotated-key")
		token, err := a.CreateBrowserSession(Caller{Name: "legacy", Scopes: newFullScopeSet()})
		if err != nil {
			t.Fatal(err)
		}
		setBrowserSessionCookie(w, r, token)
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodPost, "https://kyber.example/api/v1/rotate-api-key", nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("Origin", "https://kyber.example")
	req.AddCookie(&http.Cookie{Name: browserSessionCookie, Value: halfSpent})
	rr := httptest.NewRecorder()
	authMiddleware(a, rotate).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rr.Code, rr.Body.String())
	}
	staged := rr.Result().Header.Values("Set-Cookie")
	if len(staged) != 1 {
		t.Fatalf("Set-Cookie headers = %d, want exactly 1: %v", len(staged), staged)
	}
	cookies := rr.Result().Cookies()
	if _, err := verifyBrowserSession("rotated-key", cookies[0].Value, time.Now()); err != nil {
		t.Errorf("the surviving cookie does not verify under the new key: %v", err)
	}
}

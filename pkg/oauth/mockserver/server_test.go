package mockserver

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func pkcePair(t *testing.T) (verifier, challenge string) {
	t.Helper()
	verifier = "test-verifier-with-sufficient-entropy-abcdef0123"
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return
}

func TestAuthorizationCodeExchange_Happy(t *testing.T) {
	srv := New()
	ts := httptest.NewServer(srv)
	defer ts.Close()

	verifier, challenge := pkcePair(t)
	code := srv.IssueCode(challenge)

	body, _ := json.Marshal(map[string]string{
		"grant_type":    "authorization_code",
		"code":          code,
		"client_id":     "9d1c250a-e61b-44d9-88ed-5944d1962f5e",
		"redirect_uri":  "urn:ietf:wg:oauth:2.0:oob",
		"code_verifier": verifier,
	})
	resp, err := http.Post(ts.URL+"/v1/oauth/token", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, raw)
	}
	var out struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if out.AccessToken == "" || out.RefreshToken == "" || out.ExpiresIn == 0 {
		t.Fatalf("unexpected response: %+v", out)
	}
}

func TestAuthorizationCodeExchange_InvalidVerifier(t *testing.T) {
	srv := New()
	ts := httptest.NewServer(srv)
	defer ts.Close()

	_, challenge := pkcePair(t)
	code := srv.IssueCode(challenge)

	body, _ := json.Marshal(map[string]string{
		"grant_type":    "authorization_code",
		"code":          code,
		"client_id":     "9d1c250a-e61b-44d9-88ed-5944d1962f5e",
		"redirect_uri":  "urn:ietf:wg:oauth:2.0:oob",
		"code_verifier": "wrong-verifier",
	})
	resp, err := http.Post(ts.URL+"/v1/oauth/token", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 400 {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestRefreshToken_Rotates(t *testing.T) {
	srv := New()
	ts := httptest.NewServer(srv)
	defer ts.Close()

	_, challenge := pkcePair(t)
	code := srv.IssueCode(challenge)
	first := doExchange(t, ts.URL, code, "test-verifier-with-sufficient-entropy-abcdef0123")
	v := url.Values{}
	v.Set("grant_type", "refresh_token")
	v.Set("refresh_token", first.RefreshToken)
	v.Set("client_id", "9d1c250a-e61b-44d9-88ed-5944d1962f5e")
	srv.SetRotateRefresh(true)
	resp, _ := http.PostForm(ts.URL+"/v1/oauth/token", v)
	var out struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if out.RefreshToken == first.RefreshToken {
		t.Fatalf("expected rotated refresh token, got same")
	}
	if out.AccessToken == first.AccessToken {
		t.Fatalf("expected new access token")
	}
}

type exchangeResp struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
}

func doExchange(t *testing.T, base, code, verifier string) exchangeResp {
	t.Helper()
	body, _ := json.Marshal(map[string]string{
		"grant_type":    "authorization_code",
		"code":          code,
		"client_id":     "9d1c250a-e61b-44d9-88ed-5944d1962f5e",
		"redirect_uri":  "urn:ietf:wg:oauth:2.0:oob",
		"code_verifier": verifier,
	})
	resp, err := http.Post(base+"/v1/oauth/token", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	var out exchangeResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestSetFailMode_NotConsumedByWrongGrant(t *testing.T) {
	srv := New()
	ts := httptest.NewServer(srv)
	defer ts.Close()

	_, challenge := pkcePair(t)
	verifier := "test-verifier-with-sufficient-entropy-abcdef0123"
	code := srv.IssueCode(challenge)

	// Set a refresh-specific failure mode.
	srv.SetFailMode("expired_refresh")

	// A code exchange should succeed — the mode must NOT be consumed here.
	first := doExchange(t, ts.URL, code, verifier)
	if first.AccessToken == "" || first.RefreshToken == "" {
		t.Fatalf("code exchange should succeed with expired_refresh mode set, got %+v", first)
	}

	// Now the refresh call should see the still-pending expired_refresh mode.
	v := url.Values{}
	v.Set("grant_type", "refresh_token")
	v.Set("refresh_token", first.RefreshToken)
	v.Set("client_id", "9d1c250a-e61b-44d9-88ed-5944d1962f5e")
	resp, err := http.PostForm(ts.URL+"/v1/oauth/token", v)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 400 {
		t.Fatalf("expected 400 from refresh with expired_refresh mode, got %d", resp.StatusCode)
	}
}

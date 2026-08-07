package oauth_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/matty-v/kyber/pkg/oauth"
	"github.com/matty-v/kyber/pkg/oauth/mockserver"
)

func pkce() (verifier, challenge string) {
	verifier = "plan-1-2-verifier-with-sufficient-entropy"
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return
}

func TestExchangeAuthorizationCode_Happy(t *testing.T) {
	srv := mockserver.New()
	ts := httptest.NewServer(srv)
	defer ts.Close()
	verifier, challenge := pkce()
	code := srv.IssueCode(challenge)

	c := oauth.NewClient(ts.URL + "/v1/oauth/token")
	tok, err := c.ExchangeAuthorizationCode(context.Background(), code, verifier, "test-state")
	if err != nil {
		t.Fatal(err)
	}
	if tok.AccessToken == "" || tok.RefreshToken == "" {
		t.Fatalf("empty tokens: %+v", tok)
	}
}

func TestExchangeAuthorizationCode_BadCode(t *testing.T) {
	srv := mockserver.New()
	ts := httptest.NewServer(srv)
	defer ts.Close()
	c := oauth.NewClient(ts.URL + "/v1/oauth/token")
	_, err := c.ExchangeAuthorizationCode(context.Background(), "never-issued", "verifier", "test-state")
	if err == nil {
		t.Fatal("expected error")
	}
	if !oauth.IsInvalidGrant(err) {
		t.Fatalf("expected invalid_grant, got %v", err)
	}
}

func TestIsInvalidGrant_TransportError(t *testing.T) {
	if oauth.IsInvalidGrant(errors.New("boom")) {
		t.Fatal("expected false for plain transport error, got true")
	}
}

func TestRefreshAccessToken(t *testing.T) {
	srv := mockserver.New()
	ts := httptest.NewServer(srv)
	defer ts.Close()
	verifier, challenge := pkce()
	code := srv.IssueCode(challenge)
	c := oauth.NewClient(ts.URL + "/v1/oauth/token")
	first, err := c.ExchangeAuthorizationCode(context.Background(), code, verifier, "test-state")
	if err != nil {
		t.Fatal(err)
	}
	srv.SetRotateRefresh(true)
	refreshed, err := c.RefreshAccessToken(context.Background(), first.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.RefreshToken == first.RefreshToken {
		t.Fatal("expected rotation")
	}
}

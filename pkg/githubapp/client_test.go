package githubapp_test

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/matty-v/kyber/pkg/githubapp"
)

// newTestKey returns a fresh 2048-bit RSA key. 2048 is the minimum GitHub
// accepts for App keys; smaller keys would speed tests but wouldn't match
// production key sizes.
func newTestKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating test key: %v", err)
	}
	return k
}

func pkcs1PEM(t *testing.T, k *rsa.PrivateKey) []byte {
	t.Helper()
	return pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(k),
	})
}

func pkcs8PEM(t *testing.T, k *rsa.PrivateKey) []byte {
	t.Helper()
	b, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		t.Fatalf("marshal PKCS8: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: b})
}

// verifyJWT asserts the JWT's signature is valid against pub and returns
// the decoded payload so tests can inspect claims.
func verifyJWT(t *testing.T, tok string, pub *rsa.PublicKey) map[string]any {
	t.Helper()
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("expected 3 JWT parts, got %d", len(parts))
	}
	headerBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatalf("decode header: %v", err)
	}
	var header map[string]string
	if err := json.Unmarshal(headerBytes, &header); err != nil {
		t.Fatalf("unmarshal header: %v", err)
	}
	if header["alg"] != "RS256" || header["typ"] != "JWT" {
		t.Fatalf("unexpected header: %v", header)
	}

	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("decode sig: %v", err)
	}
	signingInput := parts[0] + "." + parts[1]
	sum := sha256.Sum256([]byte(signingInput))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, sum[:], sig); err != nil {
		t.Fatalf("JWT signature invalid: %v", err)
	}

	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(payloadBytes, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	return payload
}

func TestConfigValidate(t *testing.T) {
	key := newTestKey(t)
	cases := []struct {
		name    string
		cfg     githubapp.Config
		wantErr string
	}{
		{"ok", githubapp.Config{AppID: 1, InstallationID: 2, PrivateKey: key}, ""},
		{"missing AppID", githubapp.Config{InstallationID: 2, PrivateKey: key}, "AppID is zero"},
		{"missing InstallationID", githubapp.Config{AppID: 1, PrivateKey: key}, "InstallationID is zero"},
		{"missing PrivateKey", githubapp.Config{AppID: 1, InstallationID: 2}, "PrivateKey is nil"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("want nil, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want %q, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestParsePrivateKey(t *testing.T) {
	key := newTestKey(t)
	t.Run("pkcs1", func(t *testing.T) {
		got, err := githubapp.ParsePrivateKey(pkcs1PEM(t, key))
		if err != nil {
			t.Fatal(err)
		}
		if got.N.Cmp(key.N) != 0 {
			t.Fatal("key mismatch")
		}
	})
	t.Run("pkcs8", func(t *testing.T) {
		got, err := githubapp.ParsePrivateKey(pkcs8PEM(t, key))
		if err != nil {
			t.Fatal(err)
		}
		if got.N.Cmp(key.N) != 0 {
			t.Fatal("key mismatch")
		}
	})
	t.Run("not pem", func(t *testing.T) {
		_, err := githubapp.ParsePrivateKey([]byte("not a pem"))
		if err == nil || !strings.Contains(err.Error(), "no PEM block") {
			t.Fatalf("want no-PEM error, got %v", err)
		}
	})
	t.Run("unsupported type", func(t *testing.T) {
		junk := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("x")})
		_, err := githubapp.ParsePrivateKey(junk)
		if err == nil || !strings.Contains(err.Error(), "unsupported") {
			t.Fatalf("want unsupported-type error, got %v", err)
		}
	})
}

// TestMintJWTClaims verifies the JWT handed to GitHub carries the claims
// the GitHub docs require: iss = App ID, iat ≈ now - backdate, exp ≈ now +
// ~9m. We assert via the httptest server so mintJWT stays unexported.
func TestMintJWTClaims(t *testing.T) {
	key := newTestKey(t)
	fixedNow := time.Unix(1_700_000_000, 0).UTC()

	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		writeTokenResponse(w, "test-token", fixedNow.Add(time.Hour))
	}))
	defer srv.Close()

	client, err := githubapp.NewClient(
		githubapp.Config{AppID: 424242, InstallationID: 7, PrivateKey: key},
		githubapp.WithBaseURL(srv.URL),
		githubapp.WithClock(func() time.Time { return fixedNow }),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.MintInstallationToken(context.Background()); err != nil {
		t.Fatal(err)
	}

	const prefix = "Bearer "
	if !strings.HasPrefix(gotAuth, prefix) {
		t.Fatalf("Authorization missing Bearer prefix: %q", gotAuth)
	}
	claims := verifyJWT(t, strings.TrimPrefix(gotAuth, prefix), &key.PublicKey)

	if got := claims["iss"]; got != "424242" {
		t.Errorf("iss: want \"424242\", got %v", got)
	}
	// iat backdated 60s; exp ~9m out. Allow a few seconds of slop though
	// the clock is fixed — JSON numbers decode to float64.
	iat := int64(claims["iat"].(float64))
	exp := int64(claims["exp"].(float64))
	if want := fixedNow.Add(-60 * time.Second).Unix(); iat != want {
		t.Errorf("iat: want %d, got %d", want, iat)
	}
	if want := fixedNow.Add(8 * time.Minute).Unix(); exp != want {
		t.Errorf("exp: want %d, got %d", want, exp)
	}
	// Guard against a future refactor that removes the backdate: iat must
	// be strictly in the past relative to the issuer's clock, otherwise
	// GitHub rejects the JWT with "not_before" errors on hosts with even
	// microseconds of forward clock skew.
	if iat >= fixedNow.Unix() {
		t.Errorf("iat must be strictly in the past: iat=%d fixedNow=%d", iat, fixedNow.Unix())
	}
}

func TestMintInstallationToken_Success(t *testing.T) {
	key := newTestKey(t)
	expiresAt := time.Now().Add(time.Hour).UTC().Truncate(time.Second)

	var gotPath, gotMethod, gotAPIVersion string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		gotAPIVersion = r.Header.Get("X-GitHub-Api-Version")
		writeTokenResponse(w, "ghs_test_abc", expiresAt)
	}))
	defer srv.Close()

	c, err := githubapp.NewClient(
		githubapp.Config{AppID: 1, InstallationID: 99, PrivateKey: key},
		githubapp.WithBaseURL(srv.URL),
	)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := c.MintInstallationToken(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method: want POST, got %s", gotMethod)
	}
	if gotPath != "/app/installations/99/access_tokens" {
		t.Errorf("path: got %s", gotPath)
	}
	if gotAPIVersion != "2022-11-28" {
		t.Errorf("api version header: got %q", gotAPIVersion)
	}
	if tok.Token != "ghs_test_abc" {
		t.Errorf("token: got %q", tok.Token)
	}
	if !tok.ExpiresAt.Equal(expiresAt) {
		t.Errorf("expiresAt: want %v, got %v", expiresAt, tok.ExpiresAt)
	}
}

func TestMintScopedToken_SendsScopeBody(t *testing.T) {
	key := newTestKey(t)
	expiresAt := time.Now().Add(time.Hour).UTC().Truncate(time.Second)

	var gotBody map[string]any
	var gotCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCT = r.Header.Get("Content-Type")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		writeTokenResponse(w, "ghs_scoped_xyz", expiresAt)
	}))
	defer srv.Close()

	c, err := githubapp.NewClient(
		githubapp.Config{AppID: 1, InstallationID: 99, PrivateKey: key},
		githubapp.WithBaseURL(srv.URL),
	)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := c.MintScopedToken(context.Background(), []string{"dave-agent"}, map[string]string{"contents": "write"})
	if err != nil {
		t.Fatal(err)
	}
	if tok.Token != "ghs_scoped_xyz" {
		t.Errorf("token: got %q", tok.Token)
	}
	if gotCT != "application/json" {
		t.Errorf("content-type: want application/json, got %q", gotCT)
	}
	repos, _ := gotBody["repositories"].([]any)
	if len(repos) != 1 || repos[0] != "dave-agent" {
		t.Errorf("repositories: want [dave-agent], got %v", gotBody["repositories"])
	}
	perms, _ := gotBody["permissions"].(map[string]any)
	if perms["contents"] != "write" {
		t.Errorf("permissions.contents: want write, got %v", gotBody["permissions"])
	}
}

func TestMintScopedToken_RejectsEmptyScope(t *testing.T) {
	c, _ := githubapp.NewClient(
		githubapp.Config{AppID: 1, InstallationID: 99, PrivateKey: newTestKey(t)},
	)
	if _, err := c.MintScopedToken(context.Background(), nil, nil); err == nil {
		t.Fatal("MintScopedToken(nil, nil): want error, got nil")
	}
}

func TestMintInstallationToken_SendsEmptyBody(t *testing.T) {
	// Regression guard: the unscoped mint must keep POSTing an empty body with
	// Content-Length: 0 (the corp-proxy 411 mitigation) — the scoped-token
	// refactor must not add a body to the unscoped path.
	var gotLen int64
	var gotCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotLen = r.ContentLength
		gotCT = r.Header.Get("Content-Type")
		writeTokenResponse(w, "ghs_x", time.Now().Add(time.Hour))
	}))
	defer srv.Close()
	c, _ := githubapp.NewClient(
		githubapp.Config{AppID: 1, InstallationID: 99, PrivateKey: newTestKey(t)},
		githubapp.WithBaseURL(srv.URL),
	)
	if _, err := c.MintInstallationToken(context.Background()); err != nil {
		t.Fatal(err)
	}
	if gotLen != 0 {
		t.Errorf("unscoped mint Content-Length: want 0, got %d", gotLen)
	}
	if gotCT != "" {
		t.Errorf("unscoped mint should send no Content-Type, got %q", gotCT)
	}
}

func TestMintInstallationToken_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"Bad credentials"}`))
	}))
	defer srv.Close()

	c, _ := githubapp.NewClient(
		githubapp.Config{AppID: 1, InstallationID: 99, PrivateKey: newTestKey(t)},
		githubapp.WithBaseURL(srv.URL),
	)
	_, err := c.MintInstallationToken(context.Background())
	if err == nil || !strings.Contains(err.Error(), "HTTP 401") {
		t.Fatalf("want HTTP 401 error, got %v", err)
	}
}

func TestMintInstallationToken_MissingToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"expires_at":"2026-01-01T00:00:00Z"}`))
	}))
	defer srv.Close()

	c, _ := githubapp.NewClient(
		githubapp.Config{AppID: 1, InstallationID: 99, PrivateKey: newTestKey(t)},
		githubapp.WithBaseURL(srv.URL),
	)
	_, err := c.MintInstallationToken(context.Background())
	if err == nil || !strings.Contains(err.Error(), "missing token") {
		t.Fatalf("want missing-token error, got %v", err)
	}
}

func TestMintInstallationToken_MalformedJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`not actually json`))
	}))
	defer srv.Close()

	c, _ := githubapp.NewClient(
		githubapp.Config{AppID: 1, InstallationID: 99, PrivateKey: newTestKey(t)},
		githubapp.WithBaseURL(srv.URL),
	)
	_, err := c.MintInstallationToken(context.Background())
	if err == nil || !strings.Contains(err.Error(), "decoding access_tokens response") {
		t.Fatalf("want decoding error, got %v", err)
	}
}

func TestRevokeInstallationToken(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		wantErr bool
	}{
		{"204", http.StatusNoContent, false},
		{"404 treated as success", http.StatusNotFound, false},
		{"401 is error", http.StatusUnauthorized, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotPath, gotMethod, gotAuth string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				gotMethod = r.Method
				gotAuth = r.Header.Get("Authorization")
				w.WriteHeader(tc.status)
			}))
			defer srv.Close()

			c, _ := githubapp.NewClient(
				githubapp.Config{AppID: 1, InstallationID: 1, PrivateKey: newTestKey(t)},
				githubapp.WithBaseURL(srv.URL),
			)
			err := c.RevokeInstallationToken(context.Background(), "ghs_abc")
			if tc.wantErr && err == nil {
				t.Fatalf("want error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("want nil, got %v", err)
			}
			if gotMethod != http.MethodDelete {
				t.Errorf("method: want DELETE, got %s", gotMethod)
			}
			if gotPath != "/installation/token" {
				t.Errorf("path: got %s", gotPath)
			}
			if gotAuth != "Bearer ghs_abc" {
				t.Errorf("auth: got %q", gotAuth)
			}
		})
	}
}

func TestRevokeInstallationToken_EmptyToken(t *testing.T) {
	c, _ := githubapp.NewClient(
		githubapp.Config{AppID: 1, InstallationID: 1, PrivateKey: newTestKey(t)},
	)
	if err := c.RevokeInstallationToken(context.Background(), ""); err == nil {
		t.Fatal("want error for empty token")
	}
}

// writeTokenResponse writes the minimal access_tokens JSON body GitHub
// returns on 201.
func writeTokenResponse(w http.ResponseWriter, token string, expires time.Time) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	body, _ := json.Marshal(map[string]any{
		"token":                token,
		"expires_at":           expires.Format(time.RFC3339),
		"permissions":          map[string]string{"contents": "write", "metadata": "read"},
		"repository_selection": "all",
	})
	fmt.Fprint(w, string(body))
}

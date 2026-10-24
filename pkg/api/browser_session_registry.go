package api

import (
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// V2 is bound to the individual credential's material and stable identity.
// Grants and role are never trusted from cookie claims. The bounded unverified
// credential ID is only a lookup hint; HMAC verification precedes acceptance.
type registryBrowserClaims struct {
	CredentialID string `json:"c"`
	PrincipalID  string `json:"p"`
	TenantID     string `json:"t"`
	Generation   uint64 `json:"g"`
	IssuedAt     int64  `json:"iat"`
	ExpiresAt    int64  `json:"exp"`
}

func registrySessionKey(material string) ([]byte, error) {
	if material == "" {
		return nil, errNoBrowserSessionKey
	}
	return hkdf.Key(sha256.New, []byte(material), nil, "kyber-browser-session-v2", 32)
}

func (a *registryCredentials) createBrowserSession(caller Caller, now time.Time) (string, error) {
	entry, ok := a.byID[caller.CredentialID]
	if !ok || !sameCredentialIdentity(entry.caller, caller) {
		return "", errSessionExpired("credential no longer valid")
	}
	key, err := registrySessionKey(entry.key)
	if err != nil {
		return "", err
	}
	raw, err := json.Marshal(registryBrowserClaims{CredentialID: caller.CredentialID, PrincipalID: caller.PrincipalID, TenantID: caller.TenantID, Generation: caller.CredentialGeneration, IssuedAt: now.Unix(), ExpiresAt: now.Add(browserSessionTTL).Unix()})
	if err != nil {
		return "", errMalformedBrowserSession
	}
	signed := "v2." + base64.RawURLEncoding.EncodeToString(raw)
	return signed + "." + base64.RawURLEncoding.EncodeToString(browserSessionMAC(key, signed)), nil
}

func (a *registryCredentials) browserCaller(token string, now time.Time) (*Caller, time.Time, error) {
	if len(token) > 4096 {
		return nil, time.Time{}, errMalformedBrowserSession
	}
	if strings.HasPrefix(token, "v1.") {
		return a.compatibleBrowserCaller(token, now)
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != "v2" {
		return nil, time.Time{}, errMalformedBrowserSession
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, time.Time{}, errMalformedBrowserSession
	}
	var claims registryBrowserClaims
	if json.Unmarshal(raw, &claims) != nil {
		return nil, time.Time{}, errMalformedBrowserSession
	}
	entry, ok := a.byID[claims.CredentialID]
	if !ok {
		return nil, time.Time{}, errMalformedBrowserSession
	}
	key, err := registrySessionKey(entry.key)
	if err != nil {
		return nil, time.Time{}, errMalformedBrowserSession
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || !hmac.Equal(signature, browserSessionMAC(key, parts[0]+"."+parts[1])) {
		return nil, time.Time{}, errMalformedBrowserSession
	}
	expiry := time.Unix(claims.ExpiresAt, 0)
	if claims.PrincipalID != entry.caller.PrincipalID || claims.TenantID != entry.caller.TenantID || claims.Generation != entry.caller.CredentialGeneration || !now.Before(expiry) || claims.IssuedAt < 946684800 || claims.IssuedAt > now.Add(time.Minute).Unix() || claims.ExpiresAt <= claims.IssuedAt || claims.ExpiresAt-claims.IssuedAt > int64(browserSessionTTL/time.Second) {
		return nil, time.Time{}, errMalformedBrowserSession
	}
	caller := entry.caller
	return &caller, expiry, nil
}

// V1 compatibility is a reader only. Its signature and material binding are
// checked against current registry keys, then current grants are returned.
// The historical FullScope bit selects only the preserved installation record;
// it never grants authority or selects another administrator by display name.
func (a *registryCredentials) compatibleBrowserCaller(token string, now time.Time) (*Caller, time.Time, error) {
	claims, err := verifyBrowserSessionWithSigningKey(a.compatibilityKey, token, now)
	if err != nil {
		return nil, time.Time{}, errMalformedBrowserSession
	}
	if claims.FullScope {
		entry, ok := a.byID[installationAdminCredentialID]
		if !ok || entry.caller.PrincipalID != installationAdminPrincipalID || entry.caller.TenantID != installationTenantID || entry.caller.CredentialGeneration != a.compatibilityGeneration || a.legacyGenerations[entry.caller.CredentialID] != entry.caller.CredentialGeneration {
			return nil, time.Time{}, errMalformedBrowserSession
		}
		if claims.PrincipalID != "" && claims.PrincipalID != installationAdminPrincipalID {
			return nil, time.Time{}, errMalformedBrowserSession
		}
		if claims.CredentialID != "" && claims.CredentialID != installationAdminCredentialID && claims.CredentialID != "credential_legacy_shared_key" {
			return nil, time.Time{}, errMalformedBrowserSession
		}
		if (claims.CredentialGeneration == 0 && entry.caller.CredentialGeneration != 1) || (claims.CredentialGeneration != 0 && claims.CredentialGeneration != entry.caller.CredentialGeneration) {
			return nil, time.Time{}, errMalformedBrowserSession
		}
		caller := entry.caller
		return &caller, claims.expiresAt(), nil
	}
	var found *Caller
	for _, entry := range a.entries {
		if a.legacyGenerations[entry.caller.CredentialID] == 0 || a.legacyGenerations[entry.caller.CredentialID] != entry.caller.CredentialGeneration {
			continue
		}
		if claims.PrincipalID != "" {
			if claims.PrincipalID != entry.caller.PrincipalID || claims.CredentialID != entry.caller.CredentialID || claims.CredentialGeneration != entry.caller.CredentialGeneration {
				continue
			}
		}
		// Pre-identity V1 cookies resolve by their unique current material binding,
		// so renaming the credential does not silently sign it out.
		binding := base64.RawURLEncoding.EncodeToString(browserSessionMAC(a.compatibilityKey, "caller-key:"+entry.key)[:16])
		if !hmac.Equal([]byte(binding), []byte(claims.KeyBinding)) {
			continue
		}
		if found != nil {
			return nil, time.Time{}, errMalformedBrowserSession
		}
		caller := entry.caller
		found = &caller
	}
	if found == nil {
		return nil, time.Time{}, errMalformedBrowserSession
	}
	return found, claims.expiresAt(), nil
}

func (a *registryCredentials) renewBrowserSession(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "" || websocket.IsWebSocketUpgrade(r) {
		return
	}
	cookie, err := r.Cookie(browserSessionCookie)
	if err != nil {
		return
	}
	caller, expiry, err := a.browserCaller(cookie.Value, time.Now())
	if err != nil {
		return
	}
	// Upgrade old cookies on their next successful HTTP request. Their old
	// signing dependency on the installation key does not persist after renewal.
	if !strings.HasPrefix(cookie.Value, "v1.") && time.Until(expiry) > browserSessionRenewAfter {
		return
	}
	token, err := a.createBrowserSession(*caller, time.Now())
	if err == nil {
		setBrowserSessionCookie(w, r, token)
	}
}

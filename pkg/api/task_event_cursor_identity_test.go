package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func TestAdministratorIdentityAndExistingTaskCursor(t *testing.T) {
	const key = "administrator-cursor-test-key"
	a := NewAPIKeyAuthenticator(key)
	caller, err := a.Authenticate(reqWithBearer(key))
	if err != nil {
		t.Fatal(err)
	}
	if caller.Name != "installation-admin" || strings.Contains(caller.Subject+caller.CredentialID, "legacy") {
		t.Fatal("new runtime still emits the retired administrator identity")
	}
	s := &Server{auth: a, Namespace: "kyber-system"}
	r := reqWithCaller(caller)
	current, err := s.encodeTaskEventCursor(r, "alpha", "task_test", 7)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(current, ".")
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var claims taskEventCursorClaims
	if err := json.Unmarshal(raw, &claims); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(claims.CredentialID, "legacy") {
		t.Fatal("new task cursor emits retired identity")
	}
	claims.CredentialID = "credential_legacy_shared_key"
	raw, _ = json.Marshal(claims)
	body := base64.RawURLEncoding.EncodeToString(raw)
	mac := hmac.New(sha256.New, []byte(key))
	_, _ = mac.Write([]byte("kyber-task-event-cursor-v1." + body))
	old := "1." + body + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if sequence, err := s.decodeTaskEventCursor(r, "alpha", "task_test", old); err != nil || sequence != 7 {
		t.Fatalf("existing signed administrator cursor was orphaned: sequence=%d err=%v", sequence, err)
	}
	spoof := &Caller{Name: "installation-admin", PrincipalID: caller.PrincipalID, TenantID: caller.TenantID, CredentialID: caller.CredentialID, Scopes: newScopeSet(ScopeTasksRead)}
	if taskCursorCredentialMatches("credential_legacy_shared_key", spoof) {
		t.Fatal("restricted caller gained administrator alias through its name or asserted identity")
	}
}

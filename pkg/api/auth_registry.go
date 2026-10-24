package api

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/matty-v/kyber/pkg/operatorcredentials"
)

// OperatorCredentialAuthority reads current installation-owned credential state.
// Only ErrUninitialized means bootstrap configuration is still authoritative.
// Implementations must not cache accepted state or hide a read outage as missing.
type OperatorCredentialAuthority interface {
	Load(context.Context) (operatorcredentials.Snapshot, error)
}

var errOperatorAuthorityUnavailable = errors.New("operator authentication unavailable")

type registryCredentials struct {
	entries          []scopedKey
	byID             map[string]scopedKey
	compatibilityKey []byte
	inventory        []operatorCredentialView
}

// resolve chooses one authority for this operation, never a union of configured
// and registry keys. No last-good snapshot is retained on read/compile failure.
func (a *APIKeyAuthenticator) resolve(ctx context.Context) (*APIKeyAuthenticator, error) {
	if a.authority == nil {
		return a, nil
	}
	snapshot, err := a.authority.Load(ctx)
	if err == operatorcredentials.ErrUninitialized {
		if a.authorityObserved.Load() {
			return nil, errOperatorAuthorityUnavailable
		}
		return a, nil
	}
	if err != nil {
		return nil, errOperatorAuthorityUnavailable
	}
	a.authorityObserved.Store(true)
	if snapshot.Version == "" || snapshot.Document.Namespace != a.namespace {
		return nil, errOperatorAuthorityUnavailable
	}
	registry, err := compileRegistryCredentials(snapshot.Document, time.Now())
	if err != nil {
		return nil, errOperatorAuthorityUnavailable
	}
	return &APIKeyAuthenticator{registry: registry}, nil
}

func compileRegistryCredentials(document operatorcredentials.Document, now time.Time) (*registryCredentials, error) {
	if err := document.Validate(); err != nil {
		return nil, errOperatorAuthorityUnavailable
	}
	registry := &registryCredentials{byID: map[string]scopedKey{}, compatibilityKey: document.LegacyBrowserSigningKey()}
	for _, credential := range document.Credentials {
		// Validate supported permissions even for inactive records; do not silently
		// reinterpret an unsupported registry after a downgrade.
		scopes := make([]Scope, 0, len(credential.Scopes))
		for _, grant := range credential.Scopes {
			if !validScope(Scope(grant)) {
				return nil, errOperatorAuthorityUnavailable
			}
			scopes = append(scopes, Scope(grant))
		}
		state := "accepted"
		if credential.State == operatorcredentials.StateRevoked {
			state = "revoked"
		} else if credential.ExpiresAt != nil && !now.Before(*credential.ExpiresAt) {
			state = "expired"
		}
		view := operatorCredentialView{ID: credential.ID, Name: credential.Name, DisplayName: credential.Name, AccessLevel: credential.Role, State: state, Source: credential.Source.Kind, Scopes: append([]string{}, credential.Scopes...), AgentResources: append([]string{}, credential.AgentResources...), MachineResources: append([]string{}, credential.MachineResources...), Permissions: credentialPermissions(credential.Scopes)}
		if credential.Role == operatorcredentials.RoleAdministrator {
			view.AgentResources, view.MachineResources = []string{"*"}, []string{"*"}
			view.Permissions = []string{"Full access to this Kyber installation"}
		}
		registry.inventory = append(registry.inventory, view)
		if credential.State != operatorcredentials.StateActive || credential.ExpiresAt != nil && !now.Before(*credential.ExpiresAt) {
			continue
		}
		if !validOperatorHTTPKey(string(credential.SecretMaterial())) {
			return nil, errOperatorAuthorityUnavailable
		}

		caller := Caller{Name: credential.Name, PrincipalID: credential.PrincipalID, TenantID: credential.TenantID, Kind: credential.Kind, Issuer: credential.Issuer, Subject: credential.Subject, CredentialID: credential.ID, CredentialGeneration: credential.Generation, CredentialAuthority: "registry", Scopes: newScopeSet(scopes...), AgentResources: newResourceSet(credential.AgentResources...), MachineResources: newResourceSet(credential.MachineResources...)}
		if credential.Role == operatorcredentials.RoleAdministrator {
			caller.Scopes = newFullScopeSet()
			caller.AgentResources, caller.MachineResources = newResourceSet("*"), newResourceSet("*")
		}
		entry := scopedKey{key: string(credential.SecretMaterial()), caller: caller}
		registry.entries = append(registry.entries, entry)
		registry.byID[credential.ID] = entry
	}
	return registry, nil
}

func (a *registryCredentials) authenticate(r *http.Request) (*Caller, error) {
	if r.Header.Get("Authorization") == "" {
		if cookie, err := r.Cookie(browserSessionCookie); err == nil && cookie.Value != "" {
			caller, _, err := a.browserCaller(cookie.Value, time.Now())
			if err != nil {
				return nil, errSessionExpired("browser session expired or no longer valid — sign in again")
			}
			return caller, nil
		}
	}
	presented, err := presentedKey(r)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(presented) == "" {
		return nil, errUnauthorized("missing API key")
	}
	var matched *Caller
	for _, entry := range a.entries {
		if subtle.ConstantTimeCompare([]byte(presented), []byte(entry.key)) == 1 {
			caller := entry.caller
			matched = &caller
		}
	}
	if matched == nil {
		return nil, errUnauthorized("invalid API key")
	}
	return matched, nil
}

// credentialKey re-resolves by immutable identity, not name or full-scope role.
// Managed administrators must never borrow the installation key for cursors.
func (a *APIKeyAuthenticator) credentialKey(ctx context.Context, caller *Caller) (string, error) {
	if caller == nil {
		return "", errUnauthorized("missing caller")
	}
	resolved, err := a.resolve(ctx)
	if err != nil {
		return "", err
	}
	if resolved.registry != nil {
		entry, ok := resolved.registry.byID[caller.CredentialID]
		if !ok || !sameCredentialIdentity(entry.caller, *caller) {
			return "", errUnauthorized("credential no longer valid")
		}
		return entry.key, nil
	}
	if caller.Scopes.full {
		return resolved.currentKey(), nil
	}
	key, ok := resolved.scopedCallerKey(caller.Name)
	if !ok {
		return "", errUnauthorized("credential no longer valid")
	}
	return key, nil
}

func sameCredentialIdentity(current, presented Caller) bool {
	return current.PrincipalID == presented.PrincipalID && current.TenantID == presented.TenantID && current.CredentialID == presented.CredentialID && current.CredentialGeneration == presented.CredentialGeneration
}

func writeOperatorAuthenticationUnavailable(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSONError(w, http.StatusServiceUnavailable, "authentication_unavailable", "operator authentication is temporarily unavailable")
}

// Go's HTTP parser rejects controls and strips trailing optional whitespace.
// Such material cannot be a usable bearer credential, even if the storage
// codec correctly preserves its bytes. Reject it before authority activation.
func validOperatorHTTPKey(key string) bool {
	if len(key) == 0 || key[len(key)-1] == ' ' || key[len(key)-1] == '\t' {
		return false
	}
	for i := 0; i < len(key); i++ {
		if key[i] == 0x7f || key[i] < 0x20 && key[i] != '\t' {
			return false
		}
	}
	return true
}

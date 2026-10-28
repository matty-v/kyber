package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/matty-v/kyber/pkg/operatorcredentials"
)

var errCredentialAdoptionCandidate = errors.New("configured credentials cannot be adopted without review")

// This private candidate never activates authority or writes storage. A future
// administrator flow must reserve maintenance, verify replicas/source ownership,
// persist required admission and reconcile writes before binding the marker.
// Generated identities become durable only when that flow commits the candidate.
type credentialAdoptionCandidate struct {
	document            operatorcredentials.Document
	shadowedDefinitions int
	identityAssignments int
	browserReconnects   int
}

func (credentialAdoptionCandidate) String() string     { return "credential adoption candidate [redacted]" }
func (c credentialAdoptionCandidate) GoString() string { return c.String() }
func (c credentialAdoptionCandidate) Format(s fmt.State, _ rune) {
	_, _ = io.WriteString(s, c.String())
}
func (credentialAdoptionCandidate) MarshalJSON() ([]byte, error) {
	return nil, operatorcredentials.ErrSensitive
}
func (*credentialAdoptionCandidate) UnmarshalJSON([]byte) error {
	return operatorcredentials.ErrSensitive
}

// prepareCredentialAdoption snapshots current accepted configuration. Duplicate
// material keeps the authenticator's final matching caller/root precedence,
// never a union of grants. Existing stable identities and generations survive;
// missing fields receive fresh IDs, never another consumer's task ownership.
func (a *APIKeyAuthenticator) prepareCredentialAdoption(ctx context.Context, namespace string, operation operatorcredentials.Operation) (*credentialAdoptionCandidate, error) {
	resolved, err := a.resolve(ctx)
	if err != nil {
		return nil, errOperatorAuthorityUnavailable
	}
	if resolved != a || a.registry != nil || a.authorityObserved.Load() {
		return nil, errCredentialAdoptionCandidate
	}
	a.mu.RLock()
	root := a.key
	entries := make([]scopedKey, len(a.callers))
	for i, entry := range a.callers {
		entry.caller.Scopes.scopes = maps.Clone(entry.caller.Scopes.scopes)
		entry.caller.AgentResources.resources = maps.Clone(entry.caller.AgentResources.resources)
		entry.caller.MachineResources.resources = maps.Clone(entry.caller.MachineResources.resources)
		entries[i] = entry
	}
	a.mu.RUnlock()
	operationUUID, uuidErr := uuid.Parse(operation.ID)
	if uuidErr != nil || operationUUID == uuid.Nil || operationUUID.String() != operation.ID {
		return nil, errCredentialAdoptionCandidate
	}
	if !validOperatorHTTPKey(root) || operation.Action != "adopt" || operation.TargetCredentialID != "" || operation.Status != "pending" || operation.ActorPrincipalID != installationAdminPrincipalID || operation.ActorCredentialID != installationAdminCredentialID || operation.ActorGeneration != 1 || operation.Authority != "configuration" {
		return nil, errCredentialAdoptionCandidate
	}
	signingKey, err := browserSessionSigningKey(root)
	if err != nil {
		return nil, errCredentialAdoptionCandidate
	}
	candidate := &credentialAdoptionCandidate{}
	doc := operatorcredentials.Document{SchemaVersion: operatorcredentials.SchemaVersion, Namespace: namespace, Operations: []operatorcredentials.Operation{operation}}
	doc = doc.WithLegacyBrowserSigningKey(signingKey, 1, operation.At.Add(time.Minute), operation.At.Add(time.Minute+browserSessionTTL))
	admin := installationAdminCaller()
	doc.Credentials = append(doc.Credentials, (operatorcredentials.Credential{
		ID: admin.CredentialID, PrincipalID: admin.PrincipalID, TenantID: admin.TenantID, Kind: admin.Kind, Issuer: admin.Issuer, Subject: admin.Subject,
		Name: "Installation administrator", Role: operatorcredentials.RoleAdministrator, State: operatorcredentials.StateActive, Generation: 1, PermissionRevision: 1,
		Source: operatorcredentials.Source{Kind: "installation"},
	}).WithSecretMaterial([]byte(root)).WithLegacyBrowserGeneration(1))
	usedNames := map[string]bool{"installation administrator": true}
	// Snapshot definitions may repeat one value many times. Linear-time
	// precedence selection avoids a quadratic scan of configuration material.
	keep := make([]bool, len(entries))
	seen := map[string]bool{root: true}
	for i := len(entries) - 1; i >= 0; i-- {
		if seen[entries[i].key] {
			candidate.shadowedDefinitions++
			continue
		}
		if len(seen) >= operatorcredentials.MaxCredentials {
			return nil, errors.Join(errCredentialAdoptionCandidate, operatorcredentials.ErrCapacity)
		}
		seen[entries[i].key] = true
		keep[i] = true
	}
	for i, entry := range entries {
		if !keep[i] {
			continue
		}
		if !validOperatorHTTPKey(entry.key) {
			return nil, errCredentialAdoptionCandidate
		}
		caller := entry.caller
		if caller.PrincipalID != "" && (caller.CredentialID == "" || caller.CredentialGeneration == 0) {
			candidate.browserReconnects++
		}
		assigned := false
		// These are identity labels, never credentials. Derive only absent
		// fields from the original admitted operation and source position so
		// recovery can reconstruct the identical candidate without storing
		// material, material digests or another private candidate copy.
		// The caller must separately verify the unchanged frozen source graph.
		importedID := func(purpose string) string {
			return purpose + "_imported_" + uuid.NewSHA1(operationUUID, []byte(fmt.Sprintf("kyber-credential-adoption/v1/%d/%s", i, purpose))).String()
		}
		if caller.PrincipalID == "" {
			caller.PrincipalID = importedID("principal")
			assigned = true
		}
		if caller.TenantID == "" {
			caller.TenantID = importedID("tenant")
			assigned = true
		}
		if caller.CredentialID == "" {
			caller.CredentialID = importedID("credential")
			assigned = true
		}
		if caller.CredentialGeneration == 0 {
			caller.CredentialGeneration = 1
			assigned = true
		}
		if assigned {
			candidate.identityAssignments++
		}
		role := operatorcredentials.RoleRestricted
		scopes := make([]string, 0, len(caller.Scopes.scopes))
		for scope, enabled := range caller.Scopes.scopes {
			if enabled {
				scopes = append(scopes, string(scope))
			}
		}
		sort.Strings(scopes)
		if caller.Scopes.full {
			role = operatorcredentials.RoleAdministrator
			scopes = []string{}
		}
		credential := operatorcredentials.Credential{
			ID: caller.CredentialID, PrincipalID: caller.PrincipalID, TenantID: caller.TenantID, Kind: caller.Kind, Issuer: caller.Issuer, Subject: caller.Subject,
			Name: adoptionCredentialName(caller.Name, i+1, usedNames), Role: role, State: operatorcredentials.StateActive, Generation: caller.CredentialGeneration, PermissionRevision: 1,
			Scopes: scopes, AgentResources: sortedResources(caller.AgentResources), MachineResources: sortedResources(caller.MachineResources),
			Source: operatorcredentials.Source{Kind: "managed"},
		}
		doc.Credentials = append(doc.Credentials, credential.WithSecretMaterial([]byte(entry.key)).WithLegacyBrowserGeneration(caller.CredentialGeneration))
	}
	if _, err := compileRegistryCredentials(doc, time.Now()); err != nil {
		return nil, errCredentialAdoptionCandidate
	}
	candidate.document = doc.Clone()
	return candidate, nil
}

func adoptionCredentialName(name string, index int, used map[string]bool) string {
	name = strings.TrimSpace(name)
	valid := name != "" && len(name) <= 256 && utf8.ValidString(name) && !strings.EqualFold(name, "legacy")
	for _, r := range name {
		if !unicode.IsPrint(r) || unicode.IsControl(r) {
			valid = false
		}
	}
	if !valid {
		name = fmt.Sprintf("Imported credential %d", index)
	}
	base := name
	for suffix := 2; used[strings.ToLower(name)]; suffix++ {
		name = fmt.Sprintf("%s (%d)", base, suffix)
		if len(name) > 256 {
			name = fmt.Sprintf("Imported credential %d (%d)", index, suffix)
		}
	}
	used[strings.ToLower(name)] = true
	return name
}

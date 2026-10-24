package api

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/matty-v/kyber/pkg/operatorcredentials"
)

func mutationFixture(t *testing.T) (operatorcredentials.Snapshot, Caller) {
	t.Helper()
	document := registryFixtureDocument(t)
	registry, err := compileRegistryCredentials(document, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	actor, err := registry.authenticate(registryBearer("original-installation-fixture"))
	if err != nil {
		t.Fatal(err)
	}
	return operatorcredentials.Snapshot{UID: "fixture-registry-uid", Version: "fixture-version", Document: document}, *actor
}
func mutationFixtureKey() string {
	return base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{42}, 32))
}
func mutationFixturePolicy() *credentialMutationPolicy {
	return &credentialMutationPolicy{Name: "Build integration", Role: operatorcredentials.RoleRestricted, Scopes: []string{string(ScopeTasksRead)}, AgentResources: []string{"kyber-system/alpha"}}
}

func TestManagedCredentialCreatePreservesExistingIdentitiesAndPrivateMaterial(t *testing.T) {
	snapshot, actor := mutationFixture(t)
	before := snapshot.Document.Clone()
	request := credentialMutationRequest{OperationID: uuid.NewString(), Action: "create", Policy: mutationFixturePolicy(), Key: mutationFixtureKey()}
	next, operation, err := prepareManagedCredentialMutation(snapshot, actor, request, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, snapshot.Document.Clone()) {
		t.Fatal("candidate mutated current authority")
	}
	if len(next.Credentials) != len(before.Credentials)+1 || len(next.Operations) != 1 || operation.TargetCredentialID == "" {
		t.Fatal("creation omitted durable identity/outbox")
	}
	for _, prior := range before.Credentials {
		found := false
		for _, current := range next.Credentials {
			if current.ID == prior.ID {
				found = true
				if !reflect.DeepEqual(current, prior) {
					t.Fatal("creation changed existing credential")
				}
			}
		}
		if !found {
			t.Fatal("existing credential vanished")
		}
	}
	registry, err := compileRegistryCredentials(next, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	caller, err := registry.authenticate(registryBearer(request.Key))
	if err != nil || caller.CredentialID != operation.TargetCredentialID || caller.PrincipalID == actor.PrincipalID || caller.TenantID == actor.TenantID || caller.Scopes.full || !caller.Scopes.Has(ScopeTasksRead) {
		t.Fatal("new key has incorrect ownership or grants", err)
	}
	for _, format := range []string{"%v", "%+v", "%#v", "%x"} {
		if strings.Contains(fmt.Sprintf(format, request), request.Key) {
			t.Fatal("request diagnostics exposed key")
		}
	}
	if _, err := json.Marshal(request); !errors.Is(err, operatorcredentials.ErrSensitive) {
		t.Fatal("private request serialized", err)
	}
}

func TestManagedCredentialUpdateRotateRevokePreserveOwnership(t *testing.T) {
	for _, action := range []string{"update", "rotate", "revoke"} {
		t.Run(action, func(t *testing.T) {
			snapshot, actor := mutationFixture(t)
			var prior operatorcredentials.Credential
			for _, c := range snapshot.Document.Credentials {
				if c.ID == "https://issuer.invalid/credential/client" {
					prior = c
				}
			}
			request := credentialMutationRequest{OperationID: uuid.NewString(), Action: action, CredentialID: prior.ID}
			switch action {
			case "update":
				request.Policy = mutationFixturePolicy()
			case "rotate":
				request.Key = mutationFixtureKey()
			}
			next, _, err := prepareManagedCredentialMutation(snapshot, actor, request, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			var changed operatorcredentials.Credential
			for _, c := range next.Credentials {
				if c.ID == prior.ID {
					changed = c
				}
			}
			if changed.PrincipalID != prior.PrincipalID || changed.TenantID != prior.TenantID || changed.Kind != prior.Kind || changed.Issuer != prior.Issuer || changed.Subject != prior.Subject {
				t.Fatal("management changed task ownership/provenance")
			}
			if action == "rotate" {
				if changed.Generation != prior.Generation+1 || changed.PermissionRevision != prior.PermissionRevision {
					t.Fatal("rotation confused material and policy revisions")
				}
			} else if changed.Generation != prior.Generation || changed.PermissionRevision != prior.PermissionRevision+1 {
				t.Fatal("policy/revocation confused material and policy revisions")
			}
			registry, err := compileRegistryCredentials(next, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := registry.authenticate(registryBearer("client-key-fixture")); (action == "update") != (err == nil) {
				t.Fatal("old key acceptance incorrect")
			}
			if action == "rotate" {
				if _, err := registry.authenticate(registryBearer(request.Key)); err != nil {
					t.Fatal("rotated key rejected", err)
				}
			}
		})
	}
}

func TestManagedCredentialChangesRequireCurrentAdminAndProtectLastAdmin(t *testing.T) {
	snapshot, actor := mutationFixture(t)
	request := credentialMutationRequest{OperationID: uuid.NewString(), Action: "revoke", CredentialID: installationAdminCredentialID}
	for label, mutate := range map[string]func(*Caller){"stale generation": func(c *Caller) { c.CredentialGeneration++ }, "wrong tenant": func(c *Caller) { c.TenantID = "other" }, "configuration authority": func(c *Caller) { c.CredentialAuthority = "configuration" }} {
		t.Run(label, func(t *testing.T) {
			current := actor
			mutate(&current)
			if _, _, err := prepareManagedCredentialMutation(snapshot, current, request, time.Now()); !errors.Is(err, errCredentialMutationForbidden) {
				t.Fatal("stale or wrong authority accepted", err)
			}
		})
	}
	for i := range snapshot.Document.Credentials {
		if snapshot.Document.Credentials[i].ID == "other-admin" {
			snapshot.Document.Credentials[i].Role = operatorcredentials.RoleRestricted
			snapshot.Document.Credentials[i].Scopes = []string{string(ScopeTasksRead)}
		}
	}
	if _, _, err := prepareManagedCredentialMutation(snapshot, actor, request, time.Now()); !errors.Is(err, errCredentialMutationInvalid) {
		t.Fatal("last administrator revoked", err)
	}
	request.Action = "update"
	request.Policy = mutationFixturePolicy()
	if _, _, err := prepareManagedCredentialMutation(snapshot, actor, request, time.Now()); !errors.Is(err, errCredentialMutationInvalid) {
		t.Fatal("last administrator demoted", err)
	}
}

func TestManagedCredentialPolicyRejectsUnsupportedOrMisleadingInput(t *testing.T) {
	for label, mutate := range map[string]func(*credentialMutationRequest){
		"weak key":                        func(r *credentialMutationRequest) { r.Key = "short" },
		"reserved permission":             func(r *credentialMutationRequest) { r.Policy.Scopes = []string{string(ScopeTasksPlatformAdmin)} },
		"unknown permission":              func(r *credentialMutationRequest) { r.Policy.Scopes = []string{"invented:access"} },
		"wildcard targets":                func(r *credentialMutationRequest) { r.Policy.AgentResources = []string{"*"} },
		"other namespace":                 func(r *credentialMutationRequest) { r.Policy.AgentResources = []string{"another/alpha"} },
		"identity label":                  func(r *credentialMutationRequest) { r.Policy.Name = "legacy" },
		"expired":                         func(r *credentialMutationRequest) { at := time.Now().Add(-time.Hour); r.Policy.ExpiresAt = &at },
		"misleading administrator limits": func(r *credentialMutationRequest) { r.Policy.Role = operatorcredentials.RoleAdministrator },
		"provided creation target":        func(r *credentialMutationRequest) { r.CredentialID = "client-selected-id" },
	} {
		t.Run(label, func(t *testing.T) {
			snapshot, actor := mutationFixture(t)
			request := credentialMutationRequest{OperationID: uuid.NewString(), Action: "create", Key: mutationFixtureKey(), Policy: mutationFixturePolicy()}
			mutate(&request)
			if _, _, err := prepareManagedCredentialMutation(snapshot, actor, request, time.Now()); !errors.Is(err, errCredentialMutationInvalid) {
				t.Fatal("unsupported policy accepted", err)
			}
		})
	}
}

func TestManagedCredentialRenameDoesNotRotateOrReviseGrants(t *testing.T) {
	snapshot, actor := mutationFixture(t)
	var prior operatorcredentials.Credential
	for _, c := range snapshot.Document.Credentials {
		if c.ID == "https://issuer.invalid/credential/client" {
			prior = c
		}
	}
	request := credentialMutationRequest{OperationID: uuid.NewString(), Action: "update", CredentialID: prior.ID, Policy: &credentialMutationPolicy{Name: "Renamed integration", Role: prior.Role, Scopes: append([]string{}, prior.Scopes...), AgentResources: append([]string{}, prior.AgentResources...), MachineResources: append([]string{}, prior.MachineResources...)}}
	for i, j := 0, len(request.Policy.Scopes)-1; i < j; i, j = i+1, j-1 {
		request.Policy.Scopes[i], request.Policy.Scopes[j] = request.Policy.Scopes[j], request.Policy.Scopes[i]
	}
	next, _, err := prepareManagedCredentialMutation(snapshot, actor, request, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range next.Credentials {
		if c.ID == prior.ID && (c.Name != request.Policy.Name || c.Generation != prior.Generation || c.PermissionRevision != prior.PermissionRevision || string(c.SecretMaterial()) != string(prior.SecretMaterial())) {
			t.Fatal("cosmetic rename changed key or authority revision")
		}
	}
}

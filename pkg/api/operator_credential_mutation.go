package api

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/matty-v/kyber/pkg/operatorcredentials"
)

var errCredentialMutationInvalid = errors.New("credential change is invalid")
var errCredentialLastAdministrator = errors.New("a durable administrator must remain")
var errCredentialMutationForbidden = errors.New("current administrator access is required")

// Explicit input only: identities, provenance, source ownership, generations
// and creation timestamps cannot be assigned by a management client.
type credentialMutationPolicy struct {
	Name             string     `json:"name"`
	Role             string     `json:"role"`
	Scopes           []string   `json:"scopes"`
	AgentResources   []string   `json:"agentResources"`
	MachineResources []string   `json:"machineResources"`
	ExpiresAt        *time.Time `json:"expiresAt"`
}

type credentialMutationRequest struct {
	requestID    string
	OperationID  string                    `json:"operationId"`
	Action       string                    `json:"action"`
	CredentialID string                    `json:"credentialId"`
	Policy       *credentialMutationPolicy `json:"policy,omitempty"`
	// New material is prepared and saved by the client before activation. No
	// endpoint returns existing material or re-reveals it after a lost response.
	Key string `json:"key,omitempty"`
}

func (credentialMutationRequest) String() string               { return "credential mutation request [redacted]" }
func (r credentialMutationRequest) Format(s fmt.State, _ rune) { _, _ = io.WriteString(s, r.String()) }
func (credentialMutationRequest) MarshalJSON() ([]byte, error) {
	return nil, operatorcredentials.ErrSensitive
}

// prepareManagedCredentialMutation is pure: its candidate owns independent
// slices/material and cannot mutate live state. The executor must commit audit
// admission, recheck current authorization and use the original CAS version.
func prepareManagedCredentialMutation(snapshot operatorcredentials.Snapshot, actor Caller, request credentialMutationRequest, now time.Time) (operatorcredentials.Document, operatorcredentials.Operation, error) {
	invalid := func() (operatorcredentials.Document, operatorcredentials.Operation, error) {
		return operatorcredentials.Document{}, operatorcredentials.Operation{}, errCredentialMutationInvalid
	}
	operationID, err := uuid.Parse(request.OperationID)
	if err != nil || operationID.String() != request.OperationID || now.IsZero() || snapshot.UID == "" || snapshot.Version == "" {
		return invalid()
	}
	registry, err := compileRegistryCredentials(snapshot.Document, now)
	if err != nil {
		return operatorcredentials.Document{}, operatorcredentials.Operation{}, errOperatorAuthorityUnavailable
	}
	current, ok := registry.byID[actor.CredentialID]
	if !ok || actor.CredentialAuthority != "registry" || !sameCredentialIdentity(actor, current.caller) || !current.caller.Scopes.full {
		return operatorcredentials.Document{}, operatorcredentials.Operation{}, errCredentialMutationForbidden
	}
	next := snapshot.Document.Clone()
	target := request.CredentialID
	if request.Action == "create" {
		if target != "" || request.Policy == nil || !validPreparedOperatorKey(request.Key) {
			return invalid()
		}
		target = "credential_" + operationID.String()
		for _, existing := range next.Credentials {
			if existing.ID == target {
				return invalid()
			}
		}
		created := now.UTC().Truncate(time.Microsecond)
		next.Credentials = append(next.Credentials, (operatorcredentials.Credential{
			ID: target, PrincipalID: "principal_" + uuid.NewSHA1(operationID, []byte("principal")).String(), TenantID: "tenant_" + uuid.NewSHA1(operationID, []byte("tenant")).String(),
			Name: request.Policy.Name, Kind: "integration", Issuer: "kyber", Subject: target, Role: request.Policy.Role, State: operatorcredentials.StateActive, Generation: 1, PermissionRevision: 1, CreatedAt: &created, Source: operatorcredentials.Source{Kind: "managed"},
		}).WithSecretMaterial([]byte(request.Key)))
	} else if target == "" {
		return invalid()
	}
	index := -1
	for i, credential := range next.Credentials {
		if credential.ID == target {
			index = i
			break
		}
	}
	if index < 0 {
		return invalid()
	}
	credential := next.Credentials[index]
	if credential.State != operatorcredentials.StateActive || (credential.Source.Kind != "managed" && credential.Source.Kind != "installation") {
		return invalid()
	}
	switch request.Action {
	case "create", "update":
		if request.Policy == nil || (request.Action == "update" && request.Key != "") {
			return invalid()
		}
		policy := request.Policy
		if policy.Name != strings.TrimSpace(policy.Name) || strings.EqualFold(policy.Name, "legacy") || (policy.ExpiresAt != nil && !policy.ExpiresAt.After(now)) {
			return invalid()
		}
		if policy.Role == operatorcredentials.RoleAdministrator {
			if len(policy.Scopes) != 0 || len(policy.AgentResources) != 0 || len(policy.MachineResources) != 0 {
				return invalid()
			}
		} else if policy.Role == operatorcredentials.RoleRestricted {
			if len(policy.Scopes) == 0 {
				return invalid()
			}
			for _, scope := range policy.Scopes {
				if !validScope(Scope(scope)) {
					return invalid()
				}
				if (Scope(scope) == ScopeTasksAdmin || Scope(scope) == ScopeTasksPlatformAdmin) && (request.Action != "update" || !slices.Contains(credential.Scopes, scope)) {
					return invalid()
				}
				switch managedPermissionTarget(Scope(scope)) {
				case "agent":
					if len(policy.AgentResources) == 0 {
						return invalid()
					}
				case "machine":
					if len(policy.MachineResources) == 0 {
						return invalid()
					}
				}
			}
		} else {
			return invalid()
		}
		for _, resources := range [][]string{policy.AgentResources, policy.MachineResources} {
			for _, resource := range resources {
				namespace, name, ok := strings.Cut(resource, "/")
				if !ok || namespace != next.Namespace || name == "" || strings.Contains(name, "/") || strings.Contains(resource, "*") {
					return invalid()
				}
			}
		}
		prior := credential
		credential.Name = policy.Name
		credential.Role = policy.Role
		credential.Scopes = append([]string{}, policy.Scopes...)
		credential.AgentResources = append([]string{}, policy.AgentResources...)
		credential.MachineResources = append([]string{}, policy.MachineResources...)
		credential.ExpiresAt = nil
		if policy.ExpiresAt != nil {
			expires := policy.ExpiresAt.UTC().Truncate(time.Microsecond)
			credential.ExpiresAt = &expires
		}
		if !sameManagedCredentialPolicy(prior, credential) {
			if credential.PermissionRevision == ^uint64(0) {
				return invalid()
			}
			credential.PermissionRevision++
		}
	case "rotate":
		if request.Policy != nil || !validPreparedOperatorKey(request.Key) || request.Key == string(credential.SecretMaterial()) || credential.Generation == ^uint64(0) {
			return invalid()
		}
		credential = credential.WithSecretMaterial([]byte(request.Key))
		credential.Generation++
	case "revoke":
		if request.Policy != nil || request.Key != "" {
			return invalid()
		}
		credential = credential.WithSecretMaterial(nil)
		credential.State = operatorcredentials.StateRevoked
		if credential.PermissionRevision == ^uint64(0) {
			return invalid()
		}
		credential.PermissionRevision++
	default:
		return invalid()
	}
	if request.Action == "create" {
		credential.PermissionRevision = 1
	}
	next.Credentials[index] = credential
	requestID := request.requestID
	if requestID == "" {
		requestID = request.OperationID
	}
	operation := operatorcredentials.Operation{
		ID: request.OperationID, AdmissionEventID: uuid.NewSHA1(operationID, []byte("admission")).String(), CompletionEventID: uuid.NewSHA1(operationID, []byte("completion")).String(), RequestID: requestID,
		Action: request.Action, ActorPrincipalID: actor.PrincipalID, ActorCredentialID: actor.CredentialID, ActorGeneration: actor.CredentialGeneration, Authority: "registry", TargetCredentialID: target, At: now.UTC().Truncate(time.Microsecond), Status: "pending",
	}
	next.Operations = append(next.Operations, operation)
	next = next.Clone()
	durableAdministrators := 0
	for _, record := range next.Credentials {
		if record.Role == operatorcredentials.RoleAdministrator && record.State == operatorcredentials.StateActive && record.ExpiresAt == nil && (record.Source.Kind == "managed" || record.Source.Kind == "installation") {
			durableAdministrators++
		}
	}
	if durableAdministrators == 0 {
		return operatorcredentials.Document{}, operatorcredentials.Operation{}, errors.Join(errCredentialMutationInvalid, errCredentialLastAdministrator)
	}
	if _, err := compileRegistryCredentials(next, now); err != nil {
		return invalid()
	}
	return next, operation, nil
}

func validPreparedOperatorKey(key string) bool {
	raw, err := base64.RawURLEncoding.DecodeString(key)
	return err == nil && len(raw) == 32 && base64.RawURLEncoding.EncodeToString(raw) == key
}

func sameManagedCredentialPolicy(a, b operatorcredentials.Credential) bool {
	// A normalized single-record document gives independent, sorted grants.
	a = operatorcredentials.Document{Credentials: []operatorcredentials.Credential{a}}.Clone().Credentials[0]
	b = operatorcredentials.Document{Credentials: []operatorcredentials.Credential{b}}.Clone().Credentials[0]
	sameExpiry := a.ExpiresAt == nil && b.ExpiresAt == nil || a.ExpiresAt != nil && b.ExpiresAt != nil && a.ExpiresAt.Equal(*b.ExpiresAt)
	return a.Role == b.Role && a.State == b.State && sameExpiry && reflect.DeepEqual(a.Scopes, b.Scopes) && reflect.DeepEqual(a.AgentResources, b.AgentResources) && reflect.DeepEqual(a.MachineResources, b.MachineResources)
}

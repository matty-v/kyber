// Package operatorcredentials provides candidate registry persistence. Runtime
// adoption, permission compilation and authentication activation are separate
// integration gates; creating a document does not change accepted operator keys.
package operatorcredentials

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	validation "k8s.io/apimachinery/pkg/util/validation"
)

var (
	ErrInvalid           = errors.New("invalid operator credential state")
	ErrUnsupported       = errors.New("unsupported operator credential authority")
	ErrConflict          = errors.New("operator credential state changed")
	ErrUnavailable       = errors.New("operator credential authority unavailable")
	ErrUninitialized     = errors.New("operator credential authority not initialized")
	ErrCapacity          = errors.New("operator credential authority capacity reached")
	ErrSensitive         = errors.New("operator credential state requires a safe projection")
	ErrLastAdministrator = errors.New("a non-expiring managed administrator must remain")
)

const (
	SchemaVersion     = 1
	MaxDocumentBytes  = 512 * 1024
	MaxCredentials    = 256
	MaxOperations     = 128
	RoleAdministrator = "administrator"
	RoleRestricted    = "restricted"
	StateActive       = "active"
	StateRevoked      = "revoked"
)

// References are always interpreted in Document.Namespace, never a caller-
// supplied namespace. Their values are storage bindings, not inventory fields.
type SecretReference struct {
	Name string `json:"name"`
	Key  string `json:"key"`
}
type CallerReference struct {
	SecretReference
	CredentialID string `json:"credentialId,omitempty"`
	CallerName   string `json:"callerName,omitempty"`
}
type Source struct {
	Kind             string           `json:"kind"` // installation, managed, external
	MaterialRef      *SecretReference `json:"materialRef,omitempty"`
	ConfigurationRef *CallerReference `json:"configurationRef,omitempty"`
}

type Credential struct {
	ID                 string     `json:"id"`
	PrincipalID        string     `json:"principalId"`
	TenantID           string     `json:"tenantId"`
	Kind               string     `json:"kind,omitempty"`
	Issuer             string     `json:"issuer,omitempty"`
	Subject            string     `json:"subject,omitempty"`
	Name               string     `json:"name"`
	Role               string     `json:"role"`
	State              string     `json:"state"`
	Generation         uint64     `json:"generation"`
	PermissionRevision uint64     `json:"permissionRevision"`
	Scopes             []string   `json:"scopes"`
	AgentResources     []string   `json:"agentResources"`
	MachineResources   []string   `json:"machineResources"`
	Source             Source     `json:"source"`
	CreatedAt          *time.Time `json:"createdAt,omitempty"`
	ExpiresAt          *time.Time `json:"expiresAt,omitempty"`
	material           []byte
}

// WithSecretMaterial and SecretMaterial copy bytes. Material is intentionally
// unavailable to ordinary field serialization and diagnostic formatting.
func (c Credential) WithSecretMaterial(material []byte) Credential {
	c.material = bytes.Clone(material)
	return c
}
func (c Credential) SecretMaterial() []byte     { return bytes.Clone(c.material) }
func (Credential) String() string               { return "operator credential [redacted]" }
func (c Credential) GoString() string           { return c.String() }
func (c Credential) Format(s fmt.State, _ rune) { _, _ = io.WriteString(s, c.String()) }
func (Credential) MarshalJSON() ([]byte, error) { return nil, ErrSensitive }
func (*Credential) UnmarshalJSON([]byte) error  { return ErrSensitive }

// Operation is a bounded outbox reference, not arbitrary request data. Pending
// means delivery is pending; a committed document proves its state mutation.
// At/event IDs are fixed before the write so later audit retries are idempotent.
type Operation struct {
	ID                 string    `json:"id"`
	AdmissionEventID   string    `json:"admissionEventId"`
	CompletionEventID  string    `json:"completionEventId"`
	RequestID          string    `json:"requestId"`
	Action             string    `json:"action"`
	ActorPrincipalID   string    `json:"actorPrincipalId"`
	ActorCredentialID  string    `json:"actorCredentialId"`
	ActorGeneration    uint64    `json:"actorGeneration"`
	Authority          string    `json:"authority"`
	TargetCredentialID string    `json:"targetCredentialId,omitempty"`
	At                 time.Time `json:"at"`
	Status             string    `json:"status"` // pending, delivered
}

type Document struct {
	SchemaVersion int
	Namespace     string
	Credentials   []Credential
	Operations    []Operation
}

// Clone gives mutation builders an independent, normalized candidate. Loaded
// snapshots remain unchanged when callers edit nested grants or source refs.
func (d Document) Clone() Document { return normalized(d) }

func (Document) String() string               { return "operator credential document [redacted]" }
func (d Document) GoString() string           { return d.String() }
func (d Document) Format(s fmt.State, _ rune) { _, _ = io.WriteString(s, d.String()) }
func (Document) MarshalJSON() ([]byte, error) { return nil, ErrSensitive }
func (*Document) UnmarshalJSON([]byte) error  { return ErrSensitive }

var scopeName = regexp.MustCompile(`^[a-z][a-z0-9-]*(:[a-z][a-z0-9-]*)+$`)
var requestID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$`)

func opaque(value string) bool {
	if len(value) == 0 || len(value) > 512 || !utf8.ValidString(value) || strings.TrimSpace(value) == "" {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}
func displayName(value string) bool {
	if !opaque(value) || len(value) > 256 || strings.EqualFold(strings.TrimSpace(value), "legacy") {
		return false
	}
	for _, r := range value {
		if !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}
func validTime(t time.Time) bool { return !t.IsZero() && t.Year() >= 2000 && t.Year() <= 9999 }
func validReference(r SecretReference) bool {
	return len(validation.IsDNS1123Subdomain(r.Name)) == 0 && len(validation.IsConfigMapKey(r.Key)) == 0
}
func canonicalUUID(value string) bool {
	id, err := uuid.Parse(value)
	return err == nil && id.String() == value
}
func ordered(values []string) []string {
	out := append([]string{}, values...)
	sort.Strings(out)
	return out
}
func distinct(values []string) bool {
	for i := 1; i < len(values); i++ {
		if values[i] == values[i-1] {
			return false
		}
	}
	return true
}

// normalized returns an independent candidate without mutating caller arrays.
func normalized(d Document) Document {
	d.Credentials = append([]Credential{}, d.Credentials...)
	for i, c := range d.Credentials {
		c.material = bytes.Clone(c.material)
		c.Scopes, c.AgentResources, c.MachineResources = ordered(c.Scopes), ordered(c.AgentResources), ordered(c.MachineResources)
		if c.CreatedAt != nil {
			t := c.CreatedAt.UTC().Truncate(time.Microsecond)
			c.CreatedAt = &t
		}
		if c.ExpiresAt != nil {
			t := c.ExpiresAt.UTC().Truncate(time.Microsecond)
			c.ExpiresAt = &t
		}
		if c.Source.MaterialRef != nil {
			r := *c.Source.MaterialRef
			c.Source.MaterialRef = &r
		}
		if c.Source.ConfigurationRef != nil {
			r := *c.Source.ConfigurationRef
			c.Source.ConfigurationRef = &r
		}
		d.Credentials[i] = c
	}
	sort.Slice(d.Credentials, func(i, j int) bool { return d.Credentials[i].ID < d.Credentials[j].ID })
	d.Operations = append([]Operation{}, d.Operations...)
	for i := range d.Operations {
		d.Operations[i].At = d.Operations[i].At.UTC().Truncate(time.Microsecond)
	}
	sort.Slice(d.Operations, func(i, j int) bool { return d.Operations[i].ID < d.Operations[j].ID })
	return d
}

func validate(d Document) error {
	if d.SchemaVersion != SchemaVersion {
		return ErrUnsupported
	}
	if len(validation.IsDNS1123Label(d.Namespace)) != 0 {
		return ErrInvalid
	}
	if len(d.Credentials) > MaxCredentials || len(d.Operations) > MaxOperations {
		return ErrCapacity
	}
	ids, names, materials := map[string]bool{}, map[string]bool{}, map[[32]byte]bool{}
	principals := map[string]string{}
	generations := map[string]uint64{}
	administrator := false
	for _, c := range d.Credentials {
		name := strings.ToLower(strings.TrimSpace(c.Name))
		if !opaque(c.ID) || !opaque(c.PrincipalID) || !opaque(c.TenantID) || !displayName(c.Name) || ids[c.ID] || names[name] || c.Generation == 0 || c.PermissionRevision == 0 {
			return ErrInvalid
		}
		ids[c.ID], names[name] = true, true
		for _, value := range []string{c.Kind, c.Issuer, c.Subject} {
			if value != "" && !opaque(value) {
				return ErrInvalid
			}
		}
		principals[c.ID] = c.PrincipalID
		generations[c.ID] = c.Generation
		if c.CreatedAt != nil && !validTime(*c.CreatedAt) || c.ExpiresAt != nil && !validTime(*c.ExpiresAt) || c.CreatedAt != nil && c.ExpiresAt != nil && !c.CreatedAt.Before(*c.ExpiresAt) {
			return ErrInvalid
		}
		if c.State != StateActive && c.State != StateRevoked {
			return ErrInvalid
		}
		if c.State == StateRevoked {
			if len(c.material) != 0 {
				return ErrInvalid
			}
		} else {
			if len(c.material) > 4096 || strings.TrimSpace(string(c.material)) == "" {
				return ErrInvalid
			}
			binding := sha256.Sum256(c.material)
			if materials[binding] {
				return ErrInvalid
			}
			materials[binding] = true
		}
		switch c.Source.Kind {
		case "installation", "managed":
			if c.Source.MaterialRef != nil || c.Source.ConfigurationRef != nil {
				return ErrInvalid
			}
		case "external":
			if c.Source.MaterialRef == nil && c.Source.ConfigurationRef == nil {
				return ErrInvalid
			}
			if c.Source.MaterialRef != nil && !validReference(*c.Source.MaterialRef) {
				return ErrInvalid
			}
			if r := c.Source.ConfigurationRef; r != nil {
				if !validReference(r.SecretReference) || (r.CredentialID == "" && r.CallerName == "") || (r.CredentialID != "" && !opaque(r.CredentialID)) || (r.CallerName != "" && !opaque(r.CallerName)) {
					return ErrInvalid
				}
			}
		default:
			return ErrInvalid
		}
		if !distinct(c.Scopes) || !distinct(c.AgentResources) || !distinct(c.MachineResources) || len(c.Scopes) > 64 || len(c.AgentResources) > 256 || len(c.MachineResources) > 256 {
			return ErrInvalid
		}
		switch c.Role {
		case RoleAdministrator:
			if len(c.Scopes) != 0 {
				return ErrInvalid
			}
			for _, targets := range [][]string{c.AgentResources, c.MachineResources} {
				if len(targets) > 0 && (len(targets) != 1 || targets[0] != "*") {
					return ErrInvalid
				}
			}
			if c.State == StateActive && c.ExpiresAt == nil && c.Source.Kind != "external" {
				administrator = true
			}
		case RoleRestricted:
			for _, scope := range c.Scopes {
				if len(scope) > 64 || !scopeName.MatchString(scope) {
					return ErrInvalid
				}
			}
			for _, targets := range [][]string{c.AgentResources, c.MachineResources} {
				for _, target := range targets {
					parts := strings.Split(target, "/")
					if len(parts) != 2 || parts[0] != d.Namespace || len(validation.IsDNS1123Subdomain(parts[1])) != 0 {
						return ErrInvalid
					}
				}
			}
		default:
			return ErrInvalid
		}
	}
	if !administrator {
		return ErrLastAdministrator
	}
	operations, events := map[string]bool{}, map[string]bool{}
	for _, op := range d.Operations {
		if !canonicalUUID(op.ID) || operations[op.ID] || !requestID.MatchString(op.RequestID) || !opaque(op.ActorPrincipalID) || !opaque(op.ActorCredentialID) || op.ActorGeneration == 0 || !validTime(op.At) {
			return ErrInvalid
		}
		if principals[op.ActorCredentialID] != op.ActorPrincipalID || op.ActorGeneration > generations[op.ActorCredentialID] || op.TargetCredentialID != "" && !ids[op.TargetCredentialID] {
			return ErrInvalid
		}
		operations[op.ID] = true
		for _, id := range []string{op.AdmissionEventID, op.CompletionEventID} {
			if !canonicalUUID(id) || events[id] {
				return ErrInvalid
			}
			events[id] = true
		}
		if op.Authority != "configuration" && op.Authority != "registry" {
			return ErrInvalid
		}
		if op.Status != "pending" && op.Status != "delivered" {
			return ErrInvalid
		}
		switch op.Action {
		case "create", "update", "rotate", "revoke":
			if !opaque(op.TargetCredentialID) {
				return ErrInvalid
			}
		case "adopt":
			if op.TargetCredentialID != "" && !opaque(op.TargetCredentialID) {
				return ErrInvalid
			}
		default:
			return ErrInvalid
		}
	}
	return nil
}

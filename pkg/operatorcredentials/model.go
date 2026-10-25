// Package operatorcredentials provides candidate registry persistence. Runtime
// adoption, permission compilation and authentication activation are separate
// integration gates; creating a document does not change accepted operator keys.
package operatorcredentials

import (
	"bytes"
	"crypto/subtle"
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
	ID                      string     `json:"id"`
	PrincipalID             string     `json:"principalId"`
	TenantID                string     `json:"tenantId"`
	Kind                    string     `json:"kind,omitempty"`
	Issuer                  string     `json:"issuer,omitempty"`
	Subject                 string     `json:"subject,omitempty"`
	Name                    string     `json:"name"`
	Role                    string     `json:"role"`
	State                   string     `json:"state"`
	Generation              uint64     `json:"generation"`
	PermissionRevision      uint64     `json:"permissionRevision"`
	Scopes                  []string   `json:"scopes"`
	AgentResources          []string   `json:"agentResources"`
	MachineResources        []string   `json:"machineResources"`
	Source                  Source     `json:"source"`
	CreatedAt               *time.Time `json:"createdAt,omitempty"`
	ExpiresAt               *time.Time `json:"expiresAt,omitempty"`
	material                []byte
	legacyBrowserGeneration uint64
}

// WithSecretMaterial and SecretMaterial copy bytes. Material is intentionally
// unavailable to ordinary field serialization and diagnostic formatting.
func (c Credential) WithSecretMaterial(material []byte) Credential {
	c.material = bytes.Clone(material)
	return c
}

// WithLegacyBrowserGeneration marks only credentials present at adoption as
// eligible for V1 compatibility. New registry credentials must leave this zero.
func (c Credential) WithLegacyBrowserGeneration(generation uint64) Credential {
	c.legacyBrowserGeneration = generation
	return c
}
func (c Credential) LegacyBrowserGeneration() uint64 { return c.legacyBrowserGeneration }
func (c Credential) SecretMaterial() []byte          { return bytes.Clone(c.material) }
func (Credential) String() string                    { return "operator credential [redacted]" }
func (c Credential) GoString() string                { return c.String() }
func (c Credential) Format(s fmt.State, _ rune)      { _, _ = io.WriteString(s, c.String()) }
func (Credential) MarshalJSON() ([]byte, error)      { return nil, ErrSensitive }
func (*Credential) UnmarshalJSON([]byte) error       { return ErrSensitive }

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
	SchemaVersion                  int
	Namespace                      string
	Credentials                    []Credential
	Operations                     []Operation
	legacyBrowserSigningKey        []byte
	legacyBrowserSigningGeneration uint64
	legacyBrowserIssuedBefore      time.Time
	legacyBrowserExpiresBefore     time.Time
}

// Clone gives mutation builders an independent, normalized candidate. Loaded
// snapshots remain unchanged when callers edit nested grants or source refs.
func (d Document) Clone() Document { return normalized(d) }

// WithLegacyBrowserSigningKey preserves only the derived V1 cookie signing
// key during adoption, never a second accepted bearer key. It cannot change
// after insertion, including its fixed issuance/expiry horizon. The private
// storage codec is its only serialization path.
func (d Document) WithLegacyBrowserSigningKey(key []byte, generation uint64, issuedBefore, expiresBefore time.Time) Document {
	d.legacyBrowserSigningKey = bytes.Clone(key)
	d.legacyBrowserSigningGeneration = generation
	d.legacyBrowserIssuedBefore = issuedBefore
	d.legacyBrowserExpiresBefore = expiresBefore
	return d
}
func (d Document) LegacyBrowserSigningKey() []byte        { return bytes.Clone(d.legacyBrowserSigningKey) }
func (d Document) LegacyBrowserSigningGeneration() uint64 { return d.legacyBrowserSigningGeneration }
func (d Document) LegacyBrowserIssuedBefore() time.Time   { return d.legacyBrowserIssuedBefore }
func (d Document) LegacyBrowserExpiresBefore() time.Time  { return d.legacyBrowserExpiresBefore }

// Validate checks the complete bounded storage shape without exposing its
// private encoding. Permission compilation must still reject unsupported grants.
func (d Document) Validate() error {
	_, _, err := encode(d)
	return err
}

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
	d.legacyBrowserIssuedBefore = d.legacyBrowserIssuedBefore.UTC().Truncate(time.Microsecond)
	d.legacyBrowserExpiresBefore = d.legacyBrowserExpiresBefore.UTC().Truncate(time.Microsecond)
	d.legacyBrowserSigningKey = bytes.Clone(d.legacyBrowserSigningKey)
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
	if len(d.legacyBrowserSigningKey) == 0 {
		if d.legacyBrowserSigningGeneration != 0 || !d.legacyBrowserIssuedBefore.IsZero() || !d.legacyBrowserExpiresBefore.IsZero() {
			return ErrInvalid
		}
	} else if len(d.legacyBrowserSigningKey) != 32 || d.legacyBrowserSigningGeneration == 0 || !validTime(d.legacyBrowserIssuedBefore) || !validTime(d.legacyBrowserExpiresBefore) || !d.legacyBrowserExpiresBefore.After(d.legacyBrowserIssuedBefore) {
		return ErrInvalid
	}
	if d.SchemaVersion != SchemaVersion {
		return ErrUnsupported
	}
	if len(validation.IsDNS1123Label(d.Namespace)) != 0 {
		return ErrInvalid
	}
	if len(d.Credentials) > MaxCredentials || len(d.Operations) > MaxOperations {
		return ErrCapacity
	}
	ids, names := map[string]bool{}, map[string]bool{}
	materials := make([][]byte, 0, len(d.Credentials))
	principals := map[string]string{}
	generations := map[string]uint64{}
	administrator := false
	for _, c := range d.Credentials {
		if c.legacyBrowserGeneration > c.Generation || c.legacyBrowserGeneration > 0 && len(d.legacyBrowserSigningKey) == 0 {
			return ErrInvalid
		}
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
			materials = append(materials, c.material)
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
	// Sort only this private list, never the document or a presented bearer.
	// Adjacent exact comparisons avoid password-like digests and a quadratic
	// scan at the supported registry capacity. No derived fingerprint escapes.
	sort.Slice(materials, func(i, j int) bool { return bytes.Compare(materials[i], materials[j]) < 0 })
	for i := 1; i < len(materials); i++ {
		if subtle.ConstantTimeCompare(materials[i-1], materials[i]) == 1 {
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

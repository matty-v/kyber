// Package securityaudit stores operator security metadata separately from Agent
// content. It is a persistence foundation; callers must enforce authorization
// and required-before-effect ordering when integrating it with an API.
package securityaudit

import (
	"errors"
	"regexp"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
)

var (
	ErrInvalid     = errors.New("invalid security event")
	ErrConflict    = errors.New("security event identity reused")
	ErrUnavailable = errors.New("security history unavailable")
)

// Event deliberately has no body, URL, error-text or arbitrary-attributes field.
// IDs identify credentials and principals, never their authentication material.
type Event struct {
	// CredentialAuthority qualifies the reported identity/generation. Existing
	// configuration snapshots are not a complete material-rotation ledger.
	CredentialAuthority string `json:"credentialAuthority,omitempty"`
	Method              string `json:"method,omitempty"`

	ID           string    `json:"id"`
	OperationID  string    `json:"operationId"`
	ObservedAt   time.Time `json:"observedAt"`
	Namespace    string    `json:"namespace"`
	PrincipalID  string    `json:"principalId"`
	CredentialID string    `json:"credentialId"`
	Generation   uint64    `json:"credentialGeneration"`
	RequestID    string    `json:"requestId"`
	Action       string    `json:"action"`
	ResourceKind string    `json:"resourceKind"`
	ResourceName string    `json:"resourceName"`
	Phase        string    `json:"phase"`
	Outcome      string    `json:"outcome"`
}

type Record struct {
	Event
	Sequence int64     `json:"sequence"`
	StoredAt time.Time `json:"storedAt"`
}

var identifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$`)
var namespaceName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
var resourceName = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,61}[a-z0-9])?$`)

var actions = map[string]bool{
	"authenticate": true, "agents:exec": true, "machines:exec": true,
	"secrets:list": true, "secrets:read": true, "secrets:write": true,
	"logs:read": true, "transcripts:read": true, "machines:logs:read": true,
	"credential:create": true, "credential:update": true, "credential:rotate": true,
	"credential:revoke": true, "lifecycle:write": true, "lifecycle:delete": true,
	"archives:export": true, "archives:import": true, "audit:read": true,
	"operator:unclassified": true,
}

func (e Event) normalized() Event {
	e.ObservedAt = e.ObservedAt.UTC().Truncate(time.Microsecond)
	return e
}

func (e Event) Validate() error {
	if e.CredentialAuthority != "" && e.CredentialAuthority != "configuration" {
		return ErrInvalid
	}
	switch e.Method {
	case "", "GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS", "OTHER":
	default:
		return ErrInvalid
	}

	if _, err := uuid.Parse(e.ID); err != nil {
		return ErrInvalid
	}
	if _, err := uuid.Parse(e.OperationID); err != nil {
		return ErrInvalid
	}
	if e.ObservedAt.IsZero() || e.ObservedAt.Year() < 2000 || e.ObservedAt.Year() > 9999 || !namespaceName.MatchString(e.Namespace) || !identifier.MatchString(e.RequestID) || !actions[e.Action] {
		return ErrInvalid
	}
	switch e.Phase {
	case "authentication", "authorization":
		if e.Outcome != "allow" && e.Outcome != "deny" {
			return ErrInvalid
		}
	case "completion":
		if e.Outcome != "success" && e.Outcome != "failure" && e.Outcome != "unknown" {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	if e.Phase == "authentication" && e.Action != "authenticate" || e.Action == "authenticate" && e.Phase != "authentication" {
		return ErrInvalid
	}
	anonymous := (e.Phase == "authentication" || e.Phase == "authorization") && e.Outcome == "deny" && e.PrincipalID == "" && e.CredentialID == "" && e.Generation == 0
	if !anonymous && (!validActorID(e.PrincipalID) || !validActorID(e.CredentialID) || e.Generation == 0) {
		return ErrInvalid
	}
	switch e.ResourceKind {
	case "installation":
		if e.ResourceName != "" {
			return ErrInvalid
		}
	case "agent", "machine":
		if !resourceName.MatchString(e.ResourceName) {
			return ErrInvalid
		}
	case "credential":
		if !identifier.MatchString(e.ResourceName) {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	return nil
}

// Configured identities are opaque: existing task principals can be emails or
// issuer-qualified IDs. Preserve them without accepting unbounded/control data.
func validActorID(value string) bool {
	if value == "" || len(value) > 512 || !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

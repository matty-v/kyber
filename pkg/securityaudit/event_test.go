package securityaudit

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

func fixtureEvent() Event {
	return Event{ID: uuid.NewString(), OperationID: uuid.NewString(), ObservedAt: time.Date(2026, 10, 23, 12, 0, 0, 123456789, time.UTC), Namespace: "kyber-system", PrincipalID: "principal_example", CredentialID: "credential_example", Generation: 1, RequestID: uuid.NewString(), Action: "secrets:read", ResourceKind: "agent", ResourceName: "example", Phase: "authorization", Outcome: "allow"}
}
func TestMetadataBoundary(t *testing.T) {
	original := fixtureEvent()
	if err := original.Validate(); err != nil {
		t.Fatal(err)
	}
	for label, mutate := range map[string]func(*Event){
		"raw URL action":                   func(e *Event) { e.Action = "/api/v1/agents/x/secrets?token=private" },
		"arbitrary outcome":                func(e *Event) { e.Outcome = "private backend error" },
		"decision cannot claim completion": func(e *Event) { e.Outcome = "success" },
		"missing credential":               func(e *Event) { e.CredentialID = "" },
		"missing generation":               func(e *Event) { e.Generation = 0 },
		"generation overflow":              func(e *Event) { e.Generation = 1 << 63 },
		"resource traversal":               func(e *Event) { e.ResourceName = "../private" },
		"raw command":                      func(e *Event) { e.ResourceName = "example; cat /secrets" },
		"unbounded identity":               func(e *Event) { e.PrincipalID = string(make([]byte, 200)) },
		"namespace wildcard":               func(e *Event) { e.Namespace = "*" },
		"unknown phase":                    func(e *Event) { e.Phase = "backend-response" },
		"invalid event identity":           func(e *Event) { e.ID = "arbitrary" },
		"no time":                          func(e *Event) { e.ObservedAt = time.Time{} },
		"auth mislabeled":                  func(e *Event) { e.Action = "authenticate" },
	} {
		t.Run(label, func(t *testing.T) {
			e := original
			mutate(&e)
			if !errors.Is(e.Validate(), ErrInvalid) {
				t.Fatal("unsafe event accepted")
			}
		})
	}
	e := original
	e.Phase = "authentication"
	e.Action = "authenticate"
	e.Outcome = "deny"
	e.PrincipalID = ""
	e.CredentialID = ""
	e.Generation = 0
	e.ResourceKind = "installation"
	e.ResourceName = ""
	if err := e.Validate(); err != nil {
		t.Fatal("anonymous denial", err)
	}
	e.Outcome = "allow"
	if e.Validate() == nil {
		t.Fatal("anonymous success accepted")
	}
	e = original
	e.Phase = "completion"
	e.Outcome = "unknown"
	if e.Validate() != nil {
		t.Fatal("uncertain completion must be representable")
	}
}
func TestUnavailableStoreAndErrorRedaction(t *testing.T) {
	if _, err := NewPostgresStore(nil); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
	for _, err := range []error{errors.New("password=private"), errors.New("SQL query private")} {
		if storeError(err) != ErrUnavailable {
			t.Fatal("backend details leaked")
		}
	}
}

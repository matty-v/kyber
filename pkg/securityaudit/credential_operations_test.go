package securityaudit

import (
	"github.com/google/uuid"
	"testing"
)

func fixtureCredentialIntent() CredentialIntent {
	e := fixtureEvent()
	e.Action = "credential:rotate"
	e.ResourceKind = "credential"
	e.ResourceName = "credential-target"
	e.CredentialAuthority = "registry"
	return CredentialIntent{Admission: e, CompletionEventID: uuid.NewString(), ExpectedVersion: "opaque-version", RegistryUID: uuid.NewString()}
}
func TestCredentialIntentMetadataBoundary(t *testing.T) {
	if fixtureCredentialIntent().Validate() != nil {
		t.Fatal("valid intent rejected")
	}
	for name, mutate := range map[string]func(*CredentialIntent){
		"missing incarnation":         func(i *CredentialIntent) { i.RegistryUID = "" },
		"missing CAS":                 func(i *CredentialIntent) { i.ExpectedVersion = "" },
		"completion reuses admission": func(i *CredentialIntent) { i.CompletionEventID = i.Admission.ID },
		"denied":                      func(i *CredentialIntent) { i.Admission.Outcome = "deny" },
		"configuration":               func(i *CredentialIntent) { i.Admission.CredentialAuthority = "configuration" },
		"unrelated action":            func(i *CredentialIntent) { i.Admission.Action = "secrets:read" },
		"completion":                  func(i *CredentialIntent) { i.Admission.Phase = "completion"; i.Admission.Outcome = "success" },
	} {
		t.Run(name, func(t *testing.T) {
			i := fixtureCredentialIntent()
			mutate(&i)
			if i.Validate() == nil {
				t.Fatal("invalid recovery intent accepted")
			}
		})
	}
}

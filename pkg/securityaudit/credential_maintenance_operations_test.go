package securityaudit

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
)

func fixtureMaintenanceIntent() CredentialIntent {
	intent := fixtureCredentialIntent()
	intent.RegistryUID = ""
	intent.MaintenanceUID = uuid.NewString()
	intent.Admission.ResourceKind = "credential-maintenance"
	intent.Admission.ResourceName = uuid.NewString()
	intent.Admission.Action = "credential:recover-maintenance"
	intent.Admission.CredentialAuthority = "configuration"
	return intent
}
func fixtureMaintenanceEffect(intent CredentialIntent) CredentialMaintenanceEffect {
	return CredentialMaintenanceEffect{ResourceUID: intent.MaintenanceUID, ResourceVersion: "fenced-version", ProofID: uuid.NewString()}
}
func TestCredentialMaintenanceIntentUsesDistinctResourceIdentity(t *testing.T) {
	intent := fixtureMaintenanceIntent()
	if err := intent.Validate(); err != nil {
		t.Fatal(err)
	}
	intent.Admission.CredentialAuthority = "registry"
	if err := intent.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*CredentialIntent){
		func(i *CredentialIntent) { i.RegistryUID = uuid.NewString() },
		func(i *CredentialIntent) { i.MaintenanceUID = "" },
		func(i *CredentialIntent) { i.Admission.ResourceKind = "credential" },
		func(i *CredentialIntent) { i.Admission.Action = "credential:rotate" },
		func(i *CredentialIntent) { i.Admission.CredentialAuthority = "" },
		func(i *CredentialIntent) { i.Admission.ResourceName = "not-an-execution-id" },
	} {
		broken := intent
		change(&broken)
		if err := broken.Validate(); !errors.Is(err, ErrInvalid) {
			t.Fatal("ambiguous recovery identity accepted", err)
		}
	}
	if err := (Filter{Namespace: intent.Admission.Namespace, ResourceKind: intent.Admission.ResourceKind, ResourceName: intent.Admission.ResourceName, Action: intent.Admission.Action, Limit: 10}).Validate(); err != nil {
		t.Fatal("maintenance history unqueryable", err)
	}
}
func TestCredentialMaintenanceEffectRejectsWrongIncarnationAndUnfencedVersion(t *testing.T) {
	intent := fixtureMaintenanceIntent()
	effect := fixtureMaintenanceEffect(intent)
	raw, _ := json.Marshal(effect)
	if got, err := decodeCredentialMaintenanceEffect(intent, raw); err != nil || *got != effect {
		t.Fatal("valid effect rejected", err)
	}
	for _, broken := range []CredentialMaintenanceEffect{
		{ResourceUID: uuid.NewString(), ResourceVersion: effect.ResourceVersion, ProofID: effect.ProofID},
		{ResourceUID: effect.ResourceUID, ResourceVersion: intent.ExpectedVersion, ProofID: effect.ProofID},
		{ResourceUID: effect.ResourceUID, ResourceVersion: "", ProofID: effect.ProofID},
	} {
		raw, _ := json.Marshal(broken)
		if _, err := decodeCredentialMaintenanceEffect(intent, raw); !errors.Is(err, ErrUnavailable) {
			t.Fatal("invalid effect proof accepted", err)
		}
	}
	if _, err := decodeCredentialMaintenanceEffect(fixtureCredentialIntent(), raw); !errors.Is(err, ErrUnavailable) {
		t.Fatal("key mutation gained maintenance delete proof", err)
	}
}

package securityaudit

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
)

func fixtureAdoptionIntent() CredentialIntent {
	i := fixtureMaintenanceIntent()
	i.Admission.Action = "credential:adopt"
	i.Admission.ResourceKind = "credential-adoption"
	i.Admission.CredentialAuthority = "configuration"
	i.Admission.ResourceName = i.MaintenanceUID
	i.Adoption = &CredentialAdoptionAdmission{RootSourceName: "root-source", Sources: []CredentialAdoptionSource{{Name: "reference-source", UID: uuid.NewString(), Version: "reference-version"}, {Name: "root-source", UID: uuid.NewString(), Version: "root-version"}}}
	return i
}
func TestCredentialAdoptionIntentPinsCompleteMetadataWithoutRegistryIdentity(t *testing.T) {
	if fixtureAdoptionIntent().Validate() != nil {
		t.Fatal("valid adoption intent rejected")
	}
	for name, change := range map[string]func(*CredentialIntent){
		"missing graph":    func(i *CredentialIntent) { i.Adoption = nil },
		"empty graph":      func(i *CredentialIntent) { i.Adoption.Sources = nil },
		"missing root":     func(i *CredentialIntent) { i.Adoption.RootSourceName = "other-source" },
		"invalid name":     func(i *CredentialIntent) { i.Adoption.Sources[0].Name = "other/namespace" },
		"duplicate source": func(i *CredentialIntent) { i.Adoption.Sources[1] = i.Adoption.Sources[0] },
		"duplicate UID":    func(i *CredentialIntent) { i.Adoption.Sources[1].UID = i.Adoption.Sources[0].UID },
		"noncanonical UID": func(i *CredentialIntent) { i.Adoption.Sources[0].UID = "invalid" },
		"missing version":  func(i *CredentialIntent) { i.Adoption.Sources[0].Version = "" },
		"unordered graph": func(i *CredentialIntent) {
			i.Adoption.Sources[0], i.Adoption.Sources[1] = i.Adoption.Sources[1], i.Adoption.Sources[0]
		},
		"registry claim":      func(i *CredentialIntent) { i.RegistryUID = uuid.NewString() },
		"selected authority":  func(i *CredentialIntent) { i.Admission.CredentialAuthority = "registry" },
		"other maintenance":   func(i *CredentialIntent) { i.Admission.ResourceName = uuid.NewString() },
		"missing maintenance": func(i *CredentialIntent) { i.MaintenanceUID = "" },
		"wrong action":        func(i *CredentialIntent) { i.Admission.Action = "credential:rotate" },
	} {
		t.Run(name, func(t *testing.T) {
			i := fixtureAdoptionIntent()
			change(&i)
			if !errors.Is(i.Validate(), ErrInvalid) {
				t.Fatal("invalid adoption metadata accepted")
			}
		})
	}
	for _, i := range []CredentialIntent{fixtureCredentialIntent(), fixtureMaintenanceIntent()} {
		i.Adoption = fixtureAdoptionIntent().Adoption
		if i.Validate() == nil {
			t.Fatal("adoption metadata accepted for another operation kind")
		}
	}
}

func TestCredentialAdoptionCheckpointsAdvanceWithoutChangingObservedUIDs(t *testing.T) {
	registry, authority := uuid.NewString(), uuid.NewString()
	steps := []CredentialAdoptionEffect{{Stage: "candidate"}, {Stage: "registry", RegistryUID: registry}, {Stage: "selection", RegistryUID: registry}, {Stage: "selected", RegistryUID: registry, AuthorityUID: authority}}
	for i, step := range steps {
		if step.Validate() != nil || !validAdoptionTransition(&step, step) {
			t.Fatal("valid checkpoint/idempotent retry rejected")
		}
		if i == 0 {
			if !validAdoptionTransition(nil, step) {
				t.Fatal("initial create admission rejected")
			}
		} else if !validAdoptionTransition(&steps[i-1], step) {
			t.Fatal("next checkpoint rejected")
		}
		if i > 0 && validAdoptionTransition(nil, step) {
			t.Fatal("effect skipped original create admission")
		}
		for j, other := range steps {
			if j != i && j != i+1 && validAdoptionTransition(&step, other) {
				t.Fatal("checkpoint regressed or skipped")
			}
		}
	}
	changed := steps[2]
	changed.RegistryUID = uuid.NewString()
	if validAdoptionTransition(&steps[1], changed) {
		t.Fatal("observed registry replaced")
	}
	changed = steps[3]
	changed.AuthorityUID = uuid.NewString()
	if validAdoptionTransition(&steps[3], changed) {
		t.Fatal("observed authority replaced")
	}
	for _, effect := range []CredentialAdoptionEffect{{}, {Stage: "candidate", RegistryUID: registry}, {Stage: "registry"}, {Stage: "selection", RegistryUID: registry, AuthorityUID: authority}, {Stage: "selected", RegistryUID: registry}} {
		if effect.Validate() == nil {
			t.Fatal("invalid effect accepted")
		}
	}
}

func TestCredentialAdoptionCheckpointCodecRejectsOtherPayloads(t *testing.T) {
	intent := fixtureAdoptionIntent()
	effect := CredentialAdoptionEffect{Stage: "candidate"}
	raw, _ := json.Marshal(effect)
	maintenance, got, err := decodeCredentialEffects(intent, raw)
	if err != nil || maintenance != nil || got == nil || *got != effect {
		t.Fatal("checkpoint did not roundtrip")
	}
	for _, bad := range [][]byte{[]byte(`{"stage":"candidate","key":"forbidden"}`), []byte(`{"stage":"candidate"}{}`), []byte(`{"stage":"selected"}`), make([]byte, 4097)} {
		if _, _, err := decodeCredentialEffects(intent, bad); !errors.Is(err, ErrUnavailable) {
			t.Fatal("invalid effect payload accepted")
		}
	}
	if _, _, err := decodeCredentialEffects(fixtureCredentialIntent(), raw); !errors.Is(err, ErrUnavailable) {
		t.Fatal("adoption effect accepted for a key mutation")
	}
}

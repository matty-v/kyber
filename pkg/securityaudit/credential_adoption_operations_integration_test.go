//go:build integration

package securityaudit

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestPostgresAdoptionJournalImmutableSourcesStagesAndRestart(t *testing.T) {
	store, db := postgresFixture(t)
	ctx := context.Background()
	intent := fixtureAdoptionIntent()
	if _, err := store.Append(ctx, intent.Admission); err != nil {
		t.Fatal("pre-launch authorization audit failed", err)
	}
	first, err := store.AdmitCredentialOperation(ctx, intent)
	if err != nil || !first.NewlyAdmitted {
		t.Fatal("adoption admission failed", err)
	}
	originalVersion := intent.Adoption.Sources[0].Version
	intent.Adoption.Sources[0].Version = "caller-mutated-version"
	if first.Intent.Adoption.Sources[0].Version != originalVersion {
		t.Fatal("caller changed the acknowledged admission snapshot")
	}
	intent.Adoption.Sources[0].Version = originalVersion
	replay, err := store.AdmitCredentialOperation(ctx, intent)
	if err != nil || replay.NewlyAdmitted {
		t.Fatal("replay owned another effect", err)
	}
	changed := fixtureAdoptionIntent()
	changed.Admission = intent.Admission
	changed.CompletionEventID = intent.CompletionEventID
	changed.MaintenanceUID = intent.MaintenanceUID
	changed.ExpectedVersion = intent.ExpectedVersion
	if _, err := store.AdmitCredentialOperation(ctx, changed); !errors.Is(err, ErrConflict) {
		t.Fatal("source graph was rewritten", err)
	}
	key := fixtureCredentialIntent()
	if _, err := store.AdmitCredentialOperation(ctx, key); err != nil {
		t.Fatal(err)
	}
	maintenance := fixtureMaintenanceIntent()
	if _, err := store.AdmitCredentialOperation(ctx, maintenance); err != nil {
		t.Fatal(err)
	}
	pending, err := store.PendingCredentialAdoptionOperations(ctx, intent.Admission.Namespace, "", 10)
	if err != nil || len(pending) != 1 || pending[0].AdoptionEffect != nil {
		t.Fatal("pending adoption not isolated", err)
	}
	keys, err := store.PendingCredentialOperations(ctx, intent.Admission.Namespace, "", 10)
	if err != nil || len(keys) != 1 || keys[0].Intent.Admission.OperationID != key.Admission.OperationID {
		t.Fatal("adoption leaked into key recovery", err)
	}
	maintenancePending, err := store.PendingCredentialMaintenanceOperations(ctx, intent.Admission.Namespace, "", 10)
	if err != nil || len(maintenancePending) != 1 {
		t.Fatal("adoption leaked into maintenance recovery", err)
	}
	registry, authority := uuid.NewString(), uuid.NewString()
	stages := []CredentialAdoptionEffect{{Stage: "candidate"}, {Stage: "registry", RegistryUID: registry}, {Stage: "selection", RegistryUID: registry}, {Stage: "selected", RegistryUID: registry, AuthorityUID: authority}}
	restarted, _ := NewPostgresStore(db)
	for _, stage := range stages {
		if _, err := restarted.RecordCredentialAdoptionEffect(ctx, intent.Admission.Namespace, intent.Admission.OperationID, stage); err != nil {
			t.Fatal("checkpoint failed", err)
		}
		state, err := restarted.GetCredentialOperation(ctx, intent.Admission.Namespace, intent.Admission.OperationID)
		if err != nil || state.AdoptionEffect == nil || *state.AdoptionEffect != stage || state.Effect != nil {
			t.Fatal("checkpoint lost on restart", err)
		}
		if _, err := store.FinishCredentialAdoptionOperation(ctx, intent.Admission.Namespace, intent.Admission.OperationID, "failure", time.Now(), &stage); !errors.Is(err, ErrConflict) {
			t.Fatal("potential late create finalized as failure", err)
		}
	}
	if _, err := store.FinishCredentialOperation(ctx, intent.Admission.Namespace, intent.Admission.OperationID, "success", time.Now()); !errors.Is(err, ErrInvalid) {
		t.Fatal("generic key finalizer bypassed adoption proof", err)
	}
	if _, err := store.FinishCredentialMaintenanceOperation(ctx, intent.Admission.Namespace, intent.Admission.OperationID, "success", time.Now(), nil); !errors.Is(err, ErrInvalid) {
		t.Fatal("maintenance finalizer bypassed adoption proof", err)
	}
	stale := stages[2]
	if _, err := store.FinishCredentialAdoptionOperation(ctx, intent.Admission.Namespace, intent.Admission.OperationID, "success", time.Now(), &stale); !errors.Is(err, ErrConflict) {
		t.Fatal("stale checkpoint finalized success", err)
	}
	final, err := store.FinishCredentialAdoptionOperation(ctx, intent.Admission.Namespace, intent.Admission.OperationID, "success", time.Now(), &stages[3])
	if err != nil {
		t.Fatal(err)
	}
	again, err := restarted.FinishCredentialAdoptionOperation(ctx, intent.Admission.Namespace, intent.Admission.OperationID, "success", time.Now().Add(time.Hour), &stages[3])
	if err != nil || final != again {
		t.Fatal("completion retry changed audit", err)
	}
	rows, err := store.Query(ctx, Filter{Namespace: intent.Admission.Namespace, ResourceKind: "credential-adoption", Limit: 10}, nil)
	if err != nil || len(rows.Items) != 2 {
		t.Fatal("missing or duplicate adoption audit", err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM operator_security_events WHERE namespace=$1 AND resource_kind=$2`, intent.Admission.Namespace, "credential-adoption"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AdmitCredentialOperation(ctx, intent); err != nil {
		t.Fatal(err)
	}
	if _, err := store.FinishCredentialAdoptionOperation(ctx, intent.Admission.Namespace, intent.Admission.OperationID, "success", time.Now(), &stages[3]); err != nil {
		t.Fatal(err)
	}
	rows, err = store.Query(ctx, Filter{Namespace: intent.Admission.Namespace, ResourceKind: "credential-adoption", Limit: 10}, nil)
	if err != nil || len(rows.Items) != 0 {
		t.Fatal("retention recreated old history", err)
	}
}

func TestPostgresAdoptionJournalFailureCannotRaceLateCreateAdmission(t *testing.T) {
	store, _ := postgresFixture(t)
	ctx := context.Background()
	for range 8 {
		intent := fixtureAdoptionIntent()
		if _, err := store.AdmitCredentialOperation(ctx, intent); err != nil {
			t.Fatal(err)
		}
		var workers sync.WaitGroup
		var effectErr, finishErr error
		effect := CredentialAdoptionEffect{Stage: "candidate"}
		workers.Go(func() {
			_, effectErr = store.RecordCredentialAdoptionEffect(ctx, intent.Admission.Namespace, intent.Admission.OperationID, effect)
		})
		workers.Go(func() {
			_, finishErr = store.FinishCredentialAdoptionOperation(ctx, intent.Admission.Namespace, intent.Admission.OperationID, "failure", time.Now(), nil)
		})
		workers.Wait()
		if (effectErr == nil) == (finishErr == nil) {
			t.Fatal("effect admission and terminal failure did not have one winner")
		}
		loser := effectErr
		if loser == nil {
			loser = finishErr
		}
		if !errors.Is(loser, ErrConflict) {
			t.Fatal("unexpected finality conflict", loser)
		}
		observed, err := store.GetCredentialOperation(ctx, intent.Admission.Namespace, intent.Admission.OperationID)
		if err != nil {
			t.Fatal(err)
		}
		if observed.Completion != nil && observed.AdoptionEffect != nil {
			t.Fatal("late create admitted after terminal failure")
		}
	}
}

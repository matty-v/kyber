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

func TestPostgresMaintenanceJournalEffectAndFinality(t *testing.T) {
	store, db := postgresFixture(t)
	ctx := context.Background()
	intent := fixtureMaintenanceIntent()
	effect := fixtureMaintenanceEffect(intent)
	admitted, err := store.AdmitCredentialOperation(ctx, intent)
	if err != nil || !admitted.NewlyAdmitted {
		t.Fatal("maintenance admission not owned", err)
	}
	keyed := fixtureCredentialIntent()
	if _, err := store.AdmitCredentialOperation(ctx, keyed); err != nil {
		t.Fatal(err)
	}
	keys, err := store.PendingCredentialOperations(ctx, intent.Admission.Namespace, "", 10)
	if err != nil || len(keys) != 1 || keys[0].Intent.Admission.OperationID != keyed.Admission.OperationID {
		t.Fatal("maintenance leaked into key recovery list", err)
	}
	pending, err := store.PendingCredentialMaintenanceOperations(ctx, intent.Admission.Namespace, "", 10)
	if err != nil || len(pending) != 1 || pending[0].Effect != nil {
		t.Fatal("maintenance recovery not discoverable", err)
	}
	var workers sync.WaitGroup
	results := make(chan error, 8)
	for range 8 {
		workers.Go(func() {
			_, err := store.RecordCredentialMaintenanceEffect(ctx, intent.Admission.Namespace, intent.Admission.OperationID, effect)
			results <- err
		})
	}
	workers.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal("identical effect admission not idempotent", err)
		}
	}
	restarted, _ := NewPostgresStore(db)
	got, err := restarted.GetCredentialOperation(ctx, intent.Admission.Namespace, intent.Admission.OperationID)
	if err != nil || got.Effect == nil || *got.Effect != effect || got.NewlyAdmitted {
		t.Fatal("effect lost on restart", err)
	}
	changed := effect
	changed.ProofID = uuid.NewString()
	if _, err := store.RecordCredentialMaintenanceEffect(ctx, intent.Admission.Namespace, intent.Admission.OperationID, changed); !errors.Is(err, ErrConflict) {
		t.Fatal("delete intent was rewritten", err)
	}
	if _, err := store.FinishCredentialMaintenanceOperation(ctx, intent.Admission.Namespace, intent.Admission.OperationID, "failure", time.Now(), nil); !errors.Is(err, ErrConflict) {
		t.Fatal("stale no-effect view finalized pending delete", err)
	}
	if _, err := store.FinishCredentialOperation(ctx, intent.Admission.Namespace, intent.Admission.OperationID, "success", time.Now()); !errors.Is(err, ErrInvalid) {
		t.Fatal("generic completion bypassed checkpoint comparison", err)
	}
	first, err := store.FinishCredentialMaintenanceOperation(ctx, intent.Admission.Namespace, intent.Admission.OperationID, "success", time.Now(), &effect)
	if err != nil {
		t.Fatal(err)
	}
	again, err := restarted.FinishCredentialMaintenanceOperation(ctx, intent.Admission.Namespace, intent.Admission.OperationID, "success", time.Now().Add(time.Hour), &effect)
	if err != nil || first.ID != again.ID || first.ObservedAt != again.ObservedAt {
		t.Fatal("completion retry changed audit", err)
	}
	history, err := store.Query(ctx, Filter{Namespace: intent.Admission.Namespace, ResourceKind: "credential-maintenance", Limit: 10}, nil)
	if err != nil || len(history.Items) != 2 {
		t.Fatal("duplicate or missing recovery audit", err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM operator_security_events WHERE namespace=$1 AND resource_kind=$2`, intent.Admission.Namespace, "credential-maintenance"); err != nil {
		t.Fatal("fixture history cleanup failed")
	}
	if _, err := restarted.AdmitCredentialOperation(ctx, intent); err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.FinishCredentialMaintenanceOperation(ctx, intent.Admission.Namespace, intent.Admission.OperationID, "success", time.Now(), &effect); err != nil {
		t.Fatal(err)
	}
	history, err = store.Query(ctx, Filter{Namespace: intent.Admission.Namespace, ResourceKind: "credential-maintenance", Limit: 10}, nil)
	if err != nil || len(history.Items) != 0 {
		t.Fatal("retention recreated operation history", err)
	}
}
func TestPostgresMaintenanceFinalFailureStopsLateEffectAdmission(t *testing.T) {
	store, _ := postgresFixture(t)
	ctx := context.Background()
	intent := fixtureMaintenanceIntent()
	if _, err := store.AdmitCredentialOperation(ctx, intent); err != nil {
		t.Fatal(err)
	}
	if _, err := store.FinishCredentialMaintenanceOperation(ctx, intent.Admission.Namespace, intent.Admission.OperationID, "failure", time.Now(), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordCredentialMaintenanceEffect(ctx, intent.Admission.Namespace, intent.Admission.OperationID, fixtureMaintenanceEffect(intent)); !errors.Is(err, ErrConflict) {
		t.Fatal("terminal no-effect failure admitted a later delete", err)
	}
}
func TestPostgresMaintenanceEffectRaceWithNoEffectCompletion(t *testing.T) {
	store, _ := postgresFixture(t)
	ctx := context.Background()
	intent := fixtureMaintenanceIntent()
	effect := fixtureMaintenanceEffect(intent)
	if _, err := store.AdmitCredentialOperation(ctx, intent); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	go func() {
		<-start
		_, err := store.RecordCredentialMaintenanceEffect(ctx, intent.Admission.Namespace, intent.Admission.OperationID, effect)
		results <- err
	}()
	go func() {
		<-start
		_, err := store.FinishCredentialMaintenanceOperation(ctx, intent.Admission.Namespace, intent.Admission.OperationID, "failure", time.Now(), nil)
		results <- err
	}()
	close(start)
	successes, conflicts := 0, 0
	for range 2 {
		err := <-results
		if err == nil {
			successes++
		} else if errors.Is(err, ErrConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatal("effect and stale failure both committed")
	}
	got, err := store.GetCredentialOperation(ctx, intent.Admission.Namespace, intent.Admission.OperationID)
	if err != nil || (got.Effect != nil && got.Completion != nil) {
		t.Fatal("contradictory recovery state", err)
	}
}

func TestPostgresMaintenanceJournalMigrationPreservesPriorKeyIntent(t *testing.T) {
	store, db := postgresFixture(t)
	ctx := context.Background()
	intent := fixtureCredentialIntent()
	if _, err := store.AdmitCredentialOperation(ctx, intent); err != nil {
		t.Fatal(err)
	}
	// This is the fixture's isolated schema, never the installation schema.
	if _, err := db.ExecContext(ctx, `ALTER TABLE operator_credential_operations DROP COLUMN effect`); err != nil {
		t.Fatal("old-schema fixture preparation failed")
	}
	restarted, _ := NewPostgresStore(db)
	if err := restarted.Migrate(ctx); err != nil {
		t.Fatal("additive journal migration failed", err)
	}
	got, err := restarted.GetCredentialOperation(ctx, intent.Admission.Namespace, intent.Admission.OperationID)
	if err != nil || got.Intent.RegistryUID != intent.RegistryUID || got.Intent.ExpectedVersion != intent.ExpectedVersion || got.Effect != nil || got.Completion != nil {
		t.Fatal("prior pending key intent changed", err)
	}
	if _, err := restarted.FinishCredentialOperation(ctx, intent.Admission.Namespace, intent.Admission.OperationID, "failure", time.Now()); err != nil {
		t.Fatal("prior operation cannot finish after migration", err)
	}
}

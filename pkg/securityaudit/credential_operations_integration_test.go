//go:build integration

package securityaudit

import (
	"context"
	"errors"
	"github.com/google/uuid"
	"sync"
	"testing"
	"time"
)

func TestPostgresCredentialIntentAtomicityRecoveryAndFinality(t *testing.T) {
	s, db := postgresFixture(t)
	ctx := context.Background()
	intent := fixtureCredentialIntent()
	if _, err := s.AdmitCredentialOperation(ctx, intent); err != nil {
		t.Fatal(err)
	}
	restarted, _ := NewPostgresStore(db)
	status, err := restarted.GetCredentialOperation(ctx, intent.Admission.Namespace, intent.Admission.OperationID)
	if err != nil || status.Intent.ExpectedVersion != intent.ExpectedVersion || status.Completion != nil {
		t.Fatal("lost admission acknowledgement lookup", err)
	}
	if _, err := restarted.GetCredentialOperation(ctx, "another-installation", intent.Admission.OperationID); !errors.Is(err, ErrOperationNotFound) {
		t.Fatal("cross-namespace operation lookup", err)
	}
	pending, err := restarted.PendingCredentialOperations(ctx, intent.Admission.Namespace, "", 10)
	if err != nil || len(pending) != 1 || pending[0].Intent.ExpectedVersion != intent.ExpectedVersion {
		t.Fatal("crash recovery lost CAS intent", err)
	}
	conflict := intent
	conflict.ExpectedVersion = "newer-version"
	if _, err = s.AdmitCredentialOperation(ctx, conflict); !errors.Is(err, ErrConflict) {
		t.Fatal("CAS refreshed during replay", err)
	}
	conflict = intent
	conflict.Admission.ID = uuid.NewString()
	if _, err = s.AdmitCredentialOperation(ctx, conflict); !errors.Is(err, ErrConflict) {
		t.Fatal("operation reused with new admission", err)
	}
	history, err := s.Query(ctx, Filter{Namespace: intent.Admission.Namespace, Limit: 10}, nil)
	if err != nil || len(history.Items) != 1 {
		t.Fatal("conflicting admission partially committed", err)
	}
	if _, err = s.FinishCredentialOperation(ctx, intent.Admission.Namespace, intent.Admission.OperationID, "unknown", time.Now()); !errors.Is(err, ErrInvalid) {
		t.Fatal("uncertain write marked final", err)
	}
	var wg sync.WaitGroup
	outcomes := make(chan error, 2)
	for _, outcome := range []string{"success", "failure"} {
		wg.Go(func() {
			_, err := s.FinishCredentialOperation(ctx, intent.Admission.Namespace, intent.Admission.OperationID, outcome, time.Now())
			outcomes <- err
		})
	}
	wg.Wait()
	close(outcomes)
	successes, conflicts := 0, 0
	for err := range outcomes {
		if err == nil {
			successes++
		} else if errors.Is(err, ErrConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatal("completion was overwritten or duplicated")
	}
	got, err := restarted.AdmitCredentialOperation(ctx, intent)
	if err != nil || got.Completion == nil {
		t.Fatal("lost completion acknowledgement could not reconcile", err)
	}
	first := *got.Completion
	again, err := s.FinishCredentialOperation(ctx, intent.Admission.Namespace, intent.Admission.OperationID, first.Outcome, time.Now().Add(time.Hour))
	if err != nil || !again.ObservedAt.Equal(first.ObservedAt) || again.ID != intent.CompletionEventID {
		t.Fatal("completion retry rewrote history", err)
	}
	pending, err = s.PendingCredentialOperations(ctx, intent.Admission.Namespace, "", 10)
	if err != nil || len(pending) != 0 {
		t.Fatal("completed operation still pending", err)
	}
	other, err := s.PendingCredentialOperations(ctx, "another-installation", "", 10)
	if err != nil || len(other) != 0 {
		t.Fatal("namespace recovery boundary", err)
	}
	// History retention must not erase replay protection or recreate old events.
	if _, err = db.ExecContext(ctx, `DELETE FROM operator_security_events WHERE namespace=$1`, intent.Admission.Namespace); err != nil {
		t.Fatal("isolated history cleanup failed")
	}
	if _, err = s.AdmitCredentialOperation(ctx, intent); err != nil {
		t.Fatal(err)
	}
	if _, err = s.FinishCredentialOperation(ctx, intent.Admission.Namespace, intent.Admission.OperationID, first.Outcome, time.Now()); err != nil {
		t.Fatal(err)
	}
	history, err = s.Query(ctx, Filter{Namespace: intent.Admission.Namespace, Limit: 10}, nil)
	if err != nil || len(history.Items) != 0 {
		t.Fatal("replay recreated retained-out history", err)
	}
}

func TestPostgresCredentialAdmissionRollsBackConflictingEvent(t *testing.T) {
	s, _ := postgresFixture(t)
	ctx := context.Background()
	intent := fixtureCredentialIntent()
	conflict := intent.Admission
	conflict.Outcome = "deny"
	if _, err := s.Append(ctx, conflict); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdmitCredentialOperation(ctx, intent); !errors.Is(err, ErrConflict) {
		t.Fatal("conflicting audit event admitted", err)
	}
	pending, err := s.PendingCredentialOperations(ctx, intent.Admission.Namespace, "", 10)
	if err != nil || len(pending) != 0 {
		t.Fatal("intent committed without matching durable admission", err)
	}
}

func TestPostgresCredentialCompletionConflictKeepsRecoveryPending(t *testing.T) {
	s, _ := postgresFixture(t)
	ctx := context.Background()
	intent := fixtureCredentialIntent()
	if _, err := s.AdmitCredentialOperation(ctx, intent); err != nil {
		t.Fatal(err)
	}
	collision := intent.Admission
	collision.ID = intent.CompletionEventID
	collision.OperationID = uuid.NewString()
	if _, err := s.Append(ctx, collision); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FinishCredentialOperation(ctx, intent.Admission.Namespace, intent.Admission.OperationID, "success", time.Now()); !errors.Is(err, ErrConflict) {
		t.Fatal("conflicting completion overwritten", err)
	}
	pending, err := s.PendingCredentialOperations(ctx, intent.Admission.Namespace, "", 10)
	if err != nil || len(pending) != 1 {
		t.Fatal("completion failure removed recovery work", err)
	}
}

func TestPostgresCredentialRecoveryScanContinuesPastUnresolvedWork(t *testing.T) {
	s, _ := postgresFixture(t)
	ctx := context.Background()
	for range 3 {
		if _, err := s.AdmitCredentialOperation(ctx, fixtureCredentialIntent()); err != nil {
			t.Fatal(err)
		}
	}
	seen := map[string]bool{}
	after := ""
	for range 4 {
		page, err := s.PendingCredentialOperations(ctx, "kyber-system", after, 1)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) == 0 {
			break
		}
		after = page[0].Intent.Admission.OperationID
		if seen[after] {
			t.Fatal("recovery scan repeated unresolved work")
		}
		seen[after] = true
	}
	if len(seen) != 3 {
		t.Fatal("unresolved first page starved later operations")
	}
	if _, err := s.PendingCredentialOperations(ctx, "kyber-system", "invalid-cursor", 1); !errors.Is(err, ErrInvalid) {
		t.Fatal("invalid recovery cursor accepted", err)
	}
}

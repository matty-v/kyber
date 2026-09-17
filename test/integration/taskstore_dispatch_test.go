//go:build integration

package integration

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/matty-v/kyber/pkg/taskstore"
)

func TestTaskStoreAmbiguousAttemptIsNeverRedelivered(t *testing.T) {
	for _, tc := range []struct {
		name           string
		taskID         string
		receiptPending bool
	}{
		{name: "attempting", taskID: "task_c1111111111111111111111111111111"},
		{name: "receipt pending", taskID: "task_c2222222222222222222222222222222", receiptPending: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store := newTaskStore(t)
			agent := taskstore.AgentRef{Namespace: "kyber-system", Name: "mat78-ambiguity"}
			auth := taskstore.AuthorizationContext{
				TenantID: "tenant-mat78", PrincipalID: "operator-mat78", AgentResourceID: "agent-mat78",
			}
			created, err := store.Create(ctx, taskstore.CreateParams{
				ID: tc.taskID, Agent: agent, CreatedBy: "operator", Prompt: "synthetic ambiguous delivery", Authorization: auth,
			})
			if err != nil {
				t.Fatal(err)
			}
			claim, err := store.ClaimPending(ctx, "worker-mat78", time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			attemptID := "attempt_" + strings.TrimPrefix(tc.taskID, "task_")
			if err = store.BeginAttempt(ctx, agent, created.Task.ID, claim.LeaseOwner, attemptID); err != nil {
				t.Fatal(err)
			}
			if tc.receiptPending {
				if err = store.MarkReceiptPending(ctx, agent, created.Task.ID, claim.LeaseOwner, attemptID); err != nil {
					t.Fatal(err)
				}
			}
			if _, err = sharedDB.Exec(`UPDATE agent_task_dispatches SET lease_until=clock_timestamp()-interval '1 second' WHERE task_id=$1`, created.Task.ID); err != nil {
				t.Fatal(err)
			}

			reconciled, err := store.Reconcile(ctx, 10)
			if err != nil || reconciled.UnknownAttempts != 1 {
				t.Fatalf("reconcile=%+v err=%v", reconciled, err)
			}
			got, err := store.Get(ctx, agent, created.Task.ID)
			if err != nil || got.State != taskstore.StateFailed || got.FailureCode != taskstore.FailureDeliveryUnknown {
				t.Fatalf("task=%+v err=%v", got, err)
			}
			if _, err = store.ClaimPending(ctx, "replacement-worker", time.Minute); !errors.Is(err, taskstore.ErrNoDispatch) {
				t.Fatalf("ambiguous attempt became dispatchable again: %v", err)
			}
			var status, failure string
			if err = sharedDB.QueryRow(`SELECT status,last_error_code FROM agent_task_dispatches WHERE task_id=$1`, created.Task.ID).Scan(&status, &failure); err != nil {
				t.Fatal(err)
			}
			if status != "closed" || failure != string(taskstore.FailureDeliveryUnknown) {
				t.Fatalf("dispatch status=%q failure=%q", status, failure)
			}
			events, err := store.ReadEvents(ctx, taskstore.EventReadParams{
				Agent: agent, TaskID: created.Task.ID, Authorization: auth, Limit: 100,
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(events.Events) == 0 {
				t.Fatal("missing terminal task evidence")
			}
			terminal := events.Events[len(events.Events)-1]
			if terminal.Type != taskstore.EventTaskTerminal || !strings.Contains(string(terminal.Payload), `"failureCode":"delivery_unknown"`) {
				t.Fatalf("terminal evidence=%s %s", terminal.Type, terminal.Payload)
			}
		})
	}
}

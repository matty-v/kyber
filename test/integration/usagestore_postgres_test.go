//go:build integration

package integration

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/matty-v/kyber/pkg/usagestore"
)

func TestUsageStorePostgresReplayAndCorrection(t *testing.T) {
	ctx := context.Background()
	store, err := usagestore.NewPostgresStore(sharedDB)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("repeated migration: %v", err)
	}
	uid := uuid.NewString()
	t.Cleanup(func() {
		_, _ = sharedDB.ExecContext(context.Background(), `DELETE FROM agent_usage_events WHERE agent_uid=$1 AND supersedes_id IS NOT NULL`, uid)
		_, _ = sharedDB.ExecContext(context.Background(), `DELETE FROM agent_usage_events WHERE agent_uid=$1`, uid)
		_, _ = sharedDB.ExecContext(context.Background(), `DELETE FROM agent_usage_gaps WHERE agent_uid=$1`, uid)
	})
	output := int64(10)
	event := usagestore.Event{AgentUID: uid, Agent: "test", ObservedAt: time.Now().UTC(), Runtime: "claude", SourceFormat: "claude-jsonl", SourceVersion: "1",
		SourceIdentity: "session-1:msg-1", Coverage: "observed", ConfiguredRoute: "native", RouteID: uuid.NewString(), Model: "claude-test", ModelEvidence: "observed", Output: &output}
	first, inserted, err := store.Ingest(ctx, event)
	if err != nil || !inserted || first.ID == "" || first.IngestedAt.IsZero() {
		t.Fatalf("first=%+v inserted=%v err=%v", first, inserted, err)
	}
	beforeCorrection, _, err := store.Snapshot(ctx)
	if err != nil || beforeCorrection < 1 {
		t.Fatalf("event snapshot=%d err=%v", beforeCorrection, err)
	}
	replayed, inserted, err := store.Ingest(ctx, event)
	if err != nil || inserted || replayed.ID != first.ID {
		t.Fatalf("replay=%+v inserted=%v err=%v", replayed, inserted, err)
	}
	var wg sync.WaitGroup
	results := make(chan struct {
		id       string
		inserted bool
		err      error
	}, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e, wasInserted, ingestErr := store.Ingest(ctx, event)
			results <- struct {
				id       string
				inserted bool
				err      error
			}{e.ID, wasInserted, ingestErr}
		}()
	}
	wg.Wait()
	close(results)
	for result := range results {
		if result.err != nil || result.inserted || result.id != first.ID {
			t.Fatalf("concurrent replay=%+v", result)
		}
	}
	changed := event
	otherOutput := int64(11)
	changed.Output = &otherOutput
	if _, _, err := store.Ingest(ctx, changed); !errors.Is(err, usagestore.ErrConflict) {
		t.Fatalf("changed replay err=%v", err)
	}
	correction := event
	correction.SourceIdentity = "session-1:correction-1"
	correction.SupersedesID = first.ID
	corrected, inserted, err := store.Ingest(ctx, correction)
	if err != nil || !inserted || corrected.SupersedesID != first.ID {
		t.Fatalf("correction=%+v inserted=%v err=%v", corrected, inserted, err)
	}
	duplicateCorrection := correction
	duplicateCorrection.SourceIdentity = "session-1:correction-2"
	if _, _, err := store.Ingest(ctx, duplicateCorrection); !errors.Is(err, usagestore.ErrConflict) {
		t.Fatalf("second correction err=%v", err)
	}
	gap := usagestore.Gap{AgentUID: uid, Runtime: "claude", SourceFormat: "claude-jsonl", SourceIdentity: "session-1:offset-400", DetectedAt: time.Now().UTC(), Reason: "truncated"}
	if err := store.RecordGap(ctx, gap); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordGap(ctx, gap); err != nil {
		t.Fatalf("repeated gap: %v", err)
	}
	afterCorrection, gapHorizon, err := store.Snapshot(ctx)
	if err != nil || afterCorrection <= beforeCorrection || gapHorizon < 1 {
		t.Fatalf("snapshot=%d/%d err=%v", afterCorrection, gapHorizon, err)
	}
	var count int
	if err := sharedDB.QueryRowContext(ctx, `SELECT count(*) FROM agent_usage_gaps WHERE agent_uid=$1`, uid).Scan(&count); err != nil || count != 1 {
		t.Fatalf("gap count=%d err=%v", count, err)
	}
	if err := store.DeleteAgent(ctx, uid); err != nil {
		t.Fatalf("DeleteAgent: %v", err)
	}
	if err := sharedDB.QueryRowContext(ctx, `SELECT count(*) FROM agent_usage_events WHERE agent_uid=$1`, uid).Scan(&count); err != nil || count != 0 {
		t.Fatalf("remaining event count=%d err=%v", count, err)
	}
}

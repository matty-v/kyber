//go:build integration

package integration

import (
	"context"
	"crypto/sha256"
	"encoding/json"
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
	reusedUID := uuid.NewString()
	t.Cleanup(func() {
		_, _ = sharedDB.ExecContext(context.Background(), `DELETE FROM agent_usage_events WHERE agent_uid=$1 AND supersedes_id IS NOT NULL`, uid)
		_, _ = sharedDB.ExecContext(context.Background(), `DELETE FROM agent_usage_events WHERE agent_uid=$1`, uid)
		_, _ = sharedDB.ExecContext(context.Background(), `DELETE FROM agent_usage_gaps WHERE agent_uid=$1`, uid)
		_, _ = sharedDB.ExecContext(context.Background(), `DELETE FROM agent_usage_cursors WHERE agent_uid=$1`, uid)
		_, _ = sharedDB.ExecContext(context.Background(), `DELETE FROM agent_usage_daily WHERE agent_uid=$1`, uid)
		_, _ = sharedDB.ExecContext(context.Background(), `DELETE FROM agent_usage_routes WHERE agent_uid=$1`, uid)
		_, _ = sharedDB.ExecContext(context.Background(), `DELETE FROM agent_usage_events WHERE agent_uid=$1`, reusedUID)
		_, _ = sharedDB.ExecContext(context.Background(), `DELETE FROM agent_usage_daily WHERE agent_uid=$1`, reusedUID)
		_, _ = sharedDB.ExecContext(context.Background(), `DELETE FROM agent_usage_routes WHERE agent_uid=$1`, reusedUID)
	})
	routeID, err := store.ResolveRoute(ctx, uid, usagestore.RouteRevision{Runtime: "claude", Kind: "native"})
	if err != nil {
		t.Fatal(err)
	}
	repeatRouteID, err := store.ResolveRoute(ctx, uid, usagestore.RouteRevision{Runtime: "claude", Kind: "native"})
	if err != nil || repeatRouteID != routeID {
		t.Fatalf("stable route=%q/%q err=%v", routeID, repeatRouteID, err)
	}
	changedRouteID, err := store.ResolveRoute(ctx, uid, usagestore.RouteRevision{Runtime: "claude", Kind: "direct-api"})
	if err != nil || changedRouteID == routeID {
		t.Fatalf("changed route=%q err=%v", changedRouteID, err)
	}
	output := int64(10)
	event := usagestore.Event{AgentUID: uid, Agent: "test", ObservedAt: time.Now().UTC(), Runtime: "claude", SourceFormat: "claude-jsonl", SourceVersion: "1",
		SourceIdentity: "session-1:msg-1", Coverage: "observed", ConfiguredRoute: "native", RouteID: routeID, Model: "claude-test", ModelEvidence: "observed", Output: &output}
	first, inserted, err := store.IngestWithCursor(ctx, event, &usagestore.SourceCursor{Namespace: "session-1", Offset: 100})
	if err != nil || !inserted || first.ID == "" || first.IngestedAt.IsZero() {
		t.Fatalf("first=%+v inserted=%v err=%v", first, inserted, err)
	}
	beforeCorrection, _, err := store.Snapshot(ctx)
	if err != nil || beforeCorrection < 1 {
		t.Fatalf("event snapshot=%d err=%v", beforeCorrection, err)
	}
	replayed, inserted, err := store.IngestWithCursor(ctx, event, &usagestore.SourceCursor{Namespace: "session-1", Offset: 120})
	if err != nil || inserted || replayed.ID != first.ID {
		t.Fatalf("replay=%+v inserted=%v err=%v", replayed, inserted, err)
	}
	if offset, err := store.Cursor(ctx, uid, "claude", "session-1"); err != nil || offset != 120 {
		t.Fatalf("cursor=%d err=%v", offset, err)
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
	if _, _, err := store.IngestWithCursor(ctx, changed, &usagestore.SourceCursor{Namespace: "session-1", Offset: 500}); !errors.Is(err, usagestore.ErrConflict) {
		t.Fatalf("changed replay err=%v", err)
	}
	if offset, err := store.Cursor(ctx, uid, "claude", "session-1"); err != nil || offset != 120 {
		t.Fatalf("conflict advanced cursor=%d err=%v", offset, err)
	}
	correction := event
	correction.SourceIdentity = "session-1:correction-1"
	correction.SupersedesID = first.ID
	correction.Output = &otherOutput
	corrected, inserted, err := store.Ingest(ctx, correction)
	if err != nil || !inserted || corrected.SupersedesID != first.ID {
		t.Fatalf("correction=%+v inserted=%v err=%v", corrected, inserted, err)
	}
	var dailyRaw []byte
	if err := sharedDB.QueryRowContext(ctx, `SELECT document FROM agent_usage_daily WHERE agent_uid=$1`, uid).Scan(&dailyRaw); err != nil {
		t.Fatal(err)
	}
	var daily usagestore.DailyAggregate
	if err := json.Unmarshal(dailyRaw, &daily); err != nil {
		t.Fatal(err)
	}
	if daily.EventCount != 1 || daily.Output.Sum == nil || *daily.Output.Sum != 11 || daily.Output.ReportedEvents != 1 {
		t.Fatalf("corrected daily aggregate=%+v", daily)
	}
	reusedName := event
	reusedName.AgentUID = reusedUID
	reusedName.RouteID, err = store.ResolveRoute(ctx, reusedUID, usagestore.RouteRevision{Runtime: "claude", Kind: "native"})
	if err != nil || reusedName.RouteID == routeID {
		t.Fatalf("reused UID route=%q err=%v", reusedName.RouteID, err)
	}
	if _, inserted, err := store.Ingest(ctx, reusedName); err != nil || !inserted {
		t.Fatalf("reused name/identity inserted=%v err=%v", inserted, err)
	}
	wrongRoute := event
	wrongRoute.SourceIdentity = "session-1:wrong-route"
	wrongRoute.RouteID = reusedName.RouteID
	if _, _, err := store.Ingest(ctx, wrongRoute); !errors.Is(err, usagestore.ErrInvalid) {
		t.Fatalf("cross-Agent route err=%v", err)
	}
	duplicateCorrection := correction
	duplicateCorrection.SourceIdentity = "session-1:correction-2"
	if _, _, err := store.Ingest(ctx, duplicateCorrection); !errors.Is(err, usagestore.ErrConflict) {
		t.Fatalf("second correction err=%v", err)
	}
	gap := usagestore.Gap{AgentUID: uid, Runtime: "claude", SourceFormat: "claude-jsonl", SourceIdentity: "session-1:offset-400", DetectedAt: time.Now().UTC(), Reason: "truncated"}
	if err := store.RecordGapWithCursor(ctx, gap, &usagestore.SourceCursor{Namespace: "session-1", Offset: 600}); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordGapWithCursor(ctx, gap, &usagestore.SourceCursor{Namespace: "session-1", Offset: 620}); err != nil {
		t.Fatalf("repeated gap: %v", err)
	}
	if offset, err := store.Cursor(ctx, uid, "claude", "session-1"); err != nil || offset != 620 {
		t.Fatalf("gap cursor=%d err=%v", offset, err)
	}
	changedGap := gap
	changedGap.Reason = "malformed"
	if err := store.RecordGapWithCursor(ctx, changedGap, &usagestore.SourceCursor{Namespace: "session-1", Offset: 700}); !errors.Is(err, usagestore.ErrConflict) {
		t.Fatalf("changed gap err=%v", err)
	}
	if offset, err := store.Cursor(ctx, uid, "claude", "session-1"); err != nil || offset != 620 {
		t.Fatalf("conflicting gap advanced cursor=%d err=%v", offset, err)
	}
	afterCorrection, gapHorizon, err := store.Snapshot(ctx)
	if err != nil || afterCorrection <= beforeCorrection || gapHorizon < 1 {
		t.Fatalf("snapshot=%d/%d err=%v", afterCorrection, gapHorizon, err)
	}
	var count int
	if err := sharedDB.QueryRowContext(ctx, `SELECT count(*) FROM agent_usage_gaps WHERE agent_uid=$1`, uid).Scan(&count); err != nil || count != 1 {
		t.Fatalf("gap count=%d err=%v", count, err)
	}
	tooOld := event
	tooOld.SourceIdentity = "session-1:old-event"
	tooOld.ObservedAt = time.Now().UTC().Add(-100 * 24 * time.Hour)
	if _, _, err := store.Ingest(ctx, tooOld); !errors.Is(err, usagestore.ErrOutsideRetention) {
		t.Fatalf("old event err=%v", err)
	}
	if err := store.Prune(ctx, time.Now().UTC().Add(91*24*time.Hour)); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if err := sharedDB.QueryRowContext(ctx, `SELECT count(*) FROM agent_usage_events WHERE agent_uid=$1`, uid).Scan(&count); err != nil || count != 0 {
		t.Fatalf("retained old events=%d err=%v", count, err)
	}
	if err := sharedDB.QueryRowContext(ctx, `SELECT count(*) FROM agent_usage_daily WHERE agent_uid=$1`, uid).Scan(&count); err != nil || count != 1 {
		t.Fatalf("daily aggregate after detail prune=%d err=%v", count, err)
	}
	if err := store.DeleteAgent(ctx, uid); err != nil {
		t.Fatalf("DeleteAgent: %v", err)
	}
	if err := sharedDB.QueryRowContext(ctx, `SELECT count(*) FROM agent_usage_events WHERE agent_uid=$1`, uid).Scan(&count); err != nil || count != 0 {
		t.Fatalf("remaining event count=%d err=%v", count, err)
	}
	if err := sharedDB.QueryRowContext(ctx, `SELECT count(*) FROM agent_usage_daily WHERE agent_uid=$1`, uid).Scan(&count); err != nil || count != 0 {
		t.Fatalf("remaining aggregate count=%d err=%v", count, err)
	}
	if err := sharedDB.QueryRowContext(ctx, `SELECT count(*) FROM agent_usage_daily WHERE agent_uid=$1`, reusedUID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("reused name aggregate removed=%d err=%v", count, err)
	}
	if err := store.DeleteAgent(ctx, reusedUID); err != nil {
		t.Fatalf("DeleteAgent reused UID: %v", err)
	}
}

func TestUsageReadSnapshotsStayStableAcrossCorrections(t *testing.T) {
	ctx := context.Background()
	store, err := usagestore.NewPostgresStore(sharedDB)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	uid := uuid.NewString()
	t.Cleanup(func() { _ = store.DeleteAgent(context.Background(), uid) })
	routeID, err := store.ResolveRoute(ctx, uid, usagestore.RouteRevision{Runtime: "claude", Kind: "native"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	output := int64(10)
	base := usagestore.Event{AgentUID: uid, Agent: "read-test", ObservedAt: now, Runtime: "claude",
		SourceFormat: "claude-jsonl", SourceVersion: "1", Coverage: "observed", ConfiguredRoute: "native",
		RouteID: routeID, Model: "claude-test", ModelEvidence: "observed", Output: &output}
	first := base
	first.SourceIdentity = "read-test:one"
	first, _, err = store.Ingest(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	second := base
	second.SourceIdentity = "read-test:two"
	if _, _, err := store.Ingest(ctx, second); err != nil {
		t.Fatal(err)
	}
	f := usagestore.ReadFilter{AgentUIDs: []string{uid}, Start: now.Add(-time.Hour), End: now.Add(time.Hour)}
	dailyFilter := usagestore.ReadFilter{AgentUIDs: []string{uid}, Start: now.Truncate(24 * time.Hour), End: now.Truncate(24 * time.Hour).Add(24 * time.Hour)}
	hash := sha256.Sum256([]byte("query"))
	id, eventsAt, gapsAt, err := store.CreateEventSnapshot(ctx, "owner", hash)
	if err != nil {
		t.Fatal(err)
	}
	dayID, err := store.CreateDailySnapshot(ctx, "owner", hash, dailyFilter)
	if err != nil {
		t.Fatal(err)
	}
	page1, err := store.ListEvents(ctx, f, eventsAt, gapsAt, time.Time{}, "", 1)
	if err != nil || len(page1.Events) != 1 || !page1.HasMore {
		t.Fatalf("page1=%+v err=%v", page1, err)
	}
	correction := base
	correction.SourceIdentity = "read-test:correction"
	correction.SupersedesID = first.ID
	forty := int64(40)
	correction.Output = &forty
	if _, _, err := store.Ingest(ctx, correction); err != nil {
		t.Fatal(err)
	}
	page2, err := store.ListEvents(ctx, f, eventsAt, gapsAt, page1.NextIngestedAt, page1.NextID, 1)
	if err != nil || len(page2.Events) != 1 || page2.Events[0].SupersedesID != "" {
		t.Fatalf("pinned page2=%+v err=%v", page2, err)
	}
	if gotEvents, gotGaps, err := store.EventSnapshot(ctx, id, "owner", hash); err != nil || gotEvents != eventsAt || gotGaps != gapsAt {
		t.Fatalf("pinned horizons=%d/%d err=%v", gotEvents, gotGaps, err)
	}
	oldDaily, err := store.ListDailySnapshot(ctx, dayID, "owner", hash, 0, 1)
	if err != nil || len(oldDaily.Rows) != 1 || oldDaily.Rows[0].Output.Sum == nil || *oldDaily.Rows[0].Output.Sum != 20 {
		t.Fatalf("pinned daily=%+v err=%v", oldDaily, err)
	}
	newID, err := store.CreateDailySnapshot(ctx, "owner", hash, dailyFilter)
	if err != nil {
		t.Fatal(err)
	}
	newDaily, err := store.ListDailySnapshot(ctx, newID, "owner", hash, 0, 1)
	if err != nil || len(newDaily.Rows) != 1 || newDaily.Rows[0].Output.Sum == nil || *newDaily.Rows[0].Output.Sum != 50 {
		t.Fatalf("fresh daily=%+v err=%v", newDaily, err)
	}
	if _, _, err := store.EventSnapshot(ctx, id, "wrong-owner", hash); !errors.Is(err, usagestore.ErrReadSnapshotExpired) {
		t.Fatalf("cross-owner snapshot err=%v", err)
	}
	if err := store.DeleteAgent(ctx, uid); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ListDailySnapshot(ctx, dayID, "owner", hash, 0, 1); !errors.Is(err, usagestore.ErrReadSnapshotExpired) {
		t.Fatalf("deleted Agent snapshot err=%v", err)
	}
}

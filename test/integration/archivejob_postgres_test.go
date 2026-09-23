//go:build integration

package integration

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/matty-v/kyber/pkg/archivejob"
)

// TestArchiveJobStore_Postgres exercises the real SQL behind disk archive
// jobs (MAT-87): the one-active-job-per-agent index, the installation-wide
// limit, optimistic versioning, and the worker's active/expired listing.
func TestArchiveJobStore_Postgres(t *testing.T) {
	ctx := context.Background()
	store := archivejob.NewPostgresStore(sharedDB)
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("Migrate is not idempotent: %v", err)
	}
	if _, err := sharedDB.ExecContext(ctx, "TRUNCATE TABLE agent_archive_jobs"); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	job := func(agent string) *archivejob.Job {
		return &archivejob.Job{ID: archivejob.NewID(), Kind: archivejob.KindExport, Agent: agent,
			State: archivejob.StateQueued, CreatedAt: now, UpdatedAt: now}
	}

	a := job("han")
	if err := store.Create(ctx, a, 2); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.Create(ctx, job("han"), 2); !errors.Is(err, archivejob.ErrActiveJob) {
		t.Fatalf("second active export for one agent = %v, want ErrActiveJob", err)
	}
	if err := store.Create(ctx, job("chewie"), 2); err != nil {
		t.Fatal(err)
	}
	if err := store.Create(ctx, job("leia"), 2); !errors.Is(err, archivejob.ErrConcurrency) {
		t.Fatalf("third active job = %v, want ErrConcurrency", err)
	}

	got, err := store.Get(ctx, a.ID)
	if err != nil || got.Version != 1 {
		t.Fatalf("Get = %+v, %v", got, err)
	}
	stale := *got
	got.State = archivejob.StateRunning
	got.Step = archivejob.StepPausing
	if err := store.Update(ctx, got); err != nil || got.Version != 2 {
		t.Fatalf("Update = %v (version %d)", err, got.Version)
	}
	stale.Message = "stale writer"
	if err := store.Update(ctx, &stale); !errors.Is(err, archivejob.ErrConflict) {
		t.Fatalf("stale Update = %v, want ErrConflict", err)
	}

	// Completing frees the agent's slot; an expired completion is listed for GC.
	past := now.Add(-time.Minute)
	got.State, got.ExpiresAt = archivejob.StateCompleted, &past
	if err := store.Update(ctx, got); err != nil {
		t.Fatal(err)
	}
	if err := store.Create(ctx, job("han"), 3); err != nil {
		t.Fatalf("new export after completion = %v", err)
	}
	active, err := store.ListActive(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	var sawExpired bool
	for _, j := range active {
		if j.ID == a.ID {
			sawExpired = true
		}
	}
	if len(active) != 3 || !sawExpired {
		t.Fatalf("ListActive returned %d jobs (expired listed: %v), want 3 including the expired completion", len(active), sawExpired)
	}
	list, err := store.List(ctx, archivejob.KindExport, "han", 10)
	if err != nil || len(list) != 2 {
		t.Fatalf("List(han) = %d, %v", len(list), err)
	}
}

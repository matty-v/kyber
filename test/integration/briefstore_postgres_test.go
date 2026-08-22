//go:build integration

package integration

import (
	"context"
	"database/sql"
	"os"
	"testing"

	_ "github.com/lib/pq"
	"github.com/redis/go-redis/v9"

	"github.com/matty-v/kyber/pkg/briefstore"
)

// TestMain for briefstore_postgres_test. Manages shared DB + Redis lifecycle.
func TestMain(m *testing.M) {
	var err error

	sharedDB, err = openDB()
	if err != nil {
		panic("openDB: " + err.Error())
	}
	defer sharedDB.Close()

	sharedRDB = openRedis()
	defer sharedRDB.Close()

	if err := waitForPostgres(sharedDB); err != nil {
		panic("waitForPostgres: " + err.Error())
	}
	if err := waitForRedis(sharedRDB); err != nil {
		panic("waitForRedis: " + err.Error())
	}

	if err := runMigrations(sharedDB); err != nil {
		panic("runMigrations: " + err.Error())
	}

	os.Exit(m.Run())
}

// newPostgresStore is a helper that returns a BriefStore backed by the shared DB.
func newPostgresStore(_ *testing.T) *briefstore.PostgresStore {
	return briefstore.NewPostgresStore(sharedDB)
}

// cleanBriefs truncates the session_briefs table so tests are isolated.
func cleanBriefs(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), "TRUNCATE TABLE session_briefs"); err != nil {
		t.Fatalf("truncating session_briefs: %v", err)
	}
}

// TestBriefStore_Postgres_PutAndGet verifies a full put/get roundtrip.
func TestBriefStore_Postgres_PutAndGet(t *testing.T) {
	cleanBriefs(t, sharedDB)
	store := newPostgresStore(t)
	ctx := context.Background()

	brief := &briefstore.Brief{
		Version:       1,
		AgentName:     "dave",
		Timestamp:     "2026-04-10T00:00:00Z",
		ShutdownType:  briefstore.ShutdownTypePlanned,
		RestartReason: "operator",
		LastActivity:  "writing D4 tests",
		RecentExchanges: []briefstore.Exchange{
			{Role: "user", Content: "continue", Timestamp: "2026-04-10T00:00:00Z"},
			{Role: "assistant", Content: "on it", Timestamp: "2026-04-10T00:00:01Z"},
		},
		Metadata: briefstore.BriefMetadata{
			PreviousModel: "claude-sonnet-4-5",
			UptimeSeconds: 3600,
			RestartCount:  2,
		},
	}

	if err := store.Put(ctx, "dave", brief); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, err := store.Get(ctx, "dave")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	if got.AgentName != brief.AgentName {
		t.Errorf("AgentName: got %q, want %q", got.AgentName, brief.AgentName)
	}
	if got.ShutdownType != brief.ShutdownType {
		t.Errorf("ShutdownType: got %q, want %q", got.ShutdownType, brief.ShutdownType)
	}
	if got.LastActivity != brief.LastActivity {
		t.Errorf("LastActivity: got %q, want %q", got.LastActivity, brief.LastActivity)
	}
	if len(got.RecentExchanges) != 2 {
		t.Errorf("RecentExchanges: got %d, want 2", len(got.RecentExchanges))
	}
	if got.Metadata.PreviousModel != brief.Metadata.PreviousModel {
		t.Errorf("PreviousModel: got %q, want %q", got.Metadata.PreviousModel, brief.Metadata.PreviousModel)
	}
	if got.Metadata.UptimeSeconds != brief.Metadata.UptimeSeconds {
		t.Errorf("UptimeSeconds: got %d, want %d", got.Metadata.UptimeSeconds, brief.Metadata.UptimeSeconds)
	}
}

// TestBriefStore_Postgres_GetNotFound verifies ErrBriefNotFound for a missing agent.
func TestBriefStore_Postgres_GetNotFound(t *testing.T) {
	cleanBriefs(t, sharedDB)
	store := newPostgresStore(t)
	ctx := context.Background()

	_, err := store.Get(ctx, "nonexistent-agent")
	if err != briefstore.ErrBriefNotFound {
		t.Errorf("expected ErrBriefNotFound, got %v", err)
	}
}

// TestBriefStore_Postgres_Upsert verifies that Put overwrites an existing brief.
func TestBriefStore_Postgres_Upsert(t *testing.T) {
	cleanBriefs(t, sharedDB)
	store := newPostgresStore(t)
	ctx := context.Background()

	first := &briefstore.Brief{
		Version:       1,
		AgentName:     "chewie",
		ShutdownType:  briefstore.ShutdownTypeUnplanned,
		RestartReason: "crash",
	}
	if err := store.Put(ctx, "chewie", first); err != nil {
		t.Fatalf("Put(first): %v", err)
	}

	second := &briefstore.Brief{
		Version:       1,
		AgentName:     "chewie",
		ShutdownType:  briefstore.ShutdownTypePlanned,
		RestartReason: "operator",
		LastActivity:  "updated brief",
	}
	if err := store.Put(ctx, "chewie", second); err != nil {
		t.Fatalf("Put(second): %v", err)
	}

	got, err := store.Get(ctx, "chewie")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	if got.ShutdownType != briefstore.ShutdownTypePlanned {
		t.Errorf("ShutdownType after upsert: got %q, want %q", got.ShutdownType, briefstore.ShutdownTypePlanned)
	}
	if got.LastActivity != "updated brief" {
		t.Errorf("LastActivity after upsert: got %q, want %q", got.LastActivity, "updated brief")
	}
}

// TestBriefStore_Postgres_Delete verifies that Delete removes a brief and Get returns ErrBriefNotFound.
func TestBriefStore_Postgres_Delete(t *testing.T) {
	cleanBriefs(t, sharedDB)
	store := newPostgresStore(t)
	ctx := context.Background()

	brief := &briefstore.Brief{
		Version:      1,
		AgentName:    "r2d2",
		ShutdownType: briefstore.ShutdownTypePlanned,
	}
	if err := store.Put(ctx, "r2d2", brief); err != nil {
		t.Fatalf("Put: %v", err)
	}

	if err := store.Delete(ctx, "r2d2"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	_, err := store.Get(ctx, "r2d2")
	if err != briefstore.ErrBriefNotFound {
		t.Errorf("expected ErrBriefNotFound after delete, got %v", err)
	}
}

// TestBriefStore_Postgres_DeleteIdempotent verifies Delete is a no-op for missing briefs.
func TestBriefStore_Postgres_DeleteIdempotent(t *testing.T) {
	cleanBriefs(t, sharedDB)
	store := newPostgresStore(t)
	ctx := context.Background()

	if err := store.Delete(ctx, "ghost-agent"); err != nil {
		t.Errorf("expected nil error deleting nonexistent agent, got %v", err)
	}
}

// TestBriefStore_Postgres_MultipleAgents verifies multiple agents' briefs are stored independently.
func TestBriefStore_Postgres_MultipleAgents(t *testing.T) {
	cleanBriefs(t, sharedDB)
	store := newPostgresStore(t)
	ctx := context.Background()

	agents := []string{"han", "lando", "threepio"}
	for _, name := range agents {
		b := &briefstore.Brief{
			Version:      1,
			AgentName:    name,
			LastActivity: "task for " + name,
		}
		if err := store.Put(ctx, name, b); err != nil {
			t.Fatalf("Put(%q): %v", name, err)
		}
	}

	for _, name := range agents {
		got, err := store.Get(ctx, name)
		if err != nil {
			t.Fatalf("Get(%q): %v", name, err)
		}
		if got.LastActivity != "task for "+name {
			t.Errorf("agent %q: LastActivity got %q, want %q", name, got.LastActivity, "task for "+name)
		}
	}
}

// Ensure unused imports don't error: redis is imported for sharedRDB type.
var _ *redis.Client = sharedRDB

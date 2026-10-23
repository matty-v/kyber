//go:build integration

package securityaudit

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/lib/pq"
)

// Each fixture uses an isolated schema and pool. Never run the general
// test/integration suite against an operator installation: other suites truncate
// shared tables. These tests only create/drop their own random schema.
func postgresFixture(t *testing.T) (*PostgresStore, *sql.DB) {
	t.Helper()
	dsn := os.Getenv("KYBER_PG_DSN")
	if dsn == "" {
		t.Skip("KYBER_PG_DSN required for real PostgreSQL verification")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	root, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal("database open failed")
	}
	schema := "security_audit_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err = root.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		root.Close()
		t.Fatal("isolated schema create failed")
	}
	t.Cleanup(func() {
		defer root.Close()
		cleanupCtx, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		if _, err := root.ExecContext(cleanupCtx, "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Error("isolated schema cleanup failed")
		}
	})
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			t.Fatal("invalid fixture DSN")
		}
		q := u.Query()
		q.Set("search_path", schema)
		u.RawQuery = q.Encode()
		dsn = u.String()
	} else {
		dsn += " search_path=" + schema
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal("isolated database open failed")
	}
	t.Cleanup(func() { db.Close() })
	db.SetMaxOpenConns(5)
	store, err := NewPostgresStore(db)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err = store.Migrate(ctx); err != nil {
		t.Fatal("repeat migration", err)
	}
	return store, db
}
func TestPostgresIdentityPersistenceAndIsolation(t *testing.T) {
	s, db := postgresFixture(t)
	ctx := context.Background()
	e := fixtureEvent()
	first, err := s.Append(ctx, e)
	if err != nil {
		t.Fatal(err)
	}
	repeat, err := s.Append(ctx, e)
	if err != nil || repeat.Sequence != first.Sequence || !repeat.StoredAt.Equal(first.StoredAt) {
		t.Fatal("replay changed committed identity", err)
	}
	e.Outcome = "deny"
	if _, err = s.Append(ctx, e); !errors.Is(err, ErrConflict) {
		t.Fatal("conflicting ID was overwritten", err)
	}
	restarted, _ := NewPostgresStore(db)
	page, err := restarted.Query(ctx, Filter{Namespace: e.Namespace, Limit: 10}, nil)
	if err != nil || len(page.Items) != 1 || page.Items[0].Outcome != "allow" || page.Items[0].ObservedAt.Nanosecond() != 123456000 {
		t.Fatal("durability/precision", err)
	}
	page, err = s.Query(ctx, Filter{Namespace: "other-installation", Limit: 10}, nil)
	if err != nil || len(page.Items) != 0 {
		t.Fatal("cross-installation disclosure", err)
	}
	for _, filter := range []Filter{{Namespace: e.Namespace, Limit: 10, PrincipalID: "different"}, {Namespace: e.Namespace, Limit: 10, CredentialID: "different"}, {Namespace: e.Namespace, Limit: 10, Action: "logs:read"}, {Namespace: e.Namespace, Limit: 10, ResourceKind: "machine"}, {Namespace: e.Namespace, Limit: 10, ResourceKind: "agent", ResourceName: "different"}, {Namespace: e.Namespace, Limit: 10, Since: e.ObservedAt.Add(time.Second)}, {Namespace: e.Namespace, Limit: 10, Until: e.ObservedAt.Add(-time.Second)}} {
		page, err = s.Query(ctx, filter, nil)
		if err != nil || len(page.Items) != 0 {
			t.Fatal("filter ignored", err)
		}
	}
}
func TestPostgresConcurrentAppendsAndSnapshot(t *testing.T) {
	s, _ := postgresFixture(t)
	ctx := context.Background()
	seed := fixtureEvent()
	var wg sync.WaitGroup
	failures := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e := seed
			e.ID = uuid.NewString()
			_, err := s.Append(ctx, e)
			if err != nil {
				failures <- err
			}
		}()
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		t.Fatal(err)
	}
	filter := Filter{Namespace: seed.Namespace, Limit: 3}
	page, err := s.Query(ctx, filter, nil)
	if err != nil || page.Next == nil {
		t.Fatal(err)
	}
	late := seed
	late.ID = uuid.NewString()
	late.ObservedAt = late.ObservedAt.Add(-time.Hour)
	if _, err = s.Append(ctx, late); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for {
		for _, r := range page.Items {
			if seen[r.ID] || r.ID == late.ID {
				t.Fatal("unstable snapshot")
			}
			seen[r.ID] = true
		}
		if page.Next == nil {
			break
		}
		page, err = s.Query(ctx, filter, page.Next)
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(seen) != 20 {
		t.Fatalf("snapshot lost events: %d", len(seen))
	}
	fresh, err := s.Query(ctx, Filter{Namespace: seed.Namespace, Limit: 100}, nil)
	if err != nil || len(fresh.Items) != 21 {
		t.Fatal("fresh query missed committed event", err)
	}
}
func TestPostgresRetentionAndFailures(t *testing.T) {
	s, db := postgresFixture(t)
	ctx := context.Background()
	e := fixtureEvent()
	if _, err := s.Append(ctx, e); err != nil {
		t.Fatal(err)
	}
	if n, err := s.Prune(ctx, e.Namespace, time.Now().Add(-time.Hour), 10); err != nil || n != 0 {
		t.Fatal("backdated observation prematurely expired", err)
	}
	other := e
	other.ID = uuid.NewString()
	other.Namespace = "another-installation"
	if _, err := s.Append(ctx, other); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		extra := e
		extra.ID = uuid.NewString()
		if _, err := s.Append(ctx, extra); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := s.Prune(ctx, e.Namespace, time.Now().Add(time.Hour), 1); err != nil || n != 1 {
		t.Fatal("bounded retention", err)
	}
	remaining, err := s.Query(ctx, Filter{Namespace: e.Namespace, Limit: 10}, nil)
	if err != nil || len(remaining.Items) != 2 {
		t.Fatal("retention exceeded batch limit", err)
	}
	page, err := s.Query(ctx, Filter{Namespace: other.Namespace, Limit: 10}, nil)
	if err != nil || len(page.Items) != 1 {
		t.Fatal("retention crossed namespace", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	e.ID = uuid.NewString()
	if _, err = s.Append(canceled, e); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled append succeeded", err)
	}
	if _, err = s.Query(canceled, Filter{Namespace: e.Namespace, Limit: 10}, nil); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled query succeeded", err)
	}
	if _, err = s.Query(ctx, Filter{Namespace: e.Namespace, Limit: 201}, nil); !errors.Is(err, ErrInvalid) {
		t.Fatal("unbounded query accepted", err)
	}
	db.Close()
	if _, err = s.Append(ctx, e); !errors.Is(err, ErrUnavailable) {
		t.Fatal("unavailable append succeeded", err)
	}
	if _, err = s.Query(ctx, Filter{Namespace: e.Namespace, Limit: 10}, nil); !errors.Is(err, ErrUnavailable) {
		t.Fatal("unavailable query returned history", err)
	}
}

func TestPostgresConcurrentReplay(t *testing.T) {
	s, _ := postgresFixture(t)
	e := fixtureEvent()
	ctx := context.Background()
	results := make(chan Record, 8)
	failures := make(chan error, 8)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := s.Append(ctx, e)
			if err != nil {
				failures <- err
				return
			}
			results <- r
		}()
	}
	wg.Wait()
	close(results)
	close(failures)
	for err := range failures {
		t.Fatal(err)
	}
	var sequence int64
	for r := range results {
		if sequence != 0 && sequence != r.Sequence {
			t.Fatal("retry inserted a second event")
		}
		sequence = r.Sequence
	}
	page, err := s.Query(ctx, Filter{Namespace: e.Namespace, Limit: 10}, nil)
	if err != nil || len(page.Items) != 1 {
		t.Fatal("concurrent replay was not idempotent", err)
	}
	expired, cancel := context.WithDeadline(ctx, time.Now().Add(-time.Second))
	defer cancel()
	e.ID = uuid.NewString()
	if _, err := s.Append(expired, e); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("expired append succeeded", err)
	}
}

func TestPostgresBlockedAppendDeadline(t *testing.T) {
	s, db := postgresFixture(t)
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, commitLock); err != nil {
		t.Fatal(err)
	}
	short, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	if _, err = s.Append(short, fixtureEvent()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("blocked append did not preserve deadline", err)
	}
	if err = tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	page, err := s.Query(ctx, Filter{Namespace: "kyber-system", Limit: 10}, nil)
	if err != nil || len(page.Items) != 0 {
		t.Fatal("timed-out append persisted an event", err)
	}
}

func TestPostgresCapacityRetentionKeepsNewestAndOtherNamespaces(t *testing.T) {
	s, _ := postgresFixture(t)
	ctx := context.Background()
	e := fixtureEvent()
	for i := 0; i < 6; i++ {
		e.ID = uuid.NewString()
		if _, err := s.Append(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	other := e
	other.ID = uuid.NewString()
	other.Namespace = "other-installation"
	if _, err := s.Append(ctx, other); err != nil {
		t.Fatal(err)
	}
	before, err := s.Query(ctx, Filter{Namespace: e.Namespace, Limit: 20}, nil)
	if err != nil {
		t.Fatal(err)
	}
	n, err := s.EnforceRetention(ctx, e.Namespace, time.Now().Add(-time.Hour), 3, 2)
	if err != nil || n != 2 {
		t.Fatal("capacity pruning did not respect batch", n, err)
	}
	n, err = s.EnforceRetention(ctx, e.Namespace, time.Now().Add(-time.Hour), 3, 2)
	if err != nil || n != 1 {
		t.Fatal("capacity pruning removed wrong count", n, err)
	}
	after, err := s.Query(ctx, Filter{Namespace: e.Namespace, Limit: 20}, nil)
	if err != nil || len(after.Items) != 3 {
		t.Fatal("newest retention", err)
	}
	for i := range after.Items {
		if after.Items[i].ID != before.Items[i].ID {
			t.Fatal("capacity pruning lost newer event")
		}
	}
	preserved, err := s.Query(ctx, Filter{Namespace: other.Namespace, Limit: 20}, nil)
	if err != nil || len(preserved.Items) != 1 {
		t.Fatal("capacity pruning crossed installation", err)
	}
}

func TestPostgresPreservesOpaqueConfiguredIdentityAndGeneration(t *testing.T) {
	s, _ := postgresFixture(t)
	ctx := context.Background()
	e := fixtureEvent()
	e.PrincipalID = "user@example.invalid"
	e.CredentialID = "https://issuer.example.invalid/credentials/one"
	e.Generation = ^uint64(0)
	if _, err := s.Append(ctx, e); err != nil {
		t.Fatal(err)
	}
	page, err := s.Query(ctx, Filter{Namespace: e.Namespace, PrincipalID: e.PrincipalID, CredentialID: e.CredentialID, Limit: 10}, nil)
	if err != nil || len(page.Items) != 1 || page.Items[0].Generation != e.Generation || page.Items[0].PrincipalID != e.PrincipalID {
		t.Fatal("existing identity metadata lost", err)
	}
}

func TestPostgresMatchedLogTargetMetadata(t *testing.T) {
	s, _ := postgresFixture(t)
	ctx := context.Background()
	e := fixtureEvent()
	e.Action = "logs:read"
	e.ResourceKind = "log-container"
	e.ResourceName = "known-pod-uid"
	e.Container = "agent"
	e.LogComponent = "agent"
	e.LogWorkload = "alpha"
	e.CredentialAuthority = "configuration"
	if _, err := s.Append(ctx, e); err != nil {
		t.Fatal(err)
	}
	page, err := s.Query(ctx, Filter{Namespace: e.Namespace, ResourceKind: e.ResourceKind, ResourceName: e.ResourceName, Limit: 10}, nil)
	if err != nil || len(page.Items) != 1 || page.Items[0].Container != e.Container || page.Items[0].LogWorkload != e.LogWorkload {
		t.Fatal("validated log target metadata lost", err)
	}
}

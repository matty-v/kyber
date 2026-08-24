//go:build integration

package integration

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/matty-v/kyber/pkg/skillscan"
	"github.com/matty-v/kyber/pkg/skillstore"
)

// cleanSkills truncates agent_skills so each test starts from empty.
func cleanSkills(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), "TRUNCATE TABLE agent_skills"); err != nil {
		t.Fatalf("truncating agent_skills: %v", err)
	}
}

func integrationReport() *skillscan.Report {
	return &skillscan.Report{
		Version:    skillscan.ReportVersion,
		ReportedAt: "2026-08-24T12:00:00Z",
		Skills: []skillscan.Skill{
			{
				Name:        "restart",
				Description: "Planned shutdown: save, commit, push.",
				Source:      skillscan.SourceIdentity,
				Path:        "skills/restart",
				Linked:      []string{skillscan.RuntimeClaudeCode, skillscan.RuntimeCodex},
			},
			{
				Name:          "triage",
				Description:   "Vendored triage flow.",
				Source:        skillscan.SourceVendor,
				SourcePackage: "falcon-dev-common",
				Path:          "vendor/falcon-dev-common/skills/triage",
				Linked:        []string{},
				Issues: []skillscan.Issue{
					{Code: skillscan.IssueNotLinked, Severity: skillscan.SeverityError, Detail: "not loadable by claude-code"},
				},
			},
		},
		Issues: []skillscan.Issue{
			{Code: skillscan.IssueUnmanaged, Severity: skillscan.SeverityWarning, Detail: "~/.claude/skills/handwritten is a real directory"},
		},
	}
}

// TestSkillStore_Postgres_RoundTrip is the only place the real SQL runs. The
// store is written once per boot and read on every Skills-tab load, so a schema
// or JSON-encoding mistake here would be invisible until an operator opened the
// tab against a Postgres install.
func TestSkillStore_Postgres_RoundTrip(t *testing.T) {
	cleanSkills(t, sharedDB)
	ctx := context.Background()
	store := skillstore.NewPostgresStore(sharedDB)

	if _, err := store.Get(ctx, "dave"); !errors.Is(err, skillstore.ErrNotFound) {
		t.Fatalf("Get before Put: got %v, want ErrNotFound", err)
	}
	if err := store.Put(ctx, "dave", integrationReport()); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, err := store.Get(ctx, "dave")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Version != skillscan.ReportVersion || got.ReportedAt != "2026-08-24T12:00:00Z" {
		t.Errorf("header round-trip lost data: %+v", got)
	}
	if len(got.Skills) != 2 {
		t.Fatalf("skills: got %d, want 2", len(got.Skills))
	}
	// Every nested field matters — the whole value is one JSONB column, so a
	// partial round-trip is a silent data loss, not a compile error.
	if got.Skills[0].Linked == nil || len(got.Skills[0].Linked) != 2 {
		t.Errorf("linked runtimes lost: %+v", got.Skills[0])
	}
	if got.Skills[1].SourcePackage != "falcon-dev-common" {
		t.Errorf("vendor package lost: %+v", got.Skills[1])
	}
	if len(got.Skills[1].Issues) != 1 || got.Skills[1].Issues[0].Code != skillscan.IssueNotLinked {
		t.Errorf("per-skill issues lost: %+v", got.Skills[1])
	} else if got.Skills[1].Issues[0].Severity != skillscan.SeverityError {
		t.Errorf("issue severity lost: %+v", got.Skills[1].Issues[0])
	}
	if len(got.Issues) != 1 || got.Issues[0].Code != skillscan.IssueUnmanaged {
		t.Errorf("report-level issues lost: %+v", got.Issues)
	} else if got.Issues[0].Severity != skillscan.SeverityWarning {
		t.Errorf("report-level issue severity lost: %+v", got.Issues[0])
	}
}

// An agent reports on every boot and sync, so Put must upsert rather than
// accumulate rows or conflict on the primary key.
func TestSkillStore_Postgres_PutOverwrites(t *testing.T) {
	cleanSkills(t, sharedDB)
	ctx := context.Background()
	store := skillstore.NewPostgresStore(sharedDB)

	if err := store.Put(ctx, "dave", integrationReport()); err != nil {
		t.Fatalf("first Put: %v", err)
	}
	second := &skillscan.Report{
		Version: skillscan.ReportVersion,
		Skills:  []skillscan.Skill{{Name: "only-one", Source: skillscan.SourceIdentity, Path: "skills/only-one"}},
	}
	if err := store.Put(ctx, "dave", second); err != nil {
		t.Fatalf("second Put: %v", err)
	}

	got, err := store.Get(ctx, "dave")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(got.Skills) != 1 || got.Skills[0].Name != "only-one" {
		t.Fatalf("expected the second report to replace the first; got %+v", got.Skills)
	}

	var rows int
	if err := sharedDB.QueryRowContext(ctx, "SELECT count(*) FROM agent_skills WHERE agent_name = $1", "dave").Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Errorf("row count: got %d, want 1 — Put must upsert, not append", rows)
	}
}

// The agent delete finalizer calls Delete unconditionally, so deleting an agent
// that never reported must not error.
func TestSkillStore_Postgres_Delete(t *testing.T) {
	cleanSkills(t, sharedDB)
	ctx := context.Background()
	store := skillstore.NewPostgresStore(sharedDB)

	if err := store.Delete(ctx, "never-reported"); err != nil {
		t.Fatalf("Delete of a missing agent: %v", err)
	}
	if err := store.Put(ctx, "dave", integrationReport()); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(ctx, "dave"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := store.Get(ctx, "dave"); !errors.Is(err, skillstore.ErrNotFound) {
		t.Fatalf("Get after Delete: got %v, want ErrNotFound", err)
	}
}

// Migrate runs on every control-plane start.
func TestSkillStore_Postgres_MigrateIsIdempotent(t *testing.T) {
	store := skillstore.NewPostgresStore(sharedDB)
	for i := 0; i < 3; i++ {
		if err := store.Migrate(context.Background()); err != nil {
			t.Fatalf("Migrate run %d: %v", i+1, err)
		}
	}
}

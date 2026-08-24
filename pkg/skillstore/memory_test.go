package skillstore_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/matty-v/kyber/pkg/skillscan"
	"github.com/matty-v/kyber/pkg/skillstore"
)

func sampleReport() *skillscan.Report {
	return &skillscan.Report{
		Version:    skillscan.ReportVersion,
		ReportedAt: "2026-08-24T00:00:00Z",
		Skills: []skillscan.Skill{{
			Name:        "restart",
			Description: "Planned shutdown.",
			Source:      skillscan.SourceIdentity,
			Path:        "skills/restart",
			Linked:      []string{skillscan.RuntimeClaudeCode, skillscan.RuntimeCodex},
		}},
		Issues: []skillscan.Issue{{Code: skillscan.IssueUnmanaged, Detail: "stray"}},
	}
}

func TestMemoryStore_PutGetDelete(t *testing.T) {
	ctx := context.Background()
	s := skillstore.NewMemoryStore()

	if _, err := s.Get(ctx, "dave"); !errors.Is(err, skillstore.ErrNotFound) {
		t.Fatalf("Get before Put: got %v, want ErrNotFound", err)
	}
	if err := s.Put(ctx, "dave", sampleReport()); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := s.Get(ctx, "dave")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(got.Skills) != 1 || got.Skills[0].Name != "restart" {
		t.Fatalf("Get returned %+v", got)
	}
	if err := s.Delete(ctx, "dave"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Get(ctx, "dave"); !errors.Is(err, skillstore.ErrNotFound) {
		t.Fatalf("Get after Delete: got %v, want ErrNotFound", err)
	}
	// Deleting an agent that was never stored is a no-op, because the agent
	// delete finalizer calls it unconditionally.
	if err := s.Delete(ctx, "never-existed"); err != nil {
		t.Fatalf("Delete of missing agent: %v", err)
	}
}

// The store hands out copies. A caller that keeps mutating the report it just
// stored — or the one it just read — must not be able to reach into the store.
func TestMemoryStore_CopiesOnPutAndGet(t *testing.T) {
	ctx := context.Background()
	s := skillstore.NewMemoryStore()

	in := sampleReport()
	if err := s.Put(ctx, "dave", in); err != nil {
		t.Fatal(err)
	}
	in.Skills[0].Name = "mutated-after-put"
	in.Skills[0].Linked[0] = "mutated"
	in.Issues[0].Code = "mutated"

	got, err := s.Get(ctx, "dave")
	if err != nil {
		t.Fatal(err)
	}
	if got.Skills[0].Name != "restart" {
		t.Errorf("Put did not copy: stored name is %q", got.Skills[0].Name)
	}
	if got.Skills[0].Linked[0] != skillscan.RuntimeClaudeCode {
		t.Errorf("Put did not copy Linked: %v", got.Skills[0].Linked)
	}
	if got.Issues[0].Code != skillscan.IssueUnmanaged {
		t.Errorf("Put did not copy Issues: %v", got.Issues)
	}

	got.Skills[0].Name = "mutated-after-get"
	again, err := s.Get(ctx, "dave")
	if err != nil {
		t.Fatal(err)
	}
	if again.Skills[0].Name != "restart" {
		t.Errorf("Get did not copy: stored name is %q", again.Skills[0].Name)
	}
}

func TestMemoryStore_PutNilReportIsRejected(t *testing.T) {
	if err := skillstore.NewMemoryStore().Put(context.Background(), "dave", nil); err == nil {
		t.Fatal("expected an error storing a nil report")
	}
}

func TestMemoryStore_ConcurrentAccess(t *testing.T) {
	ctx := context.Background()
	s := skillstore.NewMemoryStore()
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); _ = s.Put(ctx, "dave", sampleReport()) }()
		go func() { defer wg.Done(); _, _ = s.Get(ctx, "dave") }()
	}
	wg.Wait()
}

// Compile-time proof both implementations satisfy the interface the API and
// the reconciler depend on.
var (
	_ skillstore.Store = (*skillstore.MemoryStore)(nil)
	_ skillstore.Store = (*skillstore.PostgresStore)(nil)
)

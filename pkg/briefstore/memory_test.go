package briefstore_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/matty-v/kyber/pkg/briefstore"
)

func makeTestBrief(agentName string) *briefstore.Brief {
	return &briefstore.Brief{
		Version:       1,
		AgentName:     agentName,
		Timestamp:     "2026-04-10T23:00:00Z",
		ShutdownType:  "planned",
		RestartReason: "operator",
		LastActivity:  "working on B2",
		Metadata: briefstore.BriefMetadata{
			PreviousModel: "claude-sonnet-4",
			UptimeSeconds: 3600,
			RestartCount:  0,
		},
	}
}

func TestMemoryStore_PutAndGet(t *testing.T) {
	store := briefstore.NewMemoryStore()
	ctx := context.Background()

	brief := makeTestBrief("dave")

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
}

func TestMemoryStore_GetMissing_ReturnsErrBriefNotFound(t *testing.T) {
	store := briefstore.NewMemoryStore()
	ctx := context.Background()

	_, err := store.Get(ctx, "nonexistent")
	if !errors.Is(err, briefstore.ErrBriefNotFound) {
		t.Errorf("Get missing: got %v, want ErrBriefNotFound", err)
	}
}

func TestMemoryStore_PutOverwrites(t *testing.T) {
	store := briefstore.NewMemoryStore()
	ctx := context.Background()

	first := makeTestBrief("dave")
	first.LastActivity = "first session"
	if err := store.Put(ctx, "dave", first); err != nil {
		t.Fatalf("Put first: %v", err)
	}

	second := makeTestBrief("dave")
	second.LastActivity = "second session"
	if err := store.Put(ctx, "dave", second); err != nil {
		t.Fatalf("Put second: %v", err)
	}

	got, err := store.Get(ctx, "dave")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.LastActivity != "second session" {
		t.Errorf("LastActivity after overwrite: got %q, want %q", got.LastActivity, "second session")
	}
}

func TestMemoryStore_Delete(t *testing.T) {
	store := briefstore.NewMemoryStore()
	ctx := context.Background()

	brief := makeTestBrief("dave")
	if err := store.Put(ctx, "dave", brief); err != nil {
		t.Fatalf("Put: %v", err)
	}

	if err := store.Delete(ctx, "dave"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	_, err := store.Get(ctx, "dave")
	if !errors.Is(err, briefstore.ErrBriefNotFound) {
		t.Errorf("Get after Delete: got %v, want ErrBriefNotFound", err)
	}
}

func TestMemoryStore_DeleteMissing_IsIdempotent(t *testing.T) {
	store := briefstore.NewMemoryStore()
	ctx := context.Background()

	// Deleting a non-existent key must not return an error.
	if err := store.Delete(ctx, "ghost"); err != nil {
		t.Errorf("Delete missing: got %v, want nil", err)
	}
}

func TestMemoryStore_MultipleAgents(t *testing.T) {
	store := briefstore.NewMemoryStore()
	ctx := context.Background()

	agents := []string{"dave", "chewie", "han"}
	for _, name := range agents {
		b := makeTestBrief(name)
		b.RestartReason = name + "-reason"
		if err := store.Put(ctx, name, b); err != nil {
			t.Fatalf("Put %s: %v", name, err)
		}
	}

	for _, name := range agents {
		got, err := store.Get(ctx, name)
		if err != nil {
			t.Fatalf("Get %s: %v", name, err)
		}
		wantReason := name + "-reason"
		if got.RestartReason != wantReason {
			t.Errorf("%s RestartReason: got %q, want %q", name, got.RestartReason, wantReason)
		}
	}
}

func TestMemoryStore_ConcurrentAccess(t *testing.T) {
	store := briefstore.NewMemoryStore()
	ctx := context.Background()

	const goroutines = 50

	var wg sync.WaitGroup
	wg.Add(goroutines)

	// Half goroutines write, half read — exercise the RW lock.
	for i := 0; i < goroutines; i++ {
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				brief := makeTestBrief("concurrent-agent")
				_ = store.Put(ctx, "concurrent-agent", brief)
			} else {
				_, _ = store.Get(ctx, "concurrent-agent")
			}
		}(i)
	}

	wg.Wait()
	// No data race = test passes (run with -race for full coverage).
}

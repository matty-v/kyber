package tokenstore_test

import (
	"context"
	"testing"

	"github.com/matty-v/kyber/pkg/tokenreport"
	"github.com/matty-v/kyber/pkg/tokenstore"
)

// kyber#565: the agent finalizer reaps per-agent token state on delete.

func TestMemoryStore_Delete(t *testing.T) {
	s := tokenstore.NewMemoryStore()
	ctx := context.Background()
	if err := s.Put(ctx, "dave", &tokenreport.Snapshot{Tokens: tokenreport.Tokens{Used: 42}}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := s.Delete(ctx, "dave"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	got, err := s.Get(ctx, "dave")
	if err != nil {
		t.Fatalf("Get after Delete: %v", err)
	}
	if got != nil {
		t.Errorf("Get after Delete returned %+v, want nil", got)
	}
}

func TestMemoryStore_DeleteMissing_IsIdempotent(t *testing.T) {
	s := tokenstore.NewMemoryStore()
	if err := s.Delete(context.Background(), "ghost"); err != nil {
		t.Errorf("Delete on missing key: got %v, want nil", err)
	}
}

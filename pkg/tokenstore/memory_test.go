package tokenstore_test

import (
	"context"
	"testing"

	"github.com/matty-v/kyber/pkg/tokenreport"
	"github.com/matty-v/kyber/pkg/tokenstore"
)

// Compile-time assertion: MemoryStore satisfies TokenStore.
var _ tokenstore.TokenStore = (*tokenstore.MemoryStore)(nil)

func TestMemoryStore_PutThenGet(t *testing.T) {
	s := tokenstore.NewMemoryStore()
	ctx := context.Background()
	snap := &tokenreport.Snapshot{Tokens: tokenreport.Tokens{Used: 123}}

	if err := s.Put(ctx, "alice", snap); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := s.Get(ctx, "alice")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got == nil || got.Tokens.Used != 123 {
		t.Errorf("Get returned %+v, want Used=123", got)
	}
}

func TestMemoryStore_GetMissingReturnsNil(t *testing.T) {
	s := tokenstore.NewMemoryStore()
	got, err := s.Get(context.Background(), "nobody")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != nil {
		t.Errorf("Get on missing key returned %+v, want nil", got)
	}
}

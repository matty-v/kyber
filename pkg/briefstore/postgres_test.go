package briefstore_test

import (
	"testing"

	"github.com/matty-v/kyber/pkg/briefstore"
)

// Compile-time assertion: PostgresStore must satisfy the BriefStore interface.
// This catches signature mismatches without requiring a live database.
var _ briefstore.BriefStore = (*briefstore.PostgresStore)(nil)

// TestPostgresStore_InterfaceAssertion is a placeholder that documents the compile-time
// assertion above. No runtime test is needed in B2; the MemoryStore tests cover the
// interface contract. PostgresStore integration tests require a live database (deferred).
func TestPostgresStore_InterfaceAssertion(t *testing.T) {
	t.Log("PostgresStore satisfies BriefStore — compile-time check passed")
}

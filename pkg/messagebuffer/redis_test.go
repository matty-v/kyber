package messagebuffer_test

import (
	"github.com/matty-v/kyber/pkg/messagebuffer"
)

// compile-time assertion: RedisBuffer satisfies MessageBuffer.
// This test does not require a live Redis instance — it only verifies the interface
// is satisfied at compile time. Integration tests with a real Redis are deferred to
// when the D1/Helm phase wires up the Redis backend.
var _ messagebuffer.MessageBuffer = (*messagebuffer.RedisBuffer)(nil)

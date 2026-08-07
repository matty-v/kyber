package tokenstore_test

import (
	"github.com/matty-v/kyber/pkg/tokenstore"
)

// Compile-time assertion: RedisStore satisfies TokenStore.
var _ tokenstore.TokenStore = (*tokenstore.RedisStore)(nil)

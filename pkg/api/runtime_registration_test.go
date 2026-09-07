package api_test

// Test servers explicitly enable the production integrations, as main does.
import (
	_ "github.com/matty-v/kyber/pkg/runtimes/claudecode"
	_ "github.com/matty-v/kyber/pkg/runtimes/codex"
)

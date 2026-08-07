package main

import (
	"errors"
	"testing"
	"time"
)

// newTestCache builds a cache over a stub fetch, returning the cache and a
// pointer to the call count so a test can assert what hit "Discord".
func newTestCache(t *testing.T, roles []string, err error) (*botRoleCache, *int) {
	t.Helper()
	calls := 0
	return &botRoleCache{
		fetch: func(string, string) ([]string, error) {
			calls++
			return roles, err
		},
		retryAfter: defaultRoleRetryAfter,
		roles:      map[string]map[string]bool{},
		lastTry:    map[string]time.Time{},
	}, &calls
}

func TestBotRoleCache_ResolvesAndCaches(t *testing.T) {
	c, calls := newTestCache(t, []string{"barfrole", "deployrole"}, nil)

	got := c.lookup("g1", "barfbot")
	if !got["barfrole"] || !got["deployrole"] {
		t.Fatalf("lookup = %v, want both roles present", got)
	}
	// A second lookup must be served from memory: the bot's roles change only
	// when an admin re-roles it, and a per-message API call would be rate-limit
	// bait on a busy channel.
	if got2 := c.lookup("g1", "barfbot"); len(got2) != 2 {
		t.Fatalf("cached lookup = %v, want 2 roles", got2)
	}
	if *calls != 1 {
		t.Errorf("fetch calls = %d, want 1 (second lookup should hit the cache)", *calls)
	}
}

// The @everyone role's ID is the guild ID. Counting it would make every
// server-wide ping wake the agent — exactly what mention-only prevents.
func TestBotRoleCache_ExcludesEveryoneRole(t *testing.T) {
	c, _ := newTestCache(t, []string{"g1", "barfrole", ""}, nil)

	got := c.lookup("g1", "barfbot")
	if got["g1"] {
		t.Error("@everyone role (ID == guild ID) must not count as one of the bot's roles")
	}
	if got[""] {
		t.Error("empty role ID must not be admitted")
	}
	if !got["barfrole"] {
		t.Errorf("lookup = %v, want barfrole", got)
	}
}

// A failed lookup degrades to "role mentions don't match" (the behaviour before
// role mentions were honoured) rather than failing the message — and must not
// retry on every message, or a permissions problem becomes an API flood.
func TestBotRoleCache_FailureDegradesAndBacksOff(t *testing.T) {
	c, calls := newTestCache(t, nil, errors.New("403 Missing Access"))

	if got := c.lookup("g1", "barfbot"); len(got) != 0 {
		t.Fatalf("lookup after failure = %v, want empty", got)
	}
	c.lookup("g1", "barfbot")
	c.lookup("g1", "barfbot")
	if *calls != 1 {
		t.Errorf("fetch calls = %d, want 1 (retry is gated by retryAfter)", *calls)
	}

	// Once the window passes, it tries again — so granting the bot a role (or
	// fixing its permissions) takes effect without a pod restart.
	c.mu.Lock()
	c.lastTry["g1"] = time.Now().Add(-2 * defaultRoleRetryAfter)
	c.mu.Unlock()
	c.lookup("g1", "barfbot")
	if *calls != 2 {
		t.Errorf("fetch calls after the retry window = %d, want 2", *calls)
	}
}

func TestBotRoleCache_IgnoresEmptyIdentifiers(t *testing.T) {
	c, calls := newTestCache(t, []string{"barfrole"}, nil)

	if got := c.lookup("", "barfbot"); got != nil {
		t.Errorf("lookup with no guild = %v, want nil", got)
	}
	if got := c.lookup("g1", ""); got != nil {
		t.Errorf("lookup with no bot identity = %v, want nil", got)
	}
	if *calls != 0 {
		t.Errorf("fetch calls = %d, want 0 (nothing to look up)", *calls)
	}
}

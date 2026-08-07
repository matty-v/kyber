package main

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

// sessionInGuilds builds a session whose local state cache reports membership
// of the given guild IDs — the same cache GUILD_CREATE populates in the real
// sidecar.
func sessionInGuilds(ids ...string) *discordgo.Session {
	st := discordgo.NewState()
	for _, id := range ids {
		st.Guilds = append(st.Guilds, &discordgo.Guild{ID: id})
	}
	return &discordgo.Session{State: st}
}

func allow(ids ...string) map[string]bool {
	m := make(map[string]bool, len(ids))
	for _, id := range ids {
		m[id] = true
	}
	return m
}

func TestGuildWatcher_UnconstrainedStartsReady(t *testing.T) {
	// No allowlist means nothing to verify; failing closed here would break
	// every install that doesn't constrain guilds.
	w := newGuildWatcher(nil)
	if !w.isReady() {
		t.Fatal("unconstrained watcher must start ready")
	}
	if got := w.missingGuilds(sessionInGuilds()); got != nil {
		t.Fatalf("unconstrained watcher reported missing guilds: %v", got)
	}
}

func TestGuildWatcher_MissingGuildDetected(t *testing.T) {
	w := newGuildWatcher(allow("G1"))
	if w.isReady() {
		t.Fatal("constrained watcher must not start ready — membership is unverified")
	}
	missing := w.missingGuilds(sessionInGuilds("G-other"))
	if len(missing) != 1 || missing[0] != "G1" {
		t.Fatalf("missingGuilds=%v, want [G1]", missing)
	}
	if got := w.missingGuilds(sessionInGuilds("G1")); len(got) != 0 {
		t.Fatalf("member of G1 but reported missing: %v", got)
	}
}

// A nil/empty State must not panic — it is the real state right after Open()
// and before the first GUILD_CREATE.
func TestGuildWatcher_NilStateIsSafe(t *testing.T) {
	w := newGuildWatcher(allow("G1"))
	if got := w.missingGuilds(&discordgo.Session{}); len(got) != 1 {
		t.Fatalf("nil state should report the guild missing, got %v", got)
	}
	if got := w.missingGuilds(nil); len(got) != 1 {
		t.Fatalf("nil session should report the guild missing, got %v", got)
	}
}

// The core regression: the bot is invited AFTER the gateway session was
// established. That session goes silently deaf, so the watcher must reconnect
// once membership appears — and must NOT reconnect on an ordinary healthy
// start, which would add a pointless reconnect to every boot.
func TestGuildWatcher_ReconnectsOnLateJoin(t *testing.T) {
	w := newGuildWatcher(allow("G1"))
	var reconnects atomic.Int32

	// Starts absent (the HK-47 case), then the invite lands.
	sess := sessionInGuilds()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		w.run(ctx, sess, func() { reconnects.Add(1) }, 5*time.Millisecond, time.Millisecond)
	}()

	// Let it observe the absence.
	waitFor(t, func() bool { return w.sawMissingSnapshot() }, "watcher never observed the missing guild")
	if w.isReady() {
		t.Fatal("watcher reported ready while not a member")
	}
	if reconnects.Load() != 0 {
		t.Fatal("reconnected before the guild appeared")
	}

	// The bot gets invited. Mutate under the State lock, mirroring how
	// discordgo's gateway goroutine appends on GUILD_CREATE.
	sess.State.Lock()
	sess.State.Guilds = append(sess.State.Guilds, &discordgo.Guild{ID: "G1"})
	sess.State.Unlock()

	waitFor(t, func() bool { return reconnects.Load() == 1 }, "no reconnect after the guild appeared")
	waitFor(t, func() bool { return w.isReady() }, "watcher never became ready after joining")

	// Steady state: no reconnect storm.
	time.Sleep(40 * time.Millisecond)
	if n := reconnects.Load(); n != 1 {
		t.Fatalf("reconnected %d times, want exactly 1 — a repeating reconnect would flap the gateway", n)
	}
	cancel()
	<-done
}

func TestGuildWatcher_NoReconnectOnHealthyStart(t *testing.T) {
	w := newGuildWatcher(allow("G1"))
	var reconnects atomic.Int32
	sess := sessionInGuilds("G1") // already a member, the normal case

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.run(ctx, sess, func() { reconnects.Add(1) }, 5*time.Millisecond, time.Millisecond)
	}()

	waitFor(t, func() bool { return w.isReady() }, "healthy watcher never became ready")
	time.Sleep(40 * time.Millisecond)
	if n := reconnects.Load(); n != 0 {
		t.Fatalf("reconnected %d times on a healthy start, want 0", n)
	}
	cancel()
	<-done
}

func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal(msg)
}

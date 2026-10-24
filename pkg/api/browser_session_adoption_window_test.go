package api

import (
	"testing"
	"time"
)

func TestRegistryCompatibilityCannotMintSessionsAfterCutover(t *testing.T) {
	cutover := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	document := registryFixtureDocument(t)
	document = document.WithLegacyBrowserSigningKey(document.LegacyBrowserSigningKey(), 1, cutover, cutover.Add(browserSessionTTL))
	registry, err := compileRegistryCredentials(document, cutover)
	if err != nil {
		t.Fatal(err)
	}
	entry := registry.byID["https://issuer.invalid/credential/client"]
	binding, err := browserSessionKeyBinding("original-installation-fixture", entry.key)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name        string
		issued, now time.Time
		ttl         time.Duration
		want        bool
	}{
		{"existing session", cutover.Add(-time.Hour), cutover, browserSessionTTL, true},
		{"newly forged after cutover", cutover.Add(time.Second), cutover.Add(2 * time.Second), time.Hour, false},
		{"excessive lifetime", cutover.Add(-time.Hour), cutover, browserSessionTTL + time.Hour, false},
		{"renewed after compatibility horizon", cutover.Add(browserSessionTTL + time.Hour), cutover.Add(browserSessionTTL + 2*time.Hour), browserSessionTTL, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			token, err := signBrowserSession("original-installation-fixture", entry.caller, binding, tc.issued, tc.ttl)
			if err != nil {
				t.Fatal(err)
			}
			_, _, err = registry.browserCaller(token, tc.now)
			if (err == nil) != tc.want {
				t.Fatal("incorrect compatibility lifetime decision", err)
			}
		})
	}
	// New per-key sessions keep working after the one-time compatibility window.
	after := cutover.Add(browserSessionTTL + 24*time.Hour)
	v2, err := registry.createBrowserSession(entry.caller, after)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := registry.browserCaller(v2, after.Add(time.Second)); err != nil {
		t.Fatal("compatibility expiry invalidated current sessions", err)
	}
}

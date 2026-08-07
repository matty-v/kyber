package main

import (
	"os"
	"path/filepath"
	"testing"
)

// resolveDisplayVersion is the source of the user-facing version on
// /api/v1/version (kyber#482). The displayed version is build-injected into
// the image via -ldflags "-X main.Version=…" (mirroring BuildSHA), so it can
// never diverge from the running code. The chart-file read is only a
// local/dev fallback. These tests pin that precedence.

func TestResolveDisplayVersion_LdflagWins(t *testing.T) {
	// When the ldflag is injected (the deployed-image case), its value is
	// authoritative — even if a chart-version file is also present, the
	// build-injected version takes precedence so the displayed version rides
	// in the same image as `sha` and the two can never disagree (AC#2).
	orig := Version
	t.Cleanup(func() { Version = orig })
	Version = "1.10.0"

	dir := t.TempDir()
	path := filepath.Join(dir, "chart-version")
	if err := os.WriteFile(path, []byte("1.9.1\n"), 0o644); err != nil {
		t.Fatalf("write chart file: %v", err)
	}
	t.Setenv("KYBER_CHART_VERSION_PATH", path)

	if got := resolveDisplayVersion(); got != "1.10.0" {
		t.Errorf("resolveDisplayVersion() = %q, want %q (ldflag must win over chart file)", got, "1.10.0")
	}
}

func TestResolveDisplayVersion_FallsBackToChartFile(t *testing.T) {
	// Local/dev builds carry no ldflag (Version == ""). The chart-file read
	// is the documented fallback so dev still shows something meaningful.
	orig := Version
	t.Cleanup(func() { Version = orig })
	Version = ""

	dir := t.TempDir()
	path := filepath.Join(dir, "chart-version")
	if err := os.WriteFile(path, []byte("1.9.1\n"), 0o644); err != nil {
		t.Fatalf("write chart file: %v", err)
	}
	t.Setenv("KYBER_CHART_VERSION_PATH", path)

	if got := resolveDisplayVersion(); got != "1.9.1" {
		t.Errorf("resolveDisplayVersion() = %q, want %q (must fall back to chart file when ldflag empty)", got, "1.9.1")
	}
}

func TestResolveDisplayVersion_EmptyWhenNeitherPresent(t *testing.T) {
	// No ldflag and no chart file (bare `go run` with no env): the field is
	// empty, and the PWA renders "—" for it. Returning "" rather than erroring
	// keeps /api/v1/version a 200 in all environments.
	orig := Version
	t.Cleanup(func() { Version = orig })
	Version = ""

	dir := t.TempDir()
	path := filepath.Join(dir, "does-not-exist")
	t.Setenv("KYBER_CHART_VERSION_PATH", path)

	if got := resolveDisplayVersion(); got != "" {
		t.Errorf("resolveDisplayVersion() = %q, want \"\" (no ldflag, no chart file)", got)
	}
}

package main

import (
	"os"
	"strings"
)

// BuildSHA is the short Git SHA of this control-plane build.
// Injected at build time via -ldflags "-X main.BuildSHA=…" in
// images/control-plane/Dockerfile. Empty when building locally without
// passing the --build-arg (e.g. `make build` or `go run ./cmd/control-plane`).
var BuildSHA string

// BuildDate is the RFC3339 build timestamp. Same injection mechanism as
// BuildSHA; same empty-default semantics for local builds.
var BuildDate string

// Version is the user-facing release version baked into this control-plane
// build. Injected at build time via -ldflags "-X main.Version=…" exactly like
// BuildSHA — the release tag (with its leading "v" stripped) on the
// release.yml path, or `git describe --tags` on the build.yml `:latest`/canary
// path. Empty when building locally without passing the --build-arg.
//
// Because it rides inside the same image as BuildSHA, the displayed version and
// the running commit are injected from one build and can never disagree — this
// is the fix for kyber#482, where the chart-rendered version perpetually lagged
// the deployed image by one release. See resolveDisplayVersion.
var Version string

// chartVersionPath is the file the Helm chart renders with {{ .Chart.Version }}.
// Overridable via KYBER_CHART_VERSION_PATH for tests / alternate layouts.
const chartVersionPath = "/etc/kyber/chart-version"

// readChartVersion returns the chart version string with trailing whitespace
// trimmed, or "" if the file is missing or unreadable. Called once at boot
// in main.go; the resulting string is stored on the Server and returned as-is
// by /api/v1/version.
func readChartVersion() string {
	path := chartVersionPath
	if override := os.Getenv("KYBER_CHART_VERSION_PATH"); override != "" {
		path = override
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// resolveDisplayVersion returns the user-facing version reported on
// /api/v1/version (the chartVersion field). The build-injected Version wins
// when present (the deployed-image case), so the displayed version and BuildSHA
// always come from the same build and cannot diverge (kyber#482). It falls back
// to the chart-rendered file only on local/dev builds where no ldflag was
// passed, and returns "" when neither source is available.
func resolveDisplayVersion() string {
	if Version != "" {
		return Version
	}
	return readChartVersion()
}

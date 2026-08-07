package api

import "embed"

// pwaAssets contains the built PWA static files embedded at compile time.
// The pwa_dist/ directory is populated by `make pwa-build` (copies apps/embedded-pwa/dist/ here).
// A placeholder index.html is committed so `go build` succeeds without running pwa-build first.
//
//go:embed all:pwa_dist
var pwaAssets embed.FS

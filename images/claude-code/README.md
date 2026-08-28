# kyber-claude-code image

Container image for Kyber agents that use the Claude Code CLI. Built from
`Dockerfile` in this directory; entry-point script is `start-claude.sh`.

## Layout

- `Dockerfile` — image build. Pins the baked-in Claude Code version via
  the `CLAUDE_CODE_VERSION` build-arg; the chart's
  `runtime.claudeCode.version` value flows in via CI's `yq` step. The
  resulting value is surfaced inside the running pod as
  `KYBER_RUNTIME_DEFAULT_VERSION` for `start-claude.sh` to read.
- `start-claude.sh` — entry-point. Handles credentials refresh, identity
  repo setup, the boot-time runtime install (see below), model
  resolution + `[1m]` opt-in, marks the resolved identity-repo launch path as
  trusted in Claude Code's durable state, and performs the actual
  `tmux new-session "claude $CLAUDE_ARGS"` launch.
- `start_claude_test.go` — integration tests for `start-claude.sh`.
  Tagged `//go:build integration`; run with `go test -tags=integration
  ./images/claude-code/`.

## Boot-time runtime install (kyber#377 / PR-C)

`start-claude.sh` checks two env vars early in the boot sequence:

- `KYBER_REQUESTED_CC_VERSION` — operator-controlled requested CC
  version, resolved by the controller from `spec.runtimeVersion` or the
  fleet default. Empty means "use the baked-in pin."
- `KYBER_RUNTIME_DEFAULT_VERSION` — the baked-in pin from the build-arg
  (see Dockerfile `ENV KYBER_RUNTIME_DEFAULT_VERSION=...`).

The install branch fires iff `KYBER_REQUESTED_CC_VERSION` is non-empty
and the resolved version differs from the installed version. `latest` is
resolved through the npm registry before comparison, so consecutive boots do
not reinstall the same release.

Installation delegates to the shared `images/shared/kyber-harness-install`
helper. It installs under a temporary prefix, verifies the staged binary,
copies the package beside the live global package, and swaps directories only
after a second verification. Signals and failed post-swap verification restore
the prior package; a later boot also repairs a SIGKILL left between the two
renames. Install failure is **non-fatal** because the previous verified harness
remains live. The runtime report surfaces the requested-version mismatch.

### Charset guard (security-critical)

Before any shell interpolation, `start-claude.sh` validates
`KYBER_REQUESTED_CC_VERSION` against `^[0-9A-Za-z.\-]+$` plus a 64-char
length cap. The kubebuilder CRD pattern enforces the same shape
server-side; the script re-validates as defense in depth.

**Do not remove this guard.** The value flows into a shell command on a
pod that holds OAuth credentials. A future CRD change that mistakenly
opens the pattern (e.g., to allow spaces or `$`) without updating the
shell guard would become a shell-injection vector. The two pieces are
intentionally coupled.

If you legitimately need to broaden the allowed character set, update
**both**:

1. `kubebuilder:validation:Pattern` on `Agent.spec.runtimeVersion` in
   `pkg/api/v1/agent_types.go`, then `make generate` to regenerate the
   CRD.
2. The bash regex guard in `start-claude.sh`'s
   boot-time-install block (search for `KYBER_REQUESTED_CC_VERSION`).

## Data-driven `[1m]` opt-in

The 1M-context opt-in suffix (`[1m]`) is applied based on
`KYBER_MODEL_CONTEXT_WINDOW` (resolved by the controller from
`pkg/tokenreport/limits.go`'s `LimitFor`, with a 200K floor for unknown
model IDs). The previous hardcoded shell `case` mapping concrete
model IDs to `[1m]` was removed in PR-C — a brand-new 1M model can now
be adopted without editing this script.

Family aliases (`sonnet`/`opus`/`haiku`) are NOT given `[1m]` — Claude
Code's Max subscription path maps the alias to its highest concrete
model already.

## Testing

The integration test file `start_claude_test.go` covers OAuth refresh,
identity-repo setup, and (as of PR-C) the boot-time install + charset
guard + `[1m]` gate. Tests that exercise the new boot-prep block set
`KYBER_BOOTPREP_DRY_RUN=1` so the script exits right after the boot-
prep block — no real `claude`, no real `tmux`. The `npm` binary is
stubbed via `PATH` for install-branch tests. The shared helper's activation,
verification failure, stale-state recovery, and signal rollback tests live in
`images/shared/harness_install_test.go`.

```bash
# From the repo root:
go test -tags=integration ./images/claude-code/

# Just the PR-C tests:
go test -tags=integration ./images/claude-code/ -run TestStartClaude_PRC -v
```

## Related

- `docs/design/2026-05-29-runtime-model-management-design.md` — authoritative
  runtime + model management design.
- `docs/operator/adopting-cc-version.md` — operator how-to for the
  install path.
- kyber#374 — epic.
- kyber#175 — runtime-version reporting (baseline this PR builds on).
- kyber#371 — sidecar-convergence hardening (bounds the lazy-adopt
  blast radius).

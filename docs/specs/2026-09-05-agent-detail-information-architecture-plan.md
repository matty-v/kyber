# Agent detail information architecture implementation plan

Status: approved agent detail complete; Settings follow-up in progress

## Objective

Make the agent detail view faster to scan on desktop and mobile without removing
configuration or operational capabilities. Organize the page around operator
tasks rather than one tab per implementation feature.

## Approved structure

- **Observe:** Overview, Activity, and Shell.
- **Automate:** Jobs and Webhooks.
- **Configure:** General, Comms, Secrets, and A2A.

Configuration moves out of Overview and the header action menu. Unhealthy or
actionable states remain visible on Overview. Low-frequency diagnostics remain
available through progressive disclosure. Desktop uses a persistent agent-local
rail; mobile uses a full-screen navigation sheet. Every destination has its own
URL so refresh, browser history, and deep links preserve location.

## Acceptance criteria

- Agent-local navigation exposes all destinations directly without nested tabs
  or dropdown selectors.
- Overview shows actionable warnings first, then the terminal peek, token budget,
  and live pod resources; it contains no always-expanded configuration editors.
- Settings retains startup prompt, session resume, request/reply, model, harness
  version, resources, Comms, Secrets, and public capability configuration.
- Skills remain discoverable as read-only capability evidence.
- Jobs and Webhooks remain fully functional under Automations.
- The header action menu contains actions, not routine configuration.
- Desktop secondary navigation is easy to scan; mobile uses a compact full-width
  selector and avoids two-column card layouts.
- Existing action confirmation and mutation behavior remains covered by tests.
- PWA type-check, tests, build, design detector, and desktop/mobile screenshot
  capture pass before deployment.
- The exact worktree is warm-reloaded to `https://kyber-dev-gcp.voget.io` and
  exercised before the branch is pushed.

## Checkpoints

1. **IA and responsive navigation** — complete
   - Consolidate primary tabs and add responsive secondary navigation.
   - Preserve deep feature components without changing their API behavior.
2. **Overview hierarchy and progressive disclosure** — complete
   - Build the compact operational summary.
   - Move configuration editors into Settings.
   - Collapse healthy low-frequency diagnostics while auto-exposing problems.
3. **Verification and dev warm reload** — complete
   - Update unit and screenshot coverage.
   - Run type-checks, tests, build, and the design detector.
   - Warm-reload the exact worktree and inspect desktop/mobile views.
4. **Durable handoff** — complete
   - Record verification evidence here.
   - Commit, push the branch, and report the branch plus dev URL.

## Verification evidence

- TypeScript checks passed for `packages/pwa-views` and `apps/embedded-pwa`.
- `packages/pwa-views`: 85 files and 776 tests passed.
- `apps/embedded-pwa`: 2 files and 8 tests passed.
- Focused desktop/mobile Playwright capture passed for Overview, General,
  route-addressable destinations, and the agent navigation rail/sheet.
- Production embedded-PWA build passed.
- `impeccable` could not run because the pinned npm package installed without
  exposing its documented executable; rendered screenshots were inspected
  manually against the repository design standard.
- Dev image:
  `us-central1-docker.pkg.dev/datawire-dev/kyber-dev/control-plane:worktree-20260905234713-44cc566`
- Dev URL: `https://kyber-dev-gcp.voget.io`

## Current next action

The follow-up dev build was approved. Update the pushed branch and retain this
document as the implementation record.

## Follow-up refinement

Matt approved extending the same information architecture to fleet Settings:

- Name the agent capability publisher for its purpose: **A2A**.
- Move Metrics and Logs from global navigation into Settings while preserving
  direct URLs and agent-filtered log links.
- Use the same shared desktop rail and mobile navigation sheet for agent-local
  pages and Settings.

Follow-up verification:

- Shared Agent and Settings navigation lint and focused unit coverage passed.
- Desktop/mobile Playwright capture passed: 38 scenarios across both projects.
- The embedded PWA unit suite passed: 2 files and 8 tests.
- The PWA suite passed 774 of 776 tests in the full concurrent run; the two
  unrelated `AddWebhookWizard` timeouts passed together on an isolated rerun
  (12 of 12).
- Production builds passed for both `packages/pwa-views` and
  `apps/embedded-pwa`.
- Follow-up dev image:
  `us-central1-docker.pkg.dev/datawire-dev/kyber-dev/control-plane:worktree-20260906042647-eabeacb`

PR review follow-up:

- Canonicalized legacy `/metrics` and `/logs` routes into Settings while
  preserving query-string filters and cluster route prefixes.
- Added Escape-to-close, initial/return focus management, and background scroll
  locking to the mobile local-navigation dialog.
- Added regression coverage for both fixes; the full PWA suite passes with 86
  files and 779 tests.

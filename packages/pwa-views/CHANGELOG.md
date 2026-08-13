# @matty-v/kyber-pwa-views

## 0.22.0 — 2026-08-13

### Added
- Machine provider types and creation forms now support the explicit `static`
  existing-node provider and the local `fake` managed-provider simulator.
  The legacy `mock` provider remains accepted as a compatibility alias for
  `static`.
- Settings → **Updates**: what version this cluster runs, what is available, and
  how it takes new ones.
  - **Source** is a choice between published releases (the default) and tracking
    every change as it lands. Opting into the latter requires confirming a
    warning that it means running unreleased code, and the warning stays visible
    while it is selected. Switching back needs no confirmation — going back
    takes on nothing.
  - **Installing** is always operator-initiated: a button here, or
    `POST /api/v1/updates/apply`. Nothing installs itself; automatic apply is
    not offered because it is not implemented, and an option that silently did
    nothing would be worse than its absence.
  - Three distinct "cannot install" states are reported separately rather than
    collapsed: the control plane has no apply path, this cluster may not use it
    (ArgoCD), or there is simply nothing newer. Telling an operator the feature
    does not exist when their cluster is managed by something else sends them to
  the wrong place.
  - A failed check is shown rather than rendering as "up to date".

## 0.21.2 — 2026-08-12

### Fixed
- Settings → Model discovery no longer offers an Anthropic key field on installs
  that cannot accept one. Where model discovery is off (`runtimeDetect.enabled:
  false`) the control plane has no Secret to write into, so saving always failed
  — and an operator learned this only after typing a live credential into the
  form. The card now explains the state and names the value that turns it on.

  Driven by a new `supported` field on `GET /api/v1/settings/anthropic-key`,
  not by an HTTP status: a 503 can equally mean a rolling control plane or a
  tunnel with no origin, and telling an operator to change their values over a
  transient blip would be its own wrong answer. A control plane that predates
  the field omits it, and the card behaves as it always has.

## 0.21.1 — 2026-08-10

### Changed
- Dependency maintenance only, no source change: minor/patch bumps of the
  Radix UI primitives, `@fontsource/*`, `@tanstack/react-query` and the rest
  of the npm-minor-patch group. Version bumped so the refreshed dependency
  set actually reaches holocron — the library publishes on a version bump.

## 0.21.0 — 2026-08-07

### Added
- Metrics tab Token Usage table gains a "Cache write" column so all four
  priced token components (input, output, cache write, cache read) are
  visible.

### Fixed
- Cost accuracy: the control plane now accumulates output tokens (the Output
  column previously always showed 0) and prices cache-creation (cache-write)
  tokens, so `costUsd` includes both components. The per-agent `TokenUsage`
  type gains an optional `tokens.output` field; older control planes that
  omit it keep rendering as 0.

## 0.20.2 — 2026-08-07

### Changed
- Comment-only: repository doc paths referenced in source comments follow the
  design docs' move to `docs/design/`. No behavior change.

## 0.20.1 — 2026-08-07

### Fixed
- MSW mock handlers now cover `/api/v1/cluster-info` and `/api/v1/version`.
  cluster-info is the embedded app's bootstrap gate, so without it every
  mock-backed page (dev:mocks and the whole Playwright screenshot suite)
  rendered the "Could not reach the kyber control plane" error instead of
  the UI. The version handler stops the sidebar showing "version
  unavailable" under mocks.

## 0.20.0 — 2026-08-06

### Added
- The Discord Comms card now reports whether its Gateway sidecar is connected,
  still starting, waiting for a pod restart, stopped with the agent, or
  degraded. It also surfaces Kubernetes waiting reasons and restart counts so
  invalid credentials and crash loops are diagnosable without `kubectl`.
- Discord configuration changes now converge automatically once the agent is
  idle. Operators can still choose Restart pod to apply immediately.

## 0.19.0 — 2026-08-05

### Added
- Codex agents now use an in-pod `codex login --device-auth` flow for ChatGPT
  subscriptions during creation and re-authorization. The wizard no longer
  asks operators to copy a local `auth.json`; agent detail displays the device
  login session and resumes automatically after authorization.
- Codex creation also supports an explicit OpenAI API-key mode. API keys are
  stored separately and never enter the subscription device-login flow.

## 0.18.1 — 2026-08-05

### Fixed
- Saving Telegram comms with an empty allowed-users list now says why instead
  of doing nothing. The Save button called `put` only when the list was
  non-empty and otherwise returned silently, so the click produced no request,
  no error and no visible change — the operator's reasonable read being that
  the page was broken. It now shows the same message the Discord card already
  showed for the identical rule.

  The rule itself is not cosmetic: the sidecar is fail-closed and refuses to
  start without an allowlist rather than answer strangers, and as of kyber#684
  that applies to every runtime rather than Codex alone. (kyber#684)

## 0.18.0 — 2026-08-04

### Added
- Agent detail now explains the two states where the controller refuses to
  build a pod at all, instead of showing an agent with a blank status and no
  clue why. A missing runtime image ("this cluster can't run the codex
  runtime") and an unresolvable model each render as a danger card, ordered
  ahead of the other badges because they explain why the rest of the page is
  empty. Both name the install-level fix and say explicitly that restarting
  the agent will not help — the action operators reach for first, and the one
  that cannot work here.

  `ModelUnresolved` had been set by the controller since kyber#376 but never
  reached the wire, so it had never been visible in the PWA. Both flags plus
  the controller's `status.message` are now surfaced on the agent DTO.
  (kyber#674)

## 0.17.0 — 2026-08-03

### Added
- The Create Agent wizard now offers the OpenAI Codex runtime, current
  ChatGPT-subscription models, and a write-only `auth.json` import flow. Codex
  agents can configure Telegram and Discord in the same initial wizard flow as
  Claude Code agents. Telegram configuration now requires an explicit numeric
  user allowlist and supports the runtime-neutral Codex Telegram sidecar.

### Changed
- Agent actions now call `spec.runtimeVersion` the **Harness Version** instead
  of labeling the runtime-neutral control as a Claude Code version. Claude and
  Codex agents each get their own npm-backed version picker. Codex model
  choices come from the signed-in agent's subscription-aware `model/list`
  catalog instead of leaking Claude models into the selector; manual overrides
  remain available for newly released versions and models.
- Settings now groups fleet defaults by agent harness. Claude Code and Codex
  each have independent model and harness-version defaults backed by their
  live catalogs, preventing a Claude default from leaking into a Codex agent.
  Provider discovery is separate: Anthropic retains its write-only API-key
  controls, while OpenAI explains and reports subscription-derived discovery.

### Fixed
- **The test suite no longer fails at random on many-core machines.** Vitest
  defaults to about one worker per logical CPU, but this suite's cost is jsdom
  memory rather than CPU — each worker builds its own DOM and React module
  graph, so a large pool oversubscribes RAM and slows every worker at once. On a
  20-core box that stretched the longest `AddWebhookWizard` flows from ~340ms
  past the 5s `testTimeout`, failing 1–4 tests per run at random while the same
  file passed alone. The pool is now capped at 4 workers, which measured both
  faster (37s vs 49s) and stable across repeated full-suite runs. No test
  changed — this was a runner-configuration problem, not a behavioral one.

## 0.16.0 — 2026-08-02

### Changed
- **The Activity panel opens on the last 24 hours instead of the last 7 days** (kyber#669), with a **Load earlier** control that widens to 3 days and then 7. For a busy agent the 7-day window was an 84.7 MB response — reading and parsing it peaked around 330 MB of browser memory, so the panel simply never rendered. Asking for a day up front is what keeps the common case small; at the new default the same agents return well under a megabyte. The `windowDays` knob and this control were both designed when the panel was built (kyber#446) and never wired up.

### Fixed
- **The truncation banner said the opposite of what the server does.** It read *"showing the earliest part of the 7-day window; the most recent activity may be missing"* — which was accurate, and was the real bug: a capped read returned a week-old prefix and dropped everything recent. The API now retains the **newest** lines when it has to cap a response, so the banner explains that the window is shown from the most recent activity backwards.

## 0.15.0 — 2026-07-31

### Added
- **Comms tab on AgentDetail** (kyber#664). A per-agent home for the channels a human uses to reach the agent: **Telegram** and **two-way Discord**. Before this, Telegram could only be configured when the agent was created — there was no way to enable it later, turn it off, or replace a leaked bot token — and Discord could only be configured with `kubectl`. Each channel is a card showing on/off state, its own form, and a **Turn off** action behind a confirm. Discord collects the bot token, the user allowlist (required — an empty one is fail-closed, so nobody could reach the agent), optional server/channel allowlists, and the *Only when mentioned* toggle; IDs are validated as Discord snowflakes so a pasted channel *name* or URL is caught inline instead of silently mismatching at runtime.
- **Discord in the Create Agent wizard.** Optional and collapsed by default — it needs a bot that already exists, so most agents skip it. When enabled, it is validated **before** the agent is created (a bad ID must not leave a real agent with a half-configured channel) and wired **after**, through the same `PUT /comms/discord` endpoint the Comms tab uses, so there is one implementation of "wire a channel". If that call fails the operator stays on the page with *"Agent created, but Discord setup failed"* rather than a misleading create-failed error.

### Changed
- **`ChannelDef` in the wizard's auth step is now genuinely extensible.** Its field slot was hardcoded to `telegramBotToken`, so the "add a row here" comment it carried was not true for a channel needing more than one input. Fields are now a typed list of text/secret/toggle entries, and both channels are expressed through it.

### Notes
- Saving a channel does **not** restart the pod. Neither channel takes effect on a live pod, but rolling it destroys the agent's session, so the card surfaces *"Saved, but not live yet"* with an explicit **Restart pod** action and says plainly that restarting ends the current session.
- The older outbound-only Discord webhook is deliberately not surfaced here. It keeps working and stays configured the way it always was.

## 0.14.1 — 2026-07-29

### Fixed
- **Blank-page crash in the embedded PWA (react-router dual copy).** The `react-router-dom` devDependency was pinned to `^7.16.0` while `apps/embedded-pwa` depends on `^6.28.0`, so npm nested a second copy under `packages/pwa-views/node_modules`. `main.tsx`'s `<BrowserRouter>` resolved to v6 while this package's `App.tsx` `useLocation()` resolved to v7 — two distinct React contexts, so the hook could not see the provider and the app died at first render with `useLocation() may be used only in the context of a <Router> component`, rendering a blank page. The devDependency is now aligned to `^6.28.0`; the **peer range stays `^6.28.0 || ^7.0.0`**, so holocron can continue to consume this package against v7 (a published consumer dedupes against its own single copy and was never affected). Regression introduced in #414.

## 0.14.0 — 2026-07-21

### Changed
- **Agent Activity tab redesign** (kyber). The Activity tab is now just the structured history — the History / Pod Boot Log sub-tabs and the Raw terminal toggle are gone. A **Recent conversation** section pins the latest few turns up top (expanded, collapsible); the full per-session history sits below, each session collapsed. Sessions are labeled by a start→end time range with an **Active today** tag, so a long session that ran into today no longer looks like it's missing. Every turn now shows a timestamp, and a **Export .txt** button next to Refresh downloads the whole history.

### Added
- **Subagent (behind-the-scenes) work is now visible.** `parseTranscript` no longer drops `isSidechain` records — it groups each contiguous run of subagent work into a collapsible **SUBAGENT** block (violet, badge) nested at its place in the conversation, with the subagent's own text and tool calls inside. Previously all subagent activity was hidden. `transcriptToText` renders it in the export too.

## 0.13.0 — 2026-07-13

### Changed
- **Agent History sessions are now collapsible accordions** (kyber). Each session in the structured History view is an expand/collapse section: the most recent session is expanded by default and older sessions are collapsed, each showing a first-message preview in its header so it stays scannable. Replaces the static session divider.

## 0.12.0 — 2026-07-13

### Added
- **Structured multi-session Agent History** (kyber). The Activity → History tab now renders the agent's Claude Code session transcript as a structured conversation — user messages (with a Telegram channel chip), assistant replies, collapsible thinking blocks, and collapsible tool call/result pairs — grouped into clean per-session boundaries across a rolling 7-day window, with a derived "previous-session recall" header. A "Raw terminal" toggle keeps the original tmux pane snapshot available. Client-side parse of the existing `GET /logs?source=transcript` endpoint (kyber#446); no backend/contract change.

## 0.11.0 — 2026-07-11

### Added
- **Per-agent session reset from the dashboard Context Pressure widget** (kyber#618). Each running agent row now carries a reset button that opens the existing "Restart session?" confirm and fires `POST /restart-session` — the same action already on the agent detail page (#128) — so a high-context agent can be reset without drilling into its detail page. The button is a sibling of the row link (not nested inside the anchor), gated on `phase === 'Running'`, and shows a pending/disabled state while the request is in flight. Reuses the existing confirm copy, success/error toast, and server-side 429 cooldown; no backend change. Replaces the removed nightly `falcon-session-reset` automation with an on-demand, operator-triggered control; working and idle agents are treated identically (the confirm already states the context loss).

## 0.10.0 — 2026-07-06

### Changed
- **The Fleet landing tab is now an agent-centric Dashboard.** Replaces the old two-card Fleet overview at `/` with: a segmented agent-status bar (phase distribution), a recently-active list (linked rows, activity/stale flags, inline NeedsAuth/OOM badges), a context-window pressure list (model + used/total window + threshold-colored bar), and a live read-only terminal peek (`tmux attach -r`) with an agent selector that auto-suspends when the tab is hidden. All widgets derive from the existing `GET /api/v1/agents` payload plus the existing exec WebSocket — no backend changes. Also colors the `NeedsAuth`/`MemoryExhausted` phases (previously neutral-grey) in the shared phase→tone map, so those states are distinct everywhere `StatusBadge`/`phaseStyle` is used. **Breaking:** the `FleetOverview` page export is removed and replaced by `Dashboard`.

## 0.9.8 — 2026-06-27

### Fixed
- **A crashed agent can be recovered from the PWA alone — no CLI** (kyber#599). The agent detail lifecycle ("More") menu now offers the working **Start** recovery for a `Failed`/`MemoryExhausted` agent and no longer the no-op **Restart pod**. Previously the crashed-phase menu returned `['restart', 'force-needs-auth']`, but `restart` (`desiredPhase=Restarting`) only ever transitions from `Running`, so it silently did nothing from a crashed phase — recovery required a `kubectl patch … desiredPhase=Running`. `lifecycleItemsInMore` now returns `['start', 'force-needs-auth']` for those phases; `start` (`desiredPhase=Running`) is a valid recovery that recreates the pod. `force-needs-auth` (#395) is unchanged, and the `Running`/`Stopped`/`Suspended`/`Starting` menus are untouched. No public API-surface change to this package; the per-phase menu items were extracted into an exported `LifecycleMenuItems` component for isolated testing.

## 0.9.7 — 2026-06-14

### Fixed
- **Agent/machine delete now sends the required `?confirm=<name>`** (kyber#565). The control plane's `DELETE /api/v1/agents/{name}` and the machine DELETE now require a `?confirm=<name>` query param matching the resource name (an always-on safety interlock in front of the destructive delete); a request without it is rejected `400`. `api.deleteAgent` / `api.deleteMachine` append `?confirm=${encodeURIComponent(name)}` so the consuming app (Holocron) keeps working against the new contract — the human confirmation already happens in the delete dialog before the call. No public API-surface change to this package (the `deleteAgent(name)` / `deleteMachine(name)` signatures are unchanged); a behavior fix to stay compatible with the breaking control-plane DELETE contract.

## 0.9.6 — 2026-06-07

### Added
- **Unpriced models fail loud on the Token Usage panel** (kyber#487). When a model has no rate in the (now feed-derived) provider-rates source, the cost cell renders `—` with an `· unpriced` badge (tooltip: "No rate for this model in provider-rates — add it to fix the cost") instead of a believable `$0.0000`. Driven by a new `priced?: boolean` on `MetricsTokenUsage` (`TokenUsageResponse.priced` on the wire); only an explicit `priced === false` counts as unpriced (older payloads omitting the field still render cost), mirroring the `contextWindowKnown` idiom. Unpriced rows sort last.

## 0.9.5 — 2026-06-04

### Added
- **Agent logs now have a Live / Archive source toggle** (kyber#431). The agent detail log viewer can switch from the live pod-stdout tail to an **Archive** source backed by durable, off-cluster retention: pick a from/to time range and press **Load** to read every line that agent emitted in that absolute window, surviving pod restarts. Live mode is unchanged (follows new output; bounded by the current pod lifetime). Drives the new `GET /api/v1/agents/{name}/logs?source=archive&since=<RFC3339>&until=<RFC3339>` read path; `LogStreamOptions` gains `source`/`since`/`until` and `logStream` sets them in archive mode (no `follow`/`tail`).

## 0.9.4 — 2026-06-03

### Changed
- **Agent Activity Breakdown durations are now human-readable** (#426). Each duration cell on the Metrics tab renders as a compound duration showing the two most-significant units by magnitude (`7d 23h`, `1h 38m`, `12m 46s`, or `59.4s` for sub-minute) instead of raw seconds like `689402.0s`. Display-only — the `/api/v1/metrics/activity` payload stays in seconds; the `—` empty-cell guard and the Working Time Trend panel are unchanged. Formatting lives in a new vitest-covered `lib/duration.ts` helper.

## 0.9.3 — 2026-06-02

### Changed
- Release-automation validation bump — **no functional change** since 0.9.2. Confirms the release-coupled Holocron auto-bump is now fully hands-off: with the Kyber App granted Pull requests: Write, the `bump-holocron` job opens the Holocron PR itself (no manual bridge).

## 0.9.2 — 2026-06-02

### Changed
- Release-automation validation bump — **no functional change** since 0.9.1. Cut to exercise the new release-coupled publish + Holocron auto-bump pipeline end-to-end (kyber#420 autonomous auto-publish fix + kyber#421 release-coupled Holocron dependency bump). Carries the #417 "agent activity on the list" feature forward.

## 0.9.1 — 2026-06-02

### Added
- **Agent activity on the list** (#417) — each Agent list entry surfaces "Idle &lt;relative-time&gt;" or "Working" text alongside the existing activity dot, reusing `AgentActivityBadge` verbatim. Desktop table appends the badge in the Status cell; mobile card places it on its own line below the header row (no overflow). Visible-by-absence: no-activity agents keep their exact prior layout.

## 0.9.0 — 2026-06-02

### Changed
- **react-router-dom 7 support** (#413). Widened the `react-router-dom` peer range from `^6.28.0` to `^6.28.0 || ^7.0.0` so consumers can move to react-router-dom 7 without an `ERESOLVE` block — this unblocks holocron's Dependabot PR #4 (react-router-dom 6 → 7). v7 keeps the v6 component-router API this package uses (`Routes`/`Route`/`Link`/`NavLink`/`useNavigate`/`useLocation`/`useParams`/`useSearchParams`/`MemoryRouter`), so no source changes were required; verified by running the full suite against react-router-dom 7.16.0 (added as a dev/test dependency). v6 remains supported.

### Added
- `Layout.test.tsx` — locks the `NavLink` `isActive`/`end` render-prop semantics and `Link` href behavior the Layout depends on, the surface most exposed to a react-router major bump.

## 0.8.0 — 2026-06-01

### Added
- **"Require re-auth" agent action** (#395) — operator-forced re-authorization for a wedged agent. Available from the agent-detail Lifecycle menu for the recoverable phases (`Running`, `Starting`, `Failed`, `MemoryExhausted`, `Stopped`, `Suspended`); routed through the existing confirmation dialog because it tears down a running pod. On success the agent query is invalidated and a "Forced {name} into re-authorization" toast shows.
- The PWA `AgentPhase` type now models `MemoryExhausted` (the backend already emits it; #272/#395), so the OOM phase renders and supports the new action.

## 0.7.0 — 2026-06-01

### Added
- Token-budget card flags an **estimated** percentage when the model's context window is unknown (#396). When the token-usage response carries `contextWindowKnown: false` (model absent from the `kyber-model-context-windows` ConfigMap → 200K floor), the card renders `≈ {pct}%` + a "· unverified window" hint and suppresses the "over budget" warning, instead of showing a confidently-wrong number. `TokenUsage` type gains an optional `contextWindowKnown` field.

## 0.6.0 — 2026-05-30

### Added
- Runtime + model management UI (epic #374). These views landed in source under 0.5.0 but never got a version bump, so the published package — and holocron — never received them. This release publishes them, and a new CI guard (test.yml `pwa-build`) blocks any future pwa-views change that doesn't bump the version.
  - Create-Agent model picker sourced from `GET /api/v1/available` (detected models with display name + context window), replacing the hardcoded list (#378).
  - Settings: **Anthropic API key** card (set/clear) that gates model detection; "detection unavailable" state when absent (#375).
  - Per-agent runtime version (`spec.runtimeVersion`) + fleet default (#376/#377).
  - Agent-detail `RuntimeVersionMismatch` / `ModelUnsupported` badges — the requested-vs-installed safety net (#379).

## 0.5.0 — 2026-05-25

### Added
- `ClusterIdentifier` component — renders `{cluster-name} {version}` in sidebar and mobile header (every tab). Shows `version unavailable` when the cluster is unreachable. Shows a refresh affordance (RotateCw button) when a post-mount upgrade is detected.
- `useLiveVersion` hook — polls `/api/v1/version` via React Query (30 s interval, visibility-gated). Returns `{ versionInfo, liveChartVersion, isStale, unreachable }`. Deduplicated across both components via shared query key.
- `DiagnosticsCard` (Settings page) now consumes `useLiveVersion` and shows a top-level **Cluster** row. Heading renamed "Version". Bespoke `useEffect` poller removed. Closes #350.

## 0.4.1 — 2026-05-25

### Changed
- Metrics tab empty-state messages for Token Usage and Node Resources panels no longer reference Prometheus/TSDB internals (#343).

## 0.4.0 — 2026-05-25

### Added
- Metrics tab (`MetricsTab`) — displays per-agent token usage, cost, and rate metrics from the Kyber API. Added in PR #329.

## 0.3.0 — 2026-05-10

### Added
- `RoutePrefixProvider` accepts an optional `backTo: { href, label }` prop. When set, `Layout` renders an arrow button (mobile) and a sidebar link (desktop) back out to the host's URL, and `CommandPalette` adds a top entry for the same target. Embedded mode (no provider) suppresses all of it. Used by holocron to surface "← Clusters" inside cluster views.
- New `useBackTo()` hook + `BackTo` type exported from the package barrel.

### Changed
- `RoutePrefixProvider`'s prop renamed from `value: string` to `prefix: string` for clarity now that the provider carries more than the prefix string. Consumers must update the prop name.
- `RoutePrefixContext` is no longer exported from the package barrel — the public surface is the Provider + the hooks.

## 0.2.0 — 2026-05-09

### Added
- `RoutePrefixProvider` + `useRoutePrefix()` + `usePrefixedPath()`. Holocron mounts the embedded views under `/c/<cluster-id>/*` and provides the prefix so internal navigation stays scoped to the active cluster's URL space. Embedded mode leaves the default empty prefix, behavior unchanged.

## 0.1.0 — 2026-05-09

### Initial release
- Extracted the kyber control-plane's PWA views, hooks, API client, and `ClusterContext` into a publishable workspace package. Consumed by `apps/embedded-pwa` (baked into the kyber Go binary) and matty-v/holocron (multi-install hub).

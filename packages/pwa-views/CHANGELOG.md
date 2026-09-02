# @matty-v/kyber-pwa-views

## 0.37.0 — 2026-09-01

### Added
- Agent Detail now provides a structured editor for operator-curated public
  capability manifests, including an exact privacy-safe preview, private
  evidence suggestions, availability/drift state, and explicit
  publish/update/unpublish actions.

## 0.36.8 — 2026-08-31

### Changed
- Dependency updates: `@tanstack/react-query` 5.101.4 -> 5.102.8.

## 0.36.7 — 2026-08-29

### Added
- Agent Detail now provides an opt-in **Bounded requests** toggle. The setting
  controls new authenticated request/reply submissions per agent; disabling it
  does not orphan work already in flight.

## 0.36.6 — 2026-08-28

### Changed
- Agent Detail now explains that a Claude Code OAuth credential may be missing,
  expired, or invalid while preserving the existing re-authorization action.

## 0.36.5 — 2026-08-28

### Added
- Agent Detail offers a confirmed **Repair runtime** action for
  `BrokenRuntime` agents and reports repair progress or failure without
  suggesting re-authorization.

## 0.36.1 — 2026-08-27

### Changed
- The Jobs tab's "Clear context after" checkbox no longer says the flag is
  Claude Code only: Codex agents honour it too now that their runtime
  registers the platform turn-boundary hooks.

## 0.35.1 — 2026-08-27

### Added
- Agent detail now shows live CPU, memory, and persistent-disk usage reported
  from inside the pod. Agent lists flag disks at 80% usage and escalate the
  indicator at the 90% maintenance reserve.

## 0.35.0 — 2026-08-27

### Added
- Added the text agent-request submission and status/result wire types for
  clients of the new scoped request API.

## 0.34.3 — 2026-08-27

### Fixed
- EKS Machine creation now populates the disk-size selector from the
  installer-owned encrypted launch-template profile and sends the selected
  size to the API.
- EKS is represented explicitly in the compute-provider wire type, so its
  managed Machine form no longer relies on the fallback provider branch.

## 0.34.1 — 2026-08-26

### Fixed
- The Codex device-login panel no longer spins forever when Kyber cannot read
  the login out of the agent at all. That case now says so, shows the reason,
  and offers **Start again** — a spinner over it is how the panel's own broken
  probe went unnoticed. A flow that is merely still starting is unchanged.

## 0.34.0 — 2026-08-26

### Changed
- **Codex device login is now shown in the app instead of in a terminal.** The
  agent-detail panel used to embed a read-only terminal and leave the operator
  to read a link and a one-time code out of it — and select them by hand, on a
  phone. It now renders them directly: the verification link is an ordinary
  anchor that opens in a new tab, the code sits in a monospace block with a
  one-tap **Copy code** button, and a countdown shows how long it is good for.
  Clicking **Start device login** shows a spinner until the code is ready
  rather than nothing at all.
- An expired code says so and offers **Start again**, deliberately without
  restarting the flow on a timer — silently burning one-time codes in the
  background is worse than one click.
- New `CodexDeviceAuthPanel` component and `useCodexDeviceAuthStatus` hook,
  backed by a read-only `GET /api/v1/agents/{name}/codex-device-auth`. The
  poll runs only while a login could be in progress, and pauses while a live
  code is on screen rather than stopping outright — an expired code has to be
  able to see its replacement arrive.

## 0.33.4 — 2026-08-25

### Added
- Machine details explicitly distinguish requested and effective availability.
  When a cost-optimized Machine is using reliable fallback capacity, the UI
  warns that reliable-rate cost is active and offers a provider-neutral,
  confirmed retry action when the provider supports it.

## 0.33.3 — 2026-08-25

### Added
- Machine and compute-provider wire types now include the additive,
  provider-neutral reliable-fallback capability, effective availability,
  fallback timing/reason, and cost-optimized retry fields. Existing providers
  may omit them and retain their current behavior.

## 0.33.2 — 2026-08-24

### Added
- Scheduled jobs can declare **Clear context after**, so each fire of a job
  starts from a clean conversation instead of accumulating every previous run.
  Shown as a badge on the job in both the card and table views. Claude Code
  agents only; accepted but inert on other runtimes.

### Fixed
- Editing a job no longer discards `clearContextAfter`. The editor rebuilt each
  job from name, schedule, prompt and exclusive only, so saving any change to a
  job that cleared its context silently turned that off.
- The **Exclusive** checkbox described holding a per-job lock, which was never
  what it did for the agent's work. It now says what it means: skip the next
  fire while the previous run of this job is still being worked.

## 0.33.1 — 2026-08-24

### Changed
- The PWA browser title now includes the active cluster name and restores the
  host title when the shared view unmounts.
- Removed the redundant “Fleet Command Console” sidebar tagline.

## 0.33.0 — 2026-08-24

### Added
- Agent Details gains a read-only **Skills** tab: what the agent can actually
  invoke, scanned from its own pod rather than read from GitHub. Each skill
  shows its description, where it came from (the agent's own identity repo, a
  vendored package, or a Kyber runtime image), and which runtimes it is
  loadable in.
- The tab also names what is broken: a skill committed to the repo that no
  runtime can load, a missing or malformed `SKILL.md`, a name collision where
  a vendored skill silently replaces the agent's own, skill directories
  written straight into a runtime home, and skills that are not pushed to
  GitHub — none of which survive a reprovision.
- Each skill is an expandable row: collapsed shows its health, name, origin and
  a one-line summary; expanding reveals the full description, which runtimes it
  loads in, its path, and any problems. A skill with problems opens by default
  and shows a problem count even when collapsed, so folding the list away can
  never hide one.

## 0.32.2 — 2026-08-24

### Changed
- Updated `lucide-react` from 0.468.0 to 1.33.0. Existing Kyber icon imports
  remain compatible and are covered by the library and embedded-PWA builds.

## 0.32.1 — 2026-08-24

### Changed
- The startup prompt helper text in the agent creation wizard now notes that
  the prompt is also sent when a resumed session starts (with session resume
  enabled), so interrupted work continues.

## 0.32.0 — 2026-08-23

### Added
- Agent Details gains a Session resume toggle (kyber#118): when enabled, the
  agent resumes its previous harness session after an unexpected restart
  (pod recreate, preemption, crash). Intentional session restarts still
  start fresh.

## 0.31.0 — 2026-08-23

### Changed
- Fleet Logs now defaults to one consolidated view across all agents and
  platform components, with agent and component filters and bounded automatic
  live refresh.
- Structured log fields are rendered as readable timestamp, level, agent,
  component, and message columns. Every entry retains expandable raw output,
  and the visible merged snapshot remains downloadable as NDJSON or text.
- Consolidated reads stay bounded by line and byte limits, cancel when filters
  change, keep URL navigation synchronized, and show pod/container identity in
  a dedicated source column.
- Periodic live refresh keeps the current snapshot visible until its replacement
  is ready and preserves the operator's scroll position away from the tail.

## 0.30.0 — 2026-08-23

### Added
- API client types and methods for read-only platform logging settings and
  managed pod/container discovery.
- Fleet Logs provides managed component/workload/pod/container discovery,
  live follow and archive windows, visible retention and effective verbosity,
  bounded output state, authenticated NDJSON/text downloads, and resource
  detail deep links.

## 0.29.0 — 2026-08-23

### Added
- Agents can configure a startup prompt during creation or from Agent Details.
  The prompt becomes the first user turn on every new Claude Code or Codex
  session, and edits use the existing restart-required workflow.

## 0.28.8 — 2026-08-22

### Added
- Agent Details Overview now includes a live read-only terminal peek for that
  agent, reusing the Dashboard terminal stream while leaving the interactive
  Shell tab unchanged. Agents without a running pod show a clear unavailable
  state instead of exposing a raw Kubernetes exec error.

## 0.28.7 — 2026-08-22

### Changed
- Agents are now always on: removed the unused scaling field, Suspended state,
  and manual Suspend action from the fleet UI.

## 0.28.6 — 2026-08-22

### Changed
- The model-rejected banner now leads with the common cause (a wrong model
  id, including one inherited from the fleet default) and renders the
  boot-time probe's actual diagnostic output, which names the rejected
  model even when the agent has no model of its own.
- A probe that failed without a clear verdict (auth, network, or an
  unrecognized error) now shows a warning banner with the diagnostic
  instead of rendering nothing.
- When fleet-defaults saving rejects a model id as unknown to every
  catalog, the card shows the reason inline and offers Save anyway for a
  model newer than the last detection poll.

## 0.28.5 — 2026-08-22

### Fixed
- Embedded Kyber PWAs now prompt to reconnect when the browser removes an
  expired session cookie, instead of rendering a raw missing-Authorization
  error on the current page.

## 0.28.4 — 2026-08-21

### Fixed
- The new-Agent capacity card now treats disk capacity as unknown while a
  scale-from-zero Machine has no node, instead of reporting zero disk and a
  false "won't fit" warning.
- Regional managed GKE Machines with no active Agent demand now display as
  Standby instead of appearing stuck in Provisioning. They return to
  Provisioning as soon as an Agent needs capacity.
- Initial regional scale-up now says capacity is starting; reclamation copy is
  reserved for Machines that actually entered Preempted or Replacing.

## 0.28.3 — 2026-08-21

### Fixed
- Agents whose Machine loses capacity now show that Kyber is waiting and will
  resume them automatically, instead of presenting the infrastructure event as
  an agent scheduling failure.
- Machine recovery and scheduling diagnostics are collapsed by default and can
  be copied from an expandable technical-details panel.

## 0.28.2 — 2026-08-20

### Added
- The agent Secrets dialog can import a key=value file as
  individual environment-secret rows, with complete validation before upload.

### Changed
- Adding a new per-agent secret no longer restarts a running agent. New file
  secrets appear live; new environment entries become available on the next
  pod start.

## 0.28.1 — 2026-08-20

### Fixed
- Agent detail pages no longer crash while a newly created Agent is waiting
  for the controller to report its initial lifecycle phase.

## 0.28.0 — 2026-08-20

### Added
- Provider-neutral Machine profile, availability class, location, management mode, provider reference, availability, and resolved-profile types.
- Portable compute capabilities, Machine preflight, and existing-capacity candidate API types.

## 0.27.4 — 2026-08-20

### Fixed
- First publish since 0.27.1 — it carries the 0.27.2 and 0.27.3 changes below,
  neither of which reached the registry. 0.27.2 was superseded inside the same
  push and never tagged; 0.27.3's publish job hung in the test step on Node 20
  (the suite passes on Node 22, which is what PR CI runs). The publish
  workflows now run Node 22, matching the tested path. No package code changes
  beyond the version.

## 0.27.3 — 2026-08-19

### Changed
- Agent terminals retain xterm.js while adding grapheme-aware Unicode,
  clickable web links, terminal search, explicit user-initiated copy/paste
  controls, debounced fitting,
  and bounded reconnect backoff.
- Interactive terminals expose a phone-only extra-key row for Escape, Tab,
  Shift-Tab, Ctrl-C, the tmux Ctrl-B prefix, arrows, and Enter. Read-only
  terminal peeks remain non-interactive and omit those controls.
- Terminal attaches force tmux's UTF-8 client mode so Claude Code's interface
  glyphs reach xterm intact even when the pod exec environment has no locale.
- Agent debug shells set an explicit UTF-8 locale, keeping tmux sessions
  launched through the `agent` and `peek` helpers Unicode-safe as well.

## 0.27.2 — 2026-08-19

### Changed
- Agent harness defaults now expose explicit Default-model and Latest-version
  choices for Claude Code and Codex. Fresh installs use those dynamic settings;
  operators can still enter concrete model or version pins.
- New agents now inherit the selected runtime's fleet model and harness-version
  defaults. The create wizard no longer pins or manually overrides a model.
- Change model now uses the selected agent's authenticated provider catalog and
  persists the choice on that agent. Settings no longer asks for a platform
  Anthropic key; harness-version discovery remains public and npm-backed.
- Agent details show the concrete runtime-observed model when the harness chose
  the default, rather than rendering a blank Model row.
- Authenticated model picker options and token-budget gauges now display only
  provider-reported context windows. Kyber rejects incomplete catalogs rather
  than rendering guessed values.
- The Agents table and mobile cards show the runtime-observed model for agents
  using the harness default, instead of leaving the Model column blank.
- Settings now starts with Version followed immediately by Updates, keeping
  cluster identity and upgrade controls together at the top of the page.
- The Settings API-key field is directly editable while masked; the eye button
  now only controls visibility instead of unlocking the input. The card also
  reports whether this browser already has an active API session.
- The Machines page keeps the New action available on existing-node installs,
  allowing additional pre-labelled Kubernetes nodes to be registered.
- Agent-list activity badges no longer show redundant dots, and idle labels
  stay on one line at narrow table widths.
- Settings places API-key rotation directly below the API Connection card so
  credential setup and lifecycle controls stay together.
- Settings cards now share the wider layout used by Updates.

### Removed
- The compact-density preference, including its Settings control and command
  palette action. Kyber now consistently uses the comfortable layout.

## 0.27.1 — 2026-08-17

### Changed
- Dependency updates, consolidating Dependabot PRs #87, #90, #91:
  `@xterm/xterm` 5.5 → 6.0, `@vitejs/plugin-react` 4.7.0 → 6.0.5, `sonner`
  ^2.0.7 → ^2.0.8, `@testing-library/user-event` ^14.6.3 → ^14.6.4. No
  functional changes intended; build and tests pass with the new majors.
  The `typescript` 5 → 7 bump (#89) is deliberately excluded: tsup's bundled
  `rollup-plugin-dts` cannot load against TS 7 yet, so the declaration build
  breaks. Revisit when the toolchain supports it.

## 0.27.0 — 2026-08-14

### Added
- **An available update is now visible from every page**, not only from
  Settings → Updates. The refresh icon beside the cluster version in the header
  appears when a newer version is available and opens a dialog describing it.
  The control plane has polled the release feed hourly for some time, but the
  only surface that read the answer was a card an operator has no reason to
  visit, so a cluster could sit months behind without anything saying so.

- **`UpdateDialog`**, the single component behind both entry points. It leads
  with the three facts an operator needs before deciding — which version, when
  it was released, and a link to the release notes — and then states what
  installing costs: the control plane restarts, and every agent loses its
  current session. Those consequences were previously written inline in
  `UpdatesCard`; sharing them is what keeps the header and the card from
  drifting into two differently-worded warnings, one of which would eventually
  be the dangerous one.

  On a cluster that may not install its own update (ArgoCD-managed, or
  `selfUpgrade` off, or pinned) the dialog drops the Install button and becomes
  an announcement that names what owns the cluster. Telling an operator nothing
  at all would mean a GitOps cluster never mentions being behind.

  On the `main` channel there is no release and therefore no notes and no
  publish date — the chart registry publishes neither. The dialog says so
  rather than rendering blanks.

### Changed
- `useUpdates` now polls every 5 minutes while idle instead of not at all, so
  the indicator appears without a reload. The control plane's own feed check is
  hourly, so this only re-reads a cached answer.
- `ConfirmDialog` gained `hideConfirm`, for a dialog that has something to say
  but nothing to authorize.

### Removed
- The header refresh icon no longer offers to reload a tab whose bundle is
  behind the cluster. One glyph, one meaning, and the meaning is now updates
  (Matt, 2026-08-14). The header still reports the cluster's live version, so a
  stale tab still tells the truth about the cluster.
- **`LiveVersionState.isStale`.** It existed only to drive that reload
  affordance and had no consumer left in either `kyber` or `holocron` — a
  computed value nobody reads is worse than no value. `useLiveVersion` still
  returns `versionInfo`, `liveChartVersion`, and `unreachable`. Technically a
  breaking change to the published type; permitted under 0.x, and noted here
  in case an unknown host destructures it.

## 0.26.0 — 2026-08-14

### Added
- **Restart pod** action on the agent detail page for an agent in `NeedsAuth`.
  That phase previously returned no lifecycle actions at all, so the More menu
  rendered empty and an agent parked on a credential that was already valid had
  no control in the UI — recovering it took a cluster-admin `kubectl annotate`
  on the Agent object to force a reconcile. The action rebuilds the pod using
  the credentials the agent already has.

  It is a new lifecycle kind, `retry-startup`, rather than the existing
  `restart`, and it fires `POST /api/v1/agents/{name}/start`. This is
  deliberate: `restart` sets `desiredPhase=Restarting`, which matches no state
  machine transition out of `NeedsAuth` and would have shipped a visibly
  clickable no-op — the same defect that removed Restart pod from the crashed
  phases. `desiredPhase=Running` is the edge that exists, and the control plane
  clears the recovery latch on that path, so the click recovers the agent even
  when the credential Secret is unchanged. One click is one pod rebuild; a
  still-bad credential lands back in `NeedsAuth` rather than looping.

  The Re-authorize panel is unchanged and remains the control for a credential
  that is genuinely bad — this sits beside it, not in place of it.
  `MemoryExhausted` needed no equivalent: it already offers Start, which goes
  through the same endpoint.

## 0.25.0 — 2026-08-14

### Added
- **Compact session** action on the agent detail page, sitting directly above
  Restart session in the More menu and gated on the same `phase === 'Running'`
  precondition. Fires `POST /api/v1/agents/{name}/compact-session`, which asks
  the agent's running runtime to summarize its own conversation and continue
  with a smaller context — the non-destructive alternative to a session reset.
  Works on both Claude Code and Codex agents (both use `/compact`).

  The confirm copy deliberately does not reuse restart-session's context-loss
  warning, and the success toast says "Compaction requested", not "Compacted":
  the API confirms the command was delivered to the runtime, and compaction
  itself completes asynchronously afterwards. Runtimes with no compaction
  support answer 501; the server also applies a 60s per-agent cooldown, which
  surfaces as the existing 429 error toast.

  Unrelated to the `transcriptCompaction` Helm values key, which reclaims
  duplicate archive objects in the log bucket.

- **Re-authentication prompt for an expired browser session**
  (`SessionExpiredDialog`, exported for embedded mode). Browser sessions are
  process-local to the control plane, so every restart invalidates every open
  browser. That previously surfaced as "invalid browser session" rendered
  inline by whichever query failed first — no explanation, no way out, on a
  page whose data would never load. The dialog opens on the control plane's
  new `session_expired` code, takes the API key, and reloads. A rejected key
  keeps the dialog open with an error rather than dropping the operator back
  onto the broken page; a generic 401 (bad API key) does not open it at all,
  since re-prompting there would loop.

### Changed

- **Agent detail More menu is grouped into three labelled sections** — Agent
  actions (compact / restart session), Pod actions (start, stop, restart pod,
  suspend, require re-auth), and Agent configuration (model, harness version,
  resources), with Delete unchanged below a separator. Each label renders only
  when its section has items, so a non-Running agent no longer shows an empty
  heading. Adds `sessionItemsInMore(phase)` beside `lifecycleItemsInMore` so
  both halves of the menu are pure, per-phase, and testable without mounting
  the detail page.

## 0.24.2 — 2026-08-13

### Security
- The embedded PWA exchanges its API key for an opaque HttpOnly, same-site
  browser session and removes legacy keys from localStorage. Raw control-plane
  credentials are no longer retained in browser-readable storage.

## 0.24.1 — 2026-08-13

### Changed
- Dependency maintenance only: upgrade the development and embedded-app
  React Router dependency from v6 to v7. The published library's peer range
  already accepts both major versions.

## 0.24.0 — 2026-08-13

### Added
- Machine provider types and creation forms now support the explicit `static`
  existing-node provider and the local `fake` managed-provider simulator.
  The legacy `mock` provider remains accepted as a compatibility alias for
  `static`.

### Changed
- Managed-machine creation now consumes provider-neutral profiles, locations,
  disk choices, and interruptible-instance capabilities from the control plane.
  GCE-specific zone lists and request vocabulary no longer live in the PWA.

## 0.23.0 — 2026-08-13

### Added
- **Upgrade confirmation.** Install now asks first, and the warning names the
  consequence an operator actually feels: the control plane restarts and this
  page goes offline for about a minute, then every agent restarts a few at a
  time and loses its session. It lists the agents at risk by name and counts
  them on the confirm button. Agents that are `Stopped` are excluded — nothing
  is lost restarting them — but anything mid-boot is included, because it is
  rolled too.
- **In-app upgrade notifications** (`useUpgradeProgress`). Phase transitions
  become toasts; failures never auto-dismiss. Both the phase source and the
  dedupe store follow from one fact: the control plane being replaced is also
  what serves this page, so the tab may reload mid-run. Phase is read from the
  polled Job and the dedupe record lives in `sessionStorage`, so the
  "upgrade finished" toast still fires on the far side of the reload. Terminal
  outcomes are announced only while recent — finished Jobs are retained for a
  week, and without that bound every new tab would re-announce days-old news.
- **`UpgradeBanner`**, rendered app-wide, driven by `lastRun` rather than
  component state so it is correct on a cold load mid-upgrade. The
  control-plane blackout is labelled *expected*; an operator who reads
  "connection lost" starts debugging a cluster that is working as intended.

### Changed
- `ConfirmDialog.message` accepts a `ReactNode` (was `string`), rendered in a
  `div` since a `p` may not contain a list. The panel gained
  `max-h-[90vh] overflow-y-auto` — a long message could otherwise push its own
  buttons off a phone viewport.
- The cluster header shows the **live** chart version rather than the one this
  tab loaded with, polling every 3s while an upgrade lands. The refresh
  affordance remains and now explains itself: the cluster has moved on, but
  this tab's code is still the old build.
- Update polling continues while the control plane is unreachable. It stops on a
  terminal phase, and the refetch most likely to fail is the one during an
  upgrade — stopping there left the banner and toasts permanently silent.
- Policy controls and agent creation are disabled while an upgrade is in flight,
  including the keyboard submit path.

## 0.22.0 — 2026-08-13

### Added
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

# Kyber Follow-Up Items

Tracked from production deployment and manual testing. Items are prioritized by impact.

**Last updated: 2026-04-17** — PR #59 closed 12 install-followup issues (#30–#35, #40–#41, #47–#50); see Closed section below.

## Open — High Impact

_(empty — no high-impact open items as of 2026-04-26)_

## Open — Medium

- [ ] **OAuth Phase 4: fast iteration harness** — `make oauth-iter` + kind cluster + mock Anthropic server + canned agent create. None implemented.

- [ ] **OAuth Phase 4: e2e test** — `test/oauth-e2e/oauth_e2e_test.go` against mock in CI. Covers full flow + pod refresh-on-boot + NeedsAuth injection. Not implemented.

- [ ] **OAuth Phase 4: prod smoke playbook** — `docs/testing/oauth-smoke.md` with step-by-step manual verification. Not created.

- [ ] **GCE adapter: use same-network firewall tags** — Worker VMs (compute_gce.go:183-241) only set Labels, not GCE Tags. No `kyber-worker` tag, no matching firewall rule. Terraform targets `k3s-server` tag only.

- [ ] **Expose `OVERLAY_MODE` in Agent CRD status** — The three-tier dispatcher sets `OVERLAY_MODE` in boot metadata but the field isn't in AgentStatus (agent_types.go). Surface it so operators see persistence mode in PWA and `kubectl get agent`.

- [ ] **Postgres + Redis connection retry on transient failure** — Connection pooling exists (SetMaxOpenConns, SetMaxIdleConns) but no reconnect logic. If Postgres or Redis restarts at runtime, control-plane falls back to in-memory stores permanently. **Status: PARTIAL**.

## Open — Lower Priority

- [ ] **Prod-e2e CI workflow** — Prod-e2e tests have moved to [matty-v/kyber-deploy](https://github.com/matty-v/kyber-deploy/tree/main/test/prod-e2e). A `prod-e2e.yml` workflow exists there for `workflow_dispatch` runs; nightly cron + PR label gate + Telegram notification on failure still to implement. Needs WIF SA with `compute.instances.simulateMaintenanceEvent`.

- [ ] **Cold-start timing instrumentation** — `ColdStartTimings` struct + 3 duration calculators exist in test/prod-e2e/phases.go (lines 53-83) but no phases populate the fields. Need `PhaseRecordFirstPrompt` + `PhaseAssertColdStartTimings` wrappers. Low effort. **Status: PARTIAL**.

- [ ] **Composite test: wire newer phases** — `TestFR_FullStack_VMToAgentToTelegram` has 14 phases but is missing PhaseLogStream, PhaseWholeDiskPersistence, and cold-start measurement. Each exists standalone.

- [ ] **PhaseSimulatePreemption** — Infrastructure fields exist in `FullStackEnv` (GCPZone, GCPProject) but phase not implemented. README documents this as not reliably testable from outside GCE. Likely stays manual/deferred.

- [ ] **Image version pinning** — Helm values use empty tags defaulting to Chart.AppVersion. No SHA256 pinning for reproducible upgrades. **Status: PARTIAL**.

- [x] ~~**Telegram comms for agents out of the box**~~ — **Won't do (2026-04-26).** Current onboarding (operator creates a bot via `@BotFather`, pastes the token into the agent's Telegram secret) is fine for solo operation. A managed bot pool would require pre-provisioning bots by hand against BotFather (which has no API), and a single shared Kyber bot conflicts architecturally with the per-agent `getUpdates` polling the `claude-plugins-official` Telegram plugin uses. Revisit if operator count grows. See `docs/wizard-roadmap.md` § Out of scope.

- [ ] **Machine stop flow timing** — Machine never reaches Stopped within 5 min timeout. `classifyStopping` + GCE `StopInstance` path too slow or gated. Needs debugging.

## Out-of-scope from 2026-04-15 prod-e2e plan — explicitly deferred

- OAuth / PKCE end-to-end test (decision: API-key auth is the prod-e2e path)
- `?mode=attach` coverage (only `mode=shell` in scope)
- Multi-agent concurrency (create 3 agents on one VM)
- API-key rotation
- Concurrent agent PATCHes
- Machine reboot during active session
- Quota / limits enforcement
- Per-run cost measurement

## Closed 2026-04-17 (PR #59 — install followups batch, closes #30–#35, #40–#41, #47–#50)

- [x] **[#47](https://github.com/matty-v/kyber/issues/47) api: audit K8sClient handlers for swallowed errors** — PR #59 audited all `K8sClient` call sites in `pkg/api/routes_*.go`; every handler now calls `slog.Error` before returning 500.

- [x] **[#48](https://github.com/matty-v/kyber/issues/48) control-plane: noisy `Failed to watch *v1.Secret` reflector errors** — PR #59 added `watch` on `secrets` to the control-plane ClusterRole; the reflector spam is gone.

- [x] **[#49](https://github.com/matty-v/kyber/issues/49) agent-base: `bootstrap.sh` is dead code for claude-code agents** — PR #59 deleted `bootstrap.sh` from `images/agent-base/`. `start-claude.sh` is the claude-code entry point; `entrypoint.sh` is the generic container entry point for other runtimes.

- [x] **[#50](https://github.com/matty-v/kyber/issues/50) agent-base: `/etc/cron.{hourly,daily,weekly,monthly}` not shadowed in bind-mount-home fallback** — PR #59 extended the bind-mount-home shadow list to cover all four periodic dirs alongside `/etc/crontab`, `/etc/cron.d`, and `/var/spool/cron/crontabs`. All surfaces now show `✅ yes` in `docs/agents-scheduled-jobs.md`.

- [x] **[#33–#35](https://github.com/matty-v/kyber/issues/33) docs(installation): GHCR ownership, /healthz body, tailscale --operator** — installation.md updated to clarify GHCR packages are user-owned (not org), document the empty `/healthz` body, and add `--operator="$USER"` to the Tailscale gotcha.

- [x] **[#30](https://github.com/matty-v/kyber/issues/30) chart: `storage.gcePD.enabled` default flipped to `false`** — Non-GCE clusters no longer need to opt out explicitly.

- [x] **[#31](https://github.com/matty-v/kyber/issues/31) chart: k3s env wiring gated on `compute.provider != "mock"`** — Mock-provider installs no longer inject GCE-specific env vars into the control plane.

- [x] **[#32](https://github.com/matty-v/kyber/issues/32) chart: add `postgresql.auth.existingSecret` support** — Operators can now point the chart at a pre-provisioned Secret for the Postgres password, avoiding the randAlphaNum regeneration-on-render problem. See `docs/installation.md § 5a`.

- [x] **[#40](https://github.com/matty-v/kyber/issues/40) agent PVC storage class configurable** — `storage.agentStorageClass` / `KYBER_AGENT_STORAGE_CLASS` env var; empty = cluster default. Agents no longer hardcode `kyber-pd`.

- [x] **[#41](https://github.com/matty-v/kyber/issues/41) agent-base hygiene** — `bootstrap.sh` deleted; token-reporter log path moved to `/persist/var/log/kyber-token-reporter.log`; cron shadow covers all periodic dirs.

## Closed 2026-04-17 (PRs #42–#46 — standalone cron persistence + CI hardening)

- [x] **API: `POST /api/v1/machines` returned 500 with no error detail** — Phase B's stricter CRD validation (missing `+optional` markers on gce-only fields, fixed in #38) caused the apiserver to reject mock-provider machines, but `createMachine` swallowed the err and returned a generic `"failed to create machine"`. The fix to #38 had been stranded for 8h because its image never shipped (see next item). PR #42 added `slog.Error` + err-in-body for both `createMachine` and `createAgent` so this class of incident is self-explanatory going forward. Follow-up audit filed as #47 (now closed above).

- [x] **CI: `build` workflow flakes on `TestHomePersistence_BindMountSurvivesRestart`** — The test shells out to `docker build`, which runs `apt-get install` against Ubuntu mirrors. Mirror flakes on GitHub-hosted runners stall apt ~540s and trip the test's 600s timeout, failing the whole `Unit tests` step and blocking image push for every merge. PR #43 gated the test behind `//go:build docker_integration` and moved it to a dedicated `agent-base-integration` job that only runs when `images/agent-base/**` changes (gates `push-runtime-base`). Unrelated merges are no longer hostage to the Ubuntu mirror lottery.

- [x] **Agents: cron daemon doesn't start on pod restart** — Containers have no init; nothing was starting `/usr/sbin/cron` for agents. PRs #44→#45 started the daemon from `entrypoint.sh` as root before su-to-kyber (both overlay and bind-mount-home branches) with stale-pidfile cleanup. In overlay mode `/etc/crontab`, `/etc/cron.d`, `/var/spool/cron/crontabs` already ride the upper layer; in bind-mount-home fallback those three paths are shadowed from `/persist/cron` via `mount --bind` with symlink fallback. Verified end-to-end against a live agent on a WSL2 dev cluster: both `crontab -e` and `/etc/cron.d/*` survive pod deletion and fire on the new pod.

- [x] **Docs: best practices for agent cron** — `docs/agents-scheduled-jobs.md` (PR #46) covers supported install surfaces, a copy-pasteable example, the overlay vs bind-mount-home mechanics, and debugging. Linked from both `installation.md` and `installation-wsl2.md` under "Create your first agent".

## Closed 2026-04-16 (codebase audit)

- [x] **Telegram bot.pid stale across restarts** — Boot-time cleanup in start-claude.sh lines 75-86 (commit 74f17b1). Checks if stale PID is running, deletes if not. Session-restart gap (POST /restart-session kills tmux but didn't re-run that cleanup) closed in PR #184: pkill bun + unconditional rm -f in last-claude-launch.sh heredoc.
- [x] **OAuth Phase 5: NeedsAuth agent phase** — Full flow shipped: phase constant, state machine transitions, exit-code-2 detection, `POST /v1/agents/{name}/oauth` re-authorize endpoint, PWA Re-authorize button (commit 6dac6a0).
- [x] **OAuth Phase 5: remove legacy oauthToken field** — Field never existed; implementation went straight to PKCE-only design.
- [x] **PWA styling improvements** — Tailwind dark theme, consistent styling across all pages.
- [x] **PWA nav bar layout** — `flex justify-around` on bottom nav (Layout.tsx:89), evenly distributed.
- [x] **Machine type + resources as selectable dropdowns** — `<select>` elements with predefined options (CPU_OPTIONS, MEMORY_OPTIONS, DISK_OPTIONS, MACHINE_TYPES, ZONES).
- [x] **VM/agent name auto-kebab-case** — `toKebabCase()` in CreateAgent + CreateMachine forms, plus API-side validation.
- [x] **API: validate runtime + machine exists** — `createAgent` handler validates both (routes_agents.go:237-258).
- [x] **api-key + `--channels` is silently unsupported** — `createAgent` rejects `authType=api-key` + `telegramEnabled=true` with 400 `VALIDATION_ERROR`: "Telegram requires OAuth authentication — api-key agents cannot use Telegram channels" (routes_agents.go:454-458, commit 2f7cb5ce, PR #18). Test at routes_agents_test.go:377. PWA also auto-clears `telegramEnabled` when switching to api-key (CreateAgent.tsx:380).
- [x] **api-key agents hang on "Use this API key?" prompt** — `start-claude.sh:314-322` (commit 2f7cb5ce, PR #18) appends `--bare` to `CLAUDE_ARGS` when `ANTHROPIC_API_KEY` is set without `CLAUDE_ACCESS_TOKEN`. `--bare` accepts the API key without the modal AND disables OAuth/keychain (the architectural reason `--channels` is incompatible — same PR also added the API-side validation above).
- [x] **Proactive credential sync: push keychain tokens to k8s Secret while agent runs** — `pkg/tokenreport/credential_sync.go` (`CredentialSyncer.Run`) watches `~/.claude/.credentials.json` and POSTs to the control plane on every change; wired into `cmd/token-reporter/main.go:81-88` so it runs in every claude-code agent pod alongside the usage reporter. Commit d8e767b (PR #17).
- [x] **Replacing-phase reconciler doesn't re-detect preemption of replacement VM** — `classifyReplacing` (machine reconciler.go:445-463) now returns `EventReplacementFailed` when the GCE instance status is `TERMINATED`, which fires `ActionRetryReplacement` with the existing retry budget. Regression test at `pkg/controllers/machine/reconciler_test.go:1002+`. Commit d8e767b (PR #17).
- [x] **Agent phase flashes "Failed" during operator-triggered Restart** — `classifyEvent` Running-phase branch (agent reconciler.go:455-456) now returns `EventDesiredRestarting` when the pod is gone AND `spec.desiredPhase == Restarting`, routing through the Restarting branch instead of Failed. Commit d8e767b (PR #17).
- [x] **Test: PodScheduled Starting vs Running race** — Condition waits for Starting OR Running.
- [x] **Prod-e2e README** — Comprehensive, 182 lines: env vars, run instructions, test-to-requirement mapping, gap analysis.
- [x] **PhaseLogStream** — Implemented in log_stream.go, verifies `follow=true` with live marker probe.
- [x] **PhaseWholeDiskPersistence** — Implemented in persistence.go, apt install + marker survival across restart.
- [x] **PhaseSendTelegramMessage** — Bot connectivity + webhook route check (phases.go:382-464), included in composite.

## Closed 2026-04-14

- [x] **#1: Agent deletion leaves secrets behind** — cascade-delete finalizer now removes `<name>-oauth`, `<name>-telegram`, and `<name>-anthropic` secrets when agent is deleted.
- [x] **#2: CD pipeline only deploys images, not Helm manifests** — superseded by ArgoCD separation (2026-04-16). CI now only builds/pushes images; ArgoCD + Image Updater handle full chart deployment via matty-v/kyber-deploy.
- [x] **#3: Machine reconciler 409 handling** — reconciler adopts existing RUNNING machine CRD on 409; deletes TERMINATED CRD and retries. Fixes preemption replacement race.
- [x] **#4: PWA capacity badge uses hardcoded constant** — capacity badge now uses live `Node.status.allocatable` via `MachineStatus.Allocatable` field plumbed from node-agent.
- [x] **#5: `/api/v1/agents/{name}/logs` always 404s** — logs endpoint now derives pod name from agent name (was reading unset `Status.PodName`).

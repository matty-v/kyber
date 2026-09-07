# Release documentation audit evidence

Baseline: `b8c47b9`, latest published Kyber release `v1.4.1`.
This record distinguishes verified current contracts from dated design history.
Dated design/spec/ADR documents remain historical evidence, not install guides.

## Coverage and findings

| Surface | Code/CI consulted | Outcome |
|---|---|---|
| Release preparation/publication | prepare-release, release, build-image, auto-publish-pwa-views workflows; release guards | Rewrite obsolete runbook; nine images; operator-controlled installation; explain test-tag skip behavior and publication recovery; fix Slack chart pin and previous-release tag selection |
| Product pages and publication | product manifest/test; AgentDetail/Settings navigation; API routes | Update Slack, avatars, local navigation, runtime evidence, durable tasks/A2A, recovery phases and storage caveats; all published pages accounted for |
| Channel setup | routes_agent_comms; Slack main/MCP; Slack pod/binding; Telegram/Discord convergence | Document Slack's text-only scope, two tokens, both allowlists, explicit restart requirement and GET revision limitation; preserve richer Telegram/Discord contracts |
| Profiles | routes_agents handleAgentAvatar; control-plane TaskObjectStore wiring | Document API-only private PNG/JPEG/WebP up to 1 MiB; no console upload control; uploads currently require durable tasks enabled plus object storage |
| Lifecycle/persistence | Agent phase constants; state machine; chart agent.security; rootfs/runtime integration | Correct 14 phases, DiskExhausted/BrokenRuntime, chroot/user-namespace defaults, node-local loss boundary; remove obsolete overlay warning from AGENTS |
| Harness/auth/catalogs | descriptor/auth contracts; runtime adapters; internal runtime-catalog handler; routes_agent_models; production poller construction | Document per-agent discovery and unknown Codex windows; remove frozen model list and global-poller setup instructions; preserve explicit conformance evidence limits |
| Tasks/A2A/authorization | durable-tasks architecture; taskstore migration/dispatch; internal reports; A2A conformance workflow and support inventory | Product coverage added; distinguish mandatory task ownership/scopes from optional lifecycle enforcement; no protocol conformance inferred from docs |
| Installation/upgrades | Helm defaults; release packaging; selfupgrade; source migration code | Add GKE/EKS navigation; correct nine image pins, retired promotion, current database scope, generated CRD ownership and rollback caveats |
| Contributor/PWA publishing | Makefile; test workflow; auto/tag publish workflows | Replace manual-tag default with actual automatic publication and recovery; no package source change/version bump needed |
| Metrics/logging/identity/jobs | architecture status/metrics/log docs; runtime catalog handler; scheduled-job capability guidance; identity/persistence docs | Correct authoritative-window explanation and advanced-job evidence boundary; retain pricing/archival mechanisms and identity ownership contracts |
| Links and historical material | tracked Markdown link scan; product manifest integrity | Nonhistorical file links resolve except explicit template placeholders; retain dated prototype/design evidence rather than mislabel it production dead code |
| Dead code | repository-wide Go identifier/reference scan; release checkout/script pairing | Remove unreferenced private countBroken helper; remove unreachable old-release script fallback from current release workflow. Preserve compatibility APIs and provider paths that still have callers |

## Validation

- Product documentation contract: 81 checks passed at the first checkpoint.
- Release guards: 15 checks passed, including a real git fixture with an
  intervening pwa-views tag exercising the workflow's previous-tag expression.
- Focused skills and chart tests passed, including a regression check comparing
  release-built images, chart image catalog, and packaged tag coverage.
- Full local Go build passed; full lint/test, integration and A2A passed in CI
  at 99ae991. Duplicate local vet/full-suite execution stopped after CI coverage.
- Helm lint/render and 16 PWA tag-decision tests passed.
- Cleanup merged as PR #232; Matt approved v1.5.0 and preparation run
  34074770319 opened PR #233. Publication is tracked by that workflow.
- No live deployment performed. Release dispatch followed the explicit approval.

## Scope boundaries

No dependency, CRD schema, RBAC, credential, or production deployment changes.
Historical prototypes remain as evidence. Slack's missing revision-based automatic
convergence is documented, not silently claimed to work. Test-release dependency
semantics are documented rather than redesigned in this cleanup.

A comprehensive documentation review cannot prove the absence of every dead path.
The removals above have concrete reference/checkout evidence; public compatibility
surfaces were not removed solely because current main does not call them.

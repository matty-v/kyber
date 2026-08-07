# Agent Identity Repos

An **identity repo** is a private GitHub repository that backs a Kyber agent's durable identity. It contains the agent's persona files (`CLAUDE.md`, `IDENTITY.md`, `SOUL.md`, `USER.md`), long-term memory (`memory/`), session state (`state/`, `.runtime/`), and optional skills and configuration. The repo persists across pod restarts, preemptions, and redeployments; the agent picks up right where it left off.

Contrast with the default model: without an identity repo the agent's `CLAUDE.md` is generated at pod start from `spec.identity.soulDescription` and never updated. Learnings, session summaries, and memory are lost when the pod is replaced.

---

## Creation modes

The Create Agent form offers three modes. Choose one at agent creation time — changing modes afterwards requires deleting and re-creating the agent.

### 1. Create new from template (default)

Kyber scaffolds a new private repo under the configured owner (`matty-v` by default, overridable via `KYBER_IDENTITY_REPO_OWNER` on the control plane) from the template `matty-v/kyber-agent-template`. The template is runtime-neutral: `AGENTS.md` is the canonical identity/startup contract read directly by Codex, `CLAUDE.md` is Claude Code's compatibility entrypoint to that same contract, and shared memory/state/skills/scripts work in either runtime. Claude-only project hooks live in `.claude/settings.json`; Codex safely ignores them.

Template substitutions applied at scaffold time:

| Placeholder | Replaced with |
|---|---|
| `{{ .AgentName }}` | The agent's name (e.g. `chewie`) |
| `{{ .Description }}` | The agent's soul description string |

The new repo is named `<agent-name>-agent` and is private by default.

**No GitHub setup needed.** Kyber mints an installation token via the platform GitHub App (see [GitHub App setup](#github-app-setup)) and calls `POST /repos/{template}/generate` on your behalf.

### 2. Link existing repo

Operator pre-creates a GitHub repo (e.g. by forking another agent's repo or cloning the template manually) and provides the slug on the Create Agent form. No content changes are made to the repo, and the controller scaffolds nothing (no template `generate` call). At runtime the pod clones and pushes the linked repo with the same App-minted, repo-scoped token as any identity repo (see [Git credential lifecycle](#git-credential-lifecycle)) — so the Kyber Platform GitHub App must have access to it.

Use this mode to:
- Migrate an existing agent that was set up with a manual PAT
- Copy one agent's identity to bootstrap a new agent
- Bring your own repo structure

### 3. None

No identity repo. The agent runs with `spec.identity.soulDescription` only, exactly as before identity repos were introduced. Memory and session state are not persisted to git.

---

## What gets mounted in the pod

When an identity repo is configured, the pod receives a single env var:

| Env var | Value |
|---|---|
| `KYBER_IDENTITY_REPO` | `owner/repo` slug |

> **Identity-repo git is App-managed, no PAT fallback (kyber#508 Stage 3/4).** Reads AND writes of the agent's OWN identity repo authenticate with a short-lived token minted by the install's **Kyber Platform GitHub App** (`contents:write`, ~1h, cached), fetched from the control plane's `GET /internal/agents/{name}/identity-repo-token`. There is **no PAT fallback**: if the App flow fails the git op fails loudly, so a broken credential path surfaces instead of being silently masked. The generic PAT (`$USER_GITHUB_TOKEN` / `$GH_TOKEN`) is used only for **other** repos (a maintainer agent's cross-repo work). This is uniform across every Kyber agent, including the Falcon team's. (Supersedes kyber#509's PAT-only cutover.)

The shared identity setup sourced by both runtime start scripts runs at pod boot:

1. Writes a git credential helper (`~/.local/bin/git-credential-kyber-github`) and enables `credential.useHttpPath` so git tells the helper *which* repo it is authenticating. For the identity repo the helper mints an App token via the control plane — no PAT fallback, fail-loud on error; for any other repo it emits the PAT (`${GH_TOKEN:-$USER_GITHUB_TOKEN}`). All values are read fresh on every git call, nothing baked into `~/.gitconfig`. Identity-repo setup is **not** gated on a PAT, so a PAT-less agent still clones via the App.
2. Clones `https://github.com/$KYBER_IDENTITY_REPO` into `~/dev/<repo-name>` using the App token. The clone is non-fatal (the agent still boots if the repo is briefly unreachable) but a failure is logged loudly — it is never silently PAT-backed.

3. Renders the **agent manual** — [`docs/agent-manual.md`](./agent-manual.md), baked into the runtime image at `/opt/kyber/KYBER.md` — into `<repo>/.runtime/KYBER.md`. It is the platform explaining itself to the agent: durability tiers, lifecycle phases, the credential model above, how work arrives, and where the agent's authority ends. Rendered at boot rather than scaffolded into the repo so every agent reads the manual for the platform version it is *actually* running, not a copy frozen on the day its repo was created. Platform-owned and overwritten every boot — the same contract as `session-recall.md`.

Both runtimes launch with the identity repo as their working directory, so their native project instruction discovery loads the template entrypoint (`AGENTS.md` for Codex, `CLAUDE.md` for Claude Code). The shared syncer links every `skills/<name>/SKILL.md` package into both `~/.claude/skills/` and `~/.codex/skills/`. The template's `.claude/settings.json` is a normal Claude Code project setting; Kyber does not copy or render it, and Codex ignores it.

---

## Git credential lifecycle

As of kyber#508 Stage 3/4, the agent's identity repo is managed **exclusively by the Kyber Platform GitHub App**:

- **Reads and writes of the identity repo** use a short-lived token minted on demand by the App, scoped to just that repo (`contents:write`). The credential helper calls `GET /internal/agents/{name}/identity-repo-token`, authenticating with the pod-token (`/var/run/secrets/kyber/pod-token`, bind-mounted into the agent's chroot); the #566 internal auth enforces **act-on-self-only**, so an agent can only ever mint its own repo's token (cross-agent → 403). The token is cached (mode 600) until shortly before expiry and never written to `~/.gitconfig`.
- **No PAT fallback for the identity repo.** If the App flow fails — App unconfigured/broken, pod-token unreadable, endpoint down, empty response — the helper emits nothing and git **fails loudly**. A broken identity-repo credential path must be visible, not masked by the broad PAT.
- **The generic PAT** (`$USER_GITHUB_TOKEN` / `$GH_TOKEN`, from Kyber's per-agent kv-secrets) is used only for **other** repos the agent touches (e.g. a maintainer agent's cross-repo work).

**Install requirement:** the Kyber Platform GitHub App is a configured per-install plugin — nothing is hardcoded. To enable identity-repo management an install provides (1) a `kyber-github-app` Secret holding an App with `Administration` + `Contents` (write) on the identity-repo account, and (2) `identityRepo.defaultOwner` set to that account (the chart default is empty). If either is absent the feature disables cleanly — agents run without an identity repo; it is never backfilled with a PAT.

For a configured identity repo, the controller's `reconcileIdentityRepo` records `status.identityRepo.phase=Ready` (visible in the PWA and `kubectl describe agent <name>`). The `.status.identityRepo.tokenExpiresAt` / `.lastMinted` fields remain unpopulated — they reflected the removed pre-#509 in-platform mint loop, and the Stage 3/4 on-demand mint is stateless — and are slated for removal with the rest of the App-backed status surface later in [#508](https://github.com/matty-v/kyber/issues/508).

---

## Memory-backup pattern

The standard agent setup keeps `memory/` and `state/` durable through `scripts/save-state.sh`. Claude Code's project-local `.claude/settings.json` invokes it as a `PostToolUse(Write|Edit)` hook; the runtime-neutral `AGENTS.md` and memory skills also require an explicit invocation, which covers Codex and safely no-ops when Claude's hook already committed the write.

The hook is consumer-side (lives in the identity repo, not in the platform image). Agents that want to customize Claude Code hook behavior edit `.claude/settings.json`; cross-runtime behavior belongs in `AGENTS.md`, scripts, or skills.

Claude Code's Stop hook writes `.runtime/last-session-tail.md` at the end of every turn. Independently, Kyber's session-saver sidecar writes the harness-neutral `.runtime/session-recall.md` used by both Claude Code and Codex after a restart.

---

## Agent deletion

When an agent is deleted:

- Any **legacy** `<agent-name>-github` Secret (from before kyber#509 removed the git-token delivery loop) is garbage-collected via the owner-ref on the Agent CR and the finalizer's label-scoped sweep. Kyber no longer creates these Secrets; orphans simply age out on agent deletion (a one-time `kubectl delete secret -l kyber.io/purpose=github-identity` is a harmless optional cleanup, not required for correctness).
- The GitHub identity repo is **preserved**. Kyber never deletes it. Rationale: identity data — memory, session summaries, skills — is valuable post-mortem and the operator should decide when (and whether) to delete it.

To delete the repo: use the GitHub web UI or `gh repo delete owner/repo`.

---

## Graceful degradation

| Failure | Observed behavior |
|---|---|
| App token mint fails (App unconfigured/broken, pod-token unreadable, endpoint down) | The identity-repo git op fails **loudly** — there is no PAT fallback — so `start-claude.sh` logs the failure and continues without the repo; the pod still starts. Fix the App path (check the `kyber-github-app` Secret + control plane). |
| Clone fails at pod start (network error, repo deleted, bad credential) | `start-claude.sh` logs the error and skips clone. Agent starts with no identity files. |
| Scaffold (create-new mode): GitHub App not installed / mint 5xx / template missing | Controller marks `status.identityRepo.phase=Failed` (or retries). Only affects **create-new-from-template** scaffolding — linking an existing repo does not touch the App. |
| `kyber-github-app` Secret missing at control-plane start | Control plane logs a warning and continues. A linked repo's runtime git (clone/sync) then fails **loudly** — there is no PAT fallback — and **scaffolding** a new repo from a template fails with `status.identityRepo.phase=Failed`. |

---

## GitHub App setup

Identity repos require a one-time GitHub App registration. See
[installation.md § 5b](./installation.md#5b-register-the-kyber-github-app) for the full step-by-step (create App → generate key → install → apply `kyber-github-app` Secret).

If the `kyber-github-app` Secret is absent, identity-repo management is disabled and the two creation modes differ: an agent requesting a **new** repo from a template shows `phase=Failed` in `status.identityRepo` (scaffolding needs the App), while an agent **linked** to an existing repo reconciles to `Ready` but its identity-repo git fails loudly at runtime (no PAT fallback). Agents without identity repos are unaffected.

---

## Reference

- `pkg/githubapp/` — GitHub App client: JWT minting, installation token fetch, template scaffolder (used for scaffolding, the GitHub API routes, and minting the per-agent identity-repo git token)
- `pkg/controllers/agent/identity_repo.go` — reconcile loop: scaffolding dispatch + status. (Runtime identity-repo git auth is the App-minted token from the control plane's `/internal/agents/{name}/identity-repo-token` endpoint, not this controller.)
- `images/claude-code/start-claude.sh` — installs the git credential helper (App-minted token for the identity repo, generic PAT for other repos) and clones/syncs the repo at pod boot
- `pkg/api/routes_agents.go` — `agentIdentityRepoRequest` / `agentIdentityRepoResponse` shapes
- Template repo: `matty-v/kyber-agent-template` (marked `is_template: true` on GitHub)

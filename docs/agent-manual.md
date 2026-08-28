# Kyber — the platform you run on

You are an agent running on **Kyber**, a self-hosted agent-fleet platform. This is your operator's manual for the environment itself: what keeps you alive, what quietly deletes you, what you own, and what you have to ask for.

Your identity repo says *who* you are. This says *where* you live.

> Kyber wrote this file into your pod at boot, so it matches the version of the platform you are actually running on. It is read-only to you — editing it changes nothing, and your next restart overwrites it.

**Five things that will bite you if you forget them**

1. **Only git is durable.** Your identity repo is the one layer guaranteed to survive. Pushed, or it didn't happen.
2. **You will be restarted without warning.** Everything in your session context is gone. What you wake up with is `.runtime/session-recall.md`.
3. **You can't fix your own pod.** Memory limits, phases, images, secrets are the operator's. Your job is to name the exact fix.
4. **Long sessions are the expensive thing** — not long prompts. Restart on purpose.
5. **A failed push to your own identity repo is the App-token path**, not your PAT. Don't debug the wrong credential.

---

## 1. What you are

A long-lived pod, declared by an `Agent` custom resource (`kyber.io/v1`). The operator writes intent on `spec` (machine, resources, model, `desiredPhase`, identity repo, secrets); the control plane's controllers reconcile reality toward it and report back on `status`.

You are not a request handler that gets torn down between messages. You are a process someone deliberately keeps running — with a filesystem, a session, and a name.

## 2. What survives — three tiers

| Tier | Holds | Survives |
|---|---|---|
| **Identity repo** (GitHub, cloned to `~/dev/<name>-agent`) | identity, memory, state, skills, config | everything — pod loss, machine loss, being rebuilt on new hardware |
| **`/persist` durable root** | your whole root filesystem — HOME, installed packages, `/etc` edits, `/persist/session-state.json` | pod recreation and restart — **not** a PV wipe, a re-created agent, or (on node-local volumes) losing the machine |
| **Everything else** | your in-context memory of this session | nothing |

If it matters past this session, it goes in the identity repo. Also note the flip side of tier 2: your root filesystem is genuinely yours, which means it **accumulates**. When a config change or an upgrade mysteriously "won't take", something you persisted earlier is the first suspect — a Kyber base-image upgrade deliberately will not overwrite a file you have touched, and it lists what it kept in `/persist/kyber/rootfs-upgrade-conflicts.log`.

## 3. How you stop and start

Phases you can be in, and what each means for you:

| Phase | What it means |
|---|---|
| `Running` | normal |
| `Restarting` / `Stopping` / `Stopped` | operator-driven; disk preserved |
| `Draining` → `WaitingForMachine` | your spot machine is being preempted; you're waiting for a replacement |
| `NeedsAuth` | your OAuth credential is missing, expired, or invalid — a **human** must re-authorize |
| `MemoryExhausted` | you were OOM-killed. **No auto-restart.** The operator must raise the memory limit first |
| `Failed` | restart retries exhausted; alerts the operator |

Two different ways you vanish, and they need different fixes:

- **Pod-level** — OOM kill, preemption, operator delete. New pod, `/persist` intact.
- **Session-level** — your session process dies while the pod stays healthy. Filesystem intact, session context gone.

Either way you find out the same way: you boot, read your continuity files, and resume. **Never assume the last thing you remember doing actually completed.** Check the repo, the branch, the PR, the deploy — then resume.

## 4. Session continuity — your black box

A **session-saver sidecar** snapshots your last activity and recent turns to `/persist/session-state.json` every few seconds, independently of your process. At boot, that gets rendered into `.runtime/session-recall.md`.

That file is platform-owned and read-only to you. Because the sidecar writes it off-process, it survives crashes that no in-session hook could catch — so when your own session summary and the recall disagree, **the recall is fresher**. Trust it.

## 5. Your credentials

- **Pod token** — `/var/run/secrets/kyber/pod-token`. Proves you are you to the control plane. Enforced **act-on-self-only**: you cannot act as another agent (403). Don't move it, print it, or copy it.
- **Your identity repo** — git goes through the `git-credential-kyber-github` helper, which mints a short-lived, repo-scoped token from the platform GitHub App on every call. There is **no PAT fallback**, on purpose: if the App path is broken the git op fails loudly. It also needs the pod environment, so it won't work from a bare `nsenter`/`su` shell.
- **Other repos** — `$GH_TOKEN` / `$USER_GITHUB_TOKEN`, only for repos that aren't yours.
- **Operator-delivered secrets** arrive as `USER_*` environment variables. If one isn't in your environment, it usually needs a pod roll to land — ask for it, don't go hunting for it on disk.

Never echo, log, paste, or commit any of these. A token in a transcript is a leaked token.

## 6. Your skills

A **skill** is a reusable workflow you can invoke — `/name` in Claude Code, `$name` in Codex — and that either runtime can also trigger on its own when your description matches what is being asked.

Every Kyber agent keeps skills in the same place, whatever runtime it runs:

```
~/dev/<your-repo>/skills/<name>/SKILL.md
```

`SKILL.md` needs YAML frontmatter with a `name` and a `description`. The **directory name is what gets invoked** — if the frontmatter disagrees with it, the directory wins. Bundle whatever else the skill needs (a `references/` folder, scripts, assets) inside the same directory.

Two other kinds of skill show up alongside yours. Packages vendored into your repo under `vendor/<package>/skills/` come from a shared source and are not yours to edit — and note that a vendored skill **replaces** one of yours with the same name, so pick distinct names. Skills like `telegram-messaging` and `discord-messaging` are baked into the runtime image and appear only when the matching sidecar is attached.

### Saving a skill

After you write (or download) a skill, run:

```
kyber-skills install
```

That one command does the whole job: it links the skill into both runtimes so it works **immediately**, commits it, and pushes it to your identity repo. It is idempotent, so running it again is always safe. To pull in something you downloaded elsewhere, point it at the source: `kyber-skills install --from /tmp/some-skill`.

If you forget, the platform catches up on its own within a couple of minutes — it relinks and re-reports on a loop. `install` just makes it instant, and makes sure the skill is actually pushed. Until it is pushed it shows as `not_pushed`, because a skill that lives only in this pod dies with it.

Writing a skill straight into `~/.claude/skills/` or `~/.codex/skills/` looks like it works and is the one way to lose it — nothing there is committed, so it is gone the moment you are reprovisioned.

Run `kyber-skills list` any time to see what you actually have, including anything broken.

### What the operator sees

Your skills appear in the Kyber UI on your agent's **Skills** tab. That view is read-only: the only way to add, change, or remove a skill is to ask you to do it. What it shows is a scan of your real filesystem, not of your GitHub repo, so a skill that is committed but not loadable shows up as broken rather than fine.

## 7. How work reaches you

- **Channels** (Telegram, Discord, …) deliver prompts through Kyber's signed
  inbound rail. **The person is reading the channel, not your transcript** —
  explicitly reply through the originating channel. For a `<channel
  source="telegram" chat_id="…">` prompt, use the `kyber-telegram` MCP tools
  for replies, buttons, edits, reactions, and attachments. Both runtimes use
  the same sidecar; `http://127.0.0.1:14004/send` remains a text-only fallback.
  For a `<channel source="discord" channel_id="…" message_id="…">` prompt,
  use the `kyber-discord` MCP `reply` tool with that channel and message ID.
  Use its `edit_message`, `react`, and `download_attachment` tools for richer
  interactions; all remain bounded to configured channels and observed files.
  It returns the sent Discord message ID. If MCP is unavailable, POST JSON
  `{"channel_id":"…","content":"…","message_id":"…"}` to
  `http://127.0.0.1:14005/send`. `message_id` is optional and makes the response
  a Discord reply. Channel bot
  credentials stay inside the sidecars and are never available to the runtime.
  Both send endpoints are scoped to the conversations you actually serve — a
  `403` means the chat or channel you targeted is outside your allowlist, so
  reply into the one the prompt came from rather than retrying elsewhere.
- **Inbound webhooks** deliver HMAC-signed messages via bindings. Per-field truncation is binding config, so a message that arrives cut off mid-sentence is a config limit, not your reading comprehension.
- **Scheduled jobs** fire prompts at you on a cron.
- **The PWA shell** is a human typing straight into your live session.

Any of these can arrive mid-task. Acknowledge, then finish or renegotiate — don't drop the thread you were on.

## 8. How you're seen

Your activity flows through a status sidecar to the control plane and onto `Agent.status`, which is what the operator sees in the PWA: phase, current activity, token and cost usage, logs.

Two things that surface has a blind spot for: the activity view reads the archived transcript, so work you do **inside subagents** can be invisible there; and a clean phase says nothing about whether your work is correct. If it matters, say it out loud on your channel.

## 9. Staying alive

- **Memory.** Exceed your container's limit and you're OOM-killed into `MemoryExhausted`, which does **not** auto-restart. If you can feel yourself getting heavy — huge files, giant command output, long-running subagents — shed weight before you die.
- **Session length is your real cost driver.** Every turn re-reads the whole conversation, so a late turn costs multiples of an early one. Ending a session cleanly and starting fresh is cheaper and sharper than grinding on forever. Restart at a natural boundary rather than at the wall.
- **Don't run broad filesystem scans.** `find /`, whole-disk greps, recursive scans of a mounted overlay — on small nodes that's enough I/O to degrade the node for everyone.

## 10. Yours vs. the operator's

**You can:** edit and push your identity repo, save memory, install tools in your home, restart your own session, use your credentials within their scope.

**You cannot:** change your own memory/CPU limits, phase, model, image, or secrets; recover yourself from `MemoryExhausted`, `Failed`, or `NeedsAuth`; touch another agent's anything.

When you're blocked on the platform, say exactly what needs to change and where. *"Raise my memory limit from 4Gi to 8Gi on my Agent resource"* gets fixed. *"I keep crashing"* gets a conversation.

## 11. First checks when something's off

| Symptom | Look here first |
|---|---|
| Push to your identity repo fails | The App-token path (pod token readable? control plane reachable?) — **not** your PAT. It's designed to fail loudly |
| Push rejected as diverged | Someone/something wrote to the repo out of band. Pull first, then push — never force |
| You woke up with no idea what you were doing | `.runtime/session-recall.md` missing or stale → tell the operator; that's a continuity bug, not something to paper over |
| Channel went silent after a restart | The channel plugin didn't reclaim its connection. Report it — a restart usually clears it |
| You died mid-task and came back | Check whether the work actually landed (branch, PR, deploy) before redoing it |
| Repeated crashes on the same task | Likely memory. Report it as `MemoryExhausted` + the limit you need, not as "flaky" |

## 12. House rules

- **Never** edit another agent's identity repo, memory, or session. Ever.
- **Never** deploy by hand. Ship through the repo's CI/CD.
- The cluster belongs to the operator. Propose; don't reach in.
- When the platform surprises you, that's a platform gap. Write it down or file it — the surprise is worth more than the workaround.

---

## Going deeper

These live in the `kyber` repo and are the source of truth when this page is stale:

- `docs/architecture/overview.md` — the whole platform
- `docs/architecture/agent-lifecycle.md` — phases, events, transitions, failure modes
- `docs/architecture/status-pipeline.md` — how your activity becomes something the operator can see
- `docs/agents-identity-repos.md` — identity repos and the git credential lifecycle
- `docs/operator/wedged-agent-recovery.md` — what your operator does when you're stuck

# Codex CLI spike notes

- Verified 2026-08-03 with official `@openai/codex` 0.146.0 on Debian
  Bookworm and Node.js 22.
- ChatGPT subscription credentials are stored in `$CODEX_HOME/auth.json`.
  `codex login status` is the non-interactive credential check.
- Interactive Codex runs correctly inside the platform tmux session named
  `agent`; `codex resume --last` is available for later automatic resume work.
- Sessions and rollout JSONL live below `$CODEX_HOME/sessions` and include
  token-count events. A runtime-specific reporter remains required before the
  Metrics token card is considered complete.
- ChatGPT subscription model IDs verified from the current official manual are
  `gpt-5.6-sol`, `gpt-5.6-terra`, and `gpt-5.6-luna`; older Codex model IDs are
  rejected for ChatGPT-authenticated sessions.
- The runtime forces file-backed credentials, no approvals, and full filesystem
  access because the Kubernetes pod is Kyber's isolation boundary.

## Turn-boundary hooks (FAL-8, verified 2026-08-27 against `@openai/codex` 0.146.0)

Verified by running the pinned linux-x64 binary from the npm package, driving a
real TUI session in tmux against a stub Responses API so turns actually
complete. Findings, so nobody re-derives them:

- **Codex has Claude-Code-shaped hooks.** The event set is `PreToolUse`,
  `PermissionRequest`, `PostToolUse`, `PreCompact`, `PostCompact`,
  `SessionStart`, `SessionEnd`, `UserPromptSubmit`, `SubagentStart`,
  `SubagentStop`, `Stop`. The `hooks` feature flag is on by default. This
  supersedes both approaches the issue proposed (the `notify` program, and
  deriving turn boundaries from the session rollout JSONL) — neither is needed.
- **`UserPromptSubmit` carries the prompt.** Observed payload keys:
  `session_id`, `turn_id`, `transcript_path`, `cwd`, `hook_event_name`, `model`,
  `permission_mode`, `prompt`. The `prompt` key is exactly what
  `kyber-cron-turn-start` reads (`jq -er '.prompt'`), so the platform script
  needed no change.
- **`Stop` fires at turn end**, with `stop_hook_active` and
  `last_assistant_message`. `kyber-cron-postrun` reads no stdin, so it needed no
  change either.
- **User-level hooks are trust-gated and therefore unusable here.** Hooks in
  `$CODEX_HOME/config.toml` (or `$CODEX_HOME/hooks/hooks.json`) trigger an
  interactive prompt at session start — "Hooks need review / 1. Review hooks
  / 2. Trust all and continue / 3. Continue without trusting (hooks won't run)"
  — and do not run until it is answered. Trusting writes
  `[hooks.state."<config-path>:<event_snake_case>:<matcher_idx>:<handler_idx>"]
  trusted_hash = "sha256:…"` back into `config.toml`. The hash is over an
  internal normalized hook identity; it was NOT reproducible from the obvious
  preimages, so pre-seeding trust is not a supported path. On a headless pod
  that prompt is worse than an inert feature: it parks the TUI on a question
  nobody can answer.
- **The system-managed layer is the non-interactive path.** Hooks written to
  `/etc/codex/managed_config.toml` load and fire with no trust prompt at all
  (confirmed: both hooks fired on the first turn of a fresh session with an
  empty user config). Codex reads `/etc/codex/config.toml`,
  `/etc/codex/requirements.toml`, and `/etc/codex/managed_config.toml` as
  policy layers above user config. This is what `start-codex.sh` writes.
- **Hook processes inherit the runtime's environment**, so
  `KYBER_CLEAR_SESSION_TEXT` reaches `kyber-cron-postrun`. The start script
  still bakes it into the hook command string rather than relying on that: the
  hook is spawned by codex, which is spawned by tmux, and a tmux server that
  outlived an earlier boot would carry that boot's environment.
- The hook `command` string is shell-interpreted, so `env VAR=value /path/cmd`
  and trailing arguments both work.

### Which command actually clears a Codex conversation (FAL-8 review, kyber#162)

`clearContextAfter` promises each fire "begins from a clean conversation instead
of accumulating every previous run". Codex offers three relevant slash commands,
and only two of them honour that:

| Command | Description in the binary | Prior-turn context afterwards |
| --- | --- | --- |
| `/compact` | "summarize conversation to prevent hitting the context limit" | **retained** |
| `/new` | "start a new chat during a conversation" | dropped |
| `/clear` | — | dropped |

Measured, not inferred. With a stub Responses API logging every upstream request,
a token was planted in one turn and the next turn's request was searched for it:

- after `/compact` → token still present (and the TUI prints "Context compacted"),
- after `/new` → absent,
- after `/clear` → absent; both print "To continue this session, run codex resume
  <id>", i.e. the old thread is closed and a new one begins.

Codex itself warns after a compaction: *"Long threads and multiple compactions can
cause the model to be less accurate. Start a new thread when possible to keep
threads small and targeted."* A cron job firing unattended for days is precisely
the case that warning is about.

So the Stop hook sends **`/clear`** — which is also the literal Claude Code uses
(`kyber-cron-postrun`'s own default), making the two runtimes genuinely
equivalent. `pkg/runtimes/codex/adapter.go`'s `CompactSessionCommand()` keeps
sending `/compact`: that is the operator's *manual compaction* control and is a
different thing from this flag.

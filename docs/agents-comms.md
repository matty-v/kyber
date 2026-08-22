# Configuring an agent's comms channels

> kyber#664: per-agent comms configuration as an API, and the Comms tab that
> drives it. Before this, Telegram could only be set up when the agent was
> created and Discord could only be set up with `kubectl`.

An agent's *comms channels* are the ways a human reaches it and it answers
back. Kyber supports two:

| Channel | Direction | What it needs |
| --- | --- | --- |
| **Telegram** | two-way | a bot token and numeric user allowlist |
| **Discord** | two-way | a bot, an allowlist, and a running pod — see [agents-discord-two-way.md](agents-discord-two-way.md) |

The older **outbound-only Discord webhook** (`spec.secrets.discordEnabled`) is
a separate, one-way thing and is deliberately *not* part of this surface — it
keeps working exactly as it did. See
[agents-discord-webhooks.md](agents-discord-webhooks.md).

## The API

```
GET    /api/v1/agents/{name}/comms              every channel's config
GET    /api/v1/agents/{name}/comms/{channel}    one channel
PUT    /api/v1/agents/{name}/comms/{channel}    configure (idempotent)
DELETE /api/v1/agents/{name}/comms/{channel}    disable and clean up
```

`{channel}` is `telegram` or `discord`. `PUT` is idempotent because a channel
is a singleton per agent — configure and update are the same call, so a
retried request is harmless.

**Credentials are write-only.** Tokens go in through `PUT` and are never
returned by any endpoint. `GET` reports `botTokenSet` — presence, not value.

### Telegram

```bash
curl -sS -X PUT "$KYBER/api/v1/agents/dave/comms/telegram" \
  -H "Authorization: Bearer $KYBER_API_KEY" -H 'Content-Type: application/json' \
  -d '{"botToken": "123456:ABC-DEF...", "allowedUserIds": ["1000000001"]}'
```

Stores the token in the `<agent>-telegram` Secret and sets
`spec.secrets.telegramEnabled`. A second `PUT` with a different token rotates
it — the path to take when a token leaks. `DELETE` turns the channel off and
deletes the stored token.

`botToken` may be omitted when a token is already stored; that re-enables a
channel that was turned off without making you find the token again.

**Both runtimes use the same bridge.** Telegram is served by the runtime-neutral
`kyber-mcp-telegram` polling sidecar, injected into every pod with
`spec.secrets.telegramEnabled` regardless of runtime (kyber#684). Claude Code's
native in-process plugin is **retired** — the runtime container no longer receives
a bot token.

This means the install must pin `image.telegramSidecar`. If it is empty, agents
get no Telegram at all and the control plane raises `TelegramUnavailable` with
reason `NoTelegramSidecarImage`.

At least one numeric user ID is required; every other sender is ignored. An agent
with no allowlist raises `TelegramUnavailable` / `NoTelegramAllowlist` and the
sidecar refuses to start. Ask `@userinfobot` for your ID.

#### Telegram behavior and agent tools

Accepted Telegram updates enter the same signed, rate-limited, bounded inbound
dispatcher as other bindings. The sidecar supports text and edited messages,
single attachments, media albums, inline-button callbacks, and message reaction
changes. Album items are held for 600 ms and delivered as one event with an
`attachments` JSON array. The sidecar sends Telegram's `typing` action while the
agent is working and stops it when the agent replies or delivery fails.

Every enabled runtime registers the `kyber-telegram` MCP server on loopback. Its
tools are:

| Tool | Behavior |
| --- | --- |
| `reply` | sends text, optional files, reply threading, and inline buttons |
| `edit_message` | edits text; omitted `buttons` preserves the keyboard, a non-empty array replaces it, and `buttons: []` removes it |
| `react` | sets or clears the bot's emoji reaction on a message |
| `download_attachment` | downloads an allowlisted inbound file into `/persist/telegram-attachments` |

Button values are never exposed to Telegram. The sidecar substitutes opaque,
one-shot callback tokens and maps a valid press back to `callback_label` and
`callback_value`. Tokens are chat-scoped, bounded to the newest 256, and are
invalidated when their keyboard is cleared or replaced. A keyboard supports at
most 100 buttons; each label is limited to 64 characters and each internal
value to 1,024 bytes.

Outbound files use Telegram-native methods by extension: JPEG/PNG/WebP photos,
GIF animations, MP4/MOV/WebM videos, MP3/M4A/FLAC/WAV audio, OGG/OGA/Opus voice,
and documents for everything else. Two to ten compatible photos/videos are sent
as one native media group.

The capability boundary is intentionally narrow: outbound chats must already be
in scope, downloads accept only the newest 256 file IDs observed from allowlisted
senders, uploads must resolve under `/persist` without symlink escape, and a file
or aggregate media group above 20 MiB is rejected before upload. This state is
in-memory and resets when the sidecar restarts.

Agents with Telegram enabled receive a bundled `telegram-messaging` cookbook
skill in both supported runtimes. An identity repo may provide a skill with the
same name to override it. The inbound binding also carries a short always-on
instruction, so basic replies do not depend on skill discovery.

### Migrating an agent off the retired Claude Code plugin

Agents created before kyber#684 kept their Telegram allowlist in `access.json` on
their own PVC. The control plane cannot read that file, so **there is no per-agent
value to carry forward** — the allowlist has to be re-supplied.

Two ways to do it:

1. **Per agent (preferred, explicit).** `PUT .../comms/telegram` with
   `allowedUserIds`. The stored token is reused, so you don't need to find it again.
2. **Install-wide seed, for a fleet migration.** Set `telegram.defaultAllowedUserIds`
   in the chart values (env `KYBER_TELEGRAM_DEFAULT_ALLOWED_USER_IDS`):

   ```yaml
   telegram:
     defaultAllowedUserIds: "123456789,987654321"
   ```

   Whoever runs the install knows who owns these agents, which is the information
   the migration is missing. Scope, precisely:

   - It only ever fills an allowlist that is **absent**. It never overrides one set
     through `/comms`.
   - It does **not** apply to newly created agents — those supply their own.
   - Leave it empty and a migrated agent accepts **nobody** until an operator sets
     its allowlist explicitly.

Once the allowlist exists, the pod converges on its own — see below.

### Discord

See [agents-discord-two-way.md](agents-discord-two-way.md) for the full setup,
including creating the bot and finding the IDs. The short version:

```bash
curl -sS -X PUT "$KYBER/api/v1/agents/barf/comms/discord" \
  -H "Authorization: Bearer $KYBER_API_KEY" -H 'Content-Type: application/json' \
  -d '{
    "botToken": "<discord bot token>",
    "guildIds": ["234567890123456789"],
    "channelIds": ["345678901234567890"],
    "allowedUserIds": ["123456789012345678"],
    "mentionOnly": true
  }'
```

One call creates the Secret, creates the `discord` inbound binding, and sets
`spec.channels.discord`. It generates the shared HMAC secret itself, which is
what makes this safe to do from a UI: the secret is never shown to anyone, so
it cannot be mistyped into one of the two places that must agree.

Empty `guildIds` / `channelIds` mean "any". An empty `allowedUserIds` is
**fail-closed** — nobody could reach the agent — so the API rejects it instead
of silently creating a deaf agent. IDs are validated as Discord snowflakes,
which catches the usual paste error of a channel *name* or a URL.

`action` optionally overrides the inbound binding's instruction text — the
prose the agent sees on every Discord message, and where an agent's Discord
persona lives. Omit it and Kyber writes a working default; omit it on a later
update and your customized text is preserved rather than reset.

Both supported runtimes register the Discord sidecar as the loopback
`kyber-discord` MCP server. Its `reply` tool accepts `channel_id`, `text`, and
an optional inbound `message_id`, enforces configured channel scope, and
returns the sent Discord message ID. `/send` remains a text-only compatibility
fallback for older action text.

Accepted Discord turns show 👀 plus a refreshed typing indicator while the
agent works. A successful reply changes that marker to ✅; an inbound dispatch
failure is marked ❌. Indicator failures never block message delivery.
Long replies are split safely at Discord's message limit, with fenced code
blocks preserved and all created message IDs returned by MCP.
The `reply` tool also accepts files under `/persist`, and
`download_attachment` fetches allowlisted inbound Discord files into
`/persist/discord-attachments`; both directions are capped at 10 MiB per file.
`edit_message` updates bot-authored text and `react` adds or removes the bot's
emoji reaction; both enforce the configured channel scope.

## Restarting the pod

Every response carries `podRestartRequired`. Channel sidecars are injected when a
pod is **built**, so a config change does not reach a running pod by itself. What
happens next differs by channel:

### Telegram — converges on its own (kyber#688)

The controller walks a running pod onto the Telegram bridge it currently wants,
whether the pod has the wrong sidecar image or no bridge container at all. You do
not have to roll it by hand. Before #688 nothing reconciled the two, and a migrated
agent could sit on the retired plugin indefinitely while its CR claimed otherwise.

The roll is gated — it holds, rather than rolling, when any of these is true:

| Gate | Behavior when it blocks |
| --- | --- |
| Agent never enabled Telegram | never rolled — no bridge is the correct state |
| `image.telegramSidecar` unpinned | never rolled (would rebuild an identical bridge-less pod forever); surfaced as `TelegramUnavailable`/`NoTelegramSidecarImage` |
| No allowlist | holds — rolling would trade a working agent for a crash-looping sidecar; surfaced as `TelegramUnavailable`/`NoTelegramAllowlist` |
| Agent is working (or state unknown) | holds until idle — a live session is never interrupted |
| Another agent pod is already being deleted | holds; one agent-pod delete in flight cluster-wide |
| Image not yet proven pullable | first eligible pod is the canary; the rest hold until it is seen Ready, or the window elapses and the roll is held behind a `TelegramSidecarRollHeld` event |

So the practical answer to "why hasn't my agent picked up Telegram yet" is usually
**it's busy, it has no allowlist, or the image isn't pinned** — check the Agent's
conditions and events before reaching for `kubectl delete pod`.

### Discord convergence

Discord saves and disables stamp a configuration revision. The controller
compares that revision and sidecar presence against the running pod and
rolls a stale pod once the runtime reports Idle. Working and unknown agents are
held so an active session is not interrupted; all automatic rolls share the
one-pod deletion budget. To apply immediately instead of waiting:

```bash
kubectl -n kyber-system delete pod agent-<name>
```

or use **Restart pod** in the UI.

On a `GET`, `podRestartRequired` is answered from the pod itself — sidecar
presence plus the applied Discord revision — so it stays true until the change
is really live, not just until the spec was written.

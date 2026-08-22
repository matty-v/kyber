# Two-way Discord for agents

> kyber#646: an agent both **receives** messages from a Discord channel and
> **replies** into it. Supersedes the outbound-only webhook path in
> [agents-discord-webhooks.md](agents-discord-webhooks.md), which is still the
> right tool for fire-and-forget notifications (no bot, no Gateway).

## How it works

Discord delivers normal channel messages **only** over a persistent Gateway
WebSocket — there is no message webhook Kyber could hold on the agent's behalf.
So each Discord-enabled agent pod gets a `kyber-mcp-discord` sidecar that keeps
that socket open for the life of the pod:

```
Discord Gateway ──▶ kyber-mcp-discord (sidecar)
                       │  allowlist: guild / channel / user
                       │  HMAC-signs the envelope
                       ▼
                    POST /webhooks/inbound/<agent>/discord   (the kyber#208 rail)
                       │
                       ▼
                    control plane ──▶ agent tmux session
                       │
   agent replies ──▶ kyber-discord MCP http://127.0.0.1:14007/mcp
                    (or POST :14005/send as a compatibility fallback)
                       │
                    kyber-mcp-discord ──▶ Discord REST API (bot token)
```

Inbound reuses the same inbound-binding machinery as every other webhook
source — verification, buffering, dedup, wake. Outbound goes back out through
the same sidecar. Both runtimes register its loopback `kyber-discord` MCP
server and use the structured `reply` tool; `/send` remains as a compatibility
fallback for older instructions. The sidecar calls Discord's REST API. The bot
token stays in the sidecar and is never injected into the runtime container.

**The pod must be warm.** This holds for Telegram too: both channels deliver
through in-pod sidecars, so a suspended agent has nobody holding the
socket/poll loop, and messages sent while it sleeps are lost. (A server-side
Telegram wake leg exists in code but is not wired — nothing registers the
webhook.) Run chat-channel agents warm.

Durable Discord delivery is deliberately outside Kyber's platform contract.
Kyber does not run a pod-independent Gateway or retain an unbounded Discord
queue. Operators who need messages accepted while an agent pod is absent must
place their own durable relay or queue in front of Kyber's signed inbound
webhook. The built-in sidecar is the live, low-latency transport for a running
agent pod.

## What you need before you start

1. **A Discord bot** (Developer Portal → New Application → Bot). Copy the bot
   token — it is shown once.
2. **Message Content Intent** enabled on that bot (Bot → Privileged Gateway
   Intents). Without it every message arrives with empty `content` and the
   agent sees blank prompts.
3. **The bot invited to the server**, by someone with Manage Server there:

   ```
   https://discord.com/oauth2/authorize?client_id=<APP_ID>&scope=bot&permissions=68672
   ```

   `68672` = view channel + send messages + read message history + add
   reactions. Nothing more; the sidecar needs no moderation permissions.
4. **The IDs** you'll allowlist — in Discord, enable Developer Mode
   (Settings → Advanced), then right-click → Copy ID on the server, the
   channel, and each person allowed to drive the agent.

## Wire it onto an agent

Use the **Comms tab** on the agent's page in the PWA, or the API it calls:

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

That one call does everything the manual steps below do — creates the Secret,
creates the matching `discord` inbound binding, and sets `spec.channels.discord`
— generating the shared HMAC secret itself so there is nothing to copy by hand.
`DELETE` on the same path unwires it. `GET` reports the current config without
ever returning credentials.

It responds `"podRestartRequired": true`: the sidecar is injected at pod-build
time, so the change only takes effect on a new pod. Kyber does **not** roll the
pod for you — that would destroy the agent's live session. Restart it when you
are ready (`kubectl -n kyber-system delete pod agent-barf`, or Restart pod in
the UI).

`allowedUserIds` must list at least one user: an empty allowlist is fail-closed,
so the API rejects it rather than leave you with an agent that looks healthy and
can hear nobody.

### The manual path

What the API does under the hood, kept as reference for debugging a wired agent
or for setting one up without the control plane.

### 1. Create the channel Secret

```bash
kubectl -n kyber-system create secret generic barf-discord \
  --from-literal=bot-token='<discord bot token>' \
  --from-literal=webhook-secret="$(openssl rand -hex 32)" \
  --from-literal=guild-id='234567890123456789' \
  --from-literal=channel-id='345678901234567890' \
  --from-literal=allowed-user-ids='123456789012345678'
```

| Key | Required | Meaning |
| --- | --- | --- |
| `bot-token` | yes | Discord bot token. Sidecar only — it never reaches the runtime. |
| `webhook-secret` | yes | HMAC secret shared with the agent's `discord` inbound binding. |
| `guild-id` | no | CSV. Empty = any server the bot is in. |
| `channel-id` | no | CSV. Empty = any channel. Gates **both** directions: when set, the agent can only be reached in, and can only reply into, these channels. |
| `allowed-user-ids` | no | CSV. **Empty is fail-closed** — nobody can drive the agent. |

All three allowlist keys accept comma-separated values, which is how one agent
serves two servers (see below).

### 2. Enable the channel on the Agent

```bash
kubectl -n kyber-system patch agent barf --type=merge -p '{
  "spec": {"channels": {"discord": {
    "existingSecret": "barf-discord",
    "mentionOnly": true
  }}}
}'
```

The agent also needs a companion inbound binding named `discord` whose
`webhookSecret` matches the Secret's `webhook-secret`, with the signature header
`X-Kyber-Signature-256` and prefix `sha256=`. The binding's **Action** is where
the reply instructions live — static text the agent sees on every inbound
message. It tells the agent to use the `kyber-discord` MCP `reply` tool with
`channel_id`, `text`, and optional `message_id`. The tool returns the created
Discord message ID. If MCP registration is unavailable, the action includes
the loopback `/send` curl fallback. The optional `message_id` creates a Discord
reply reference. The runtime never receives the bot token.

After an inbound message is accepted, the sidecar adds 👀 and refreshes
Discord's typing indicator while the agent works. A successful MCP or `/send`
reply removes 👀 and adds ✅; a failure to dispatch inbound adds ❌. Reaction
or typing permission failures are best-effort and never block message delivery.

Replies longer than Discord's 2,000 UTF-16-unit limit are split at whitespace
near the boundary. Fenced code blocks are closed and reopened around each
split so every chunk renders correctly. Only the first chunk replies to the
inbound message, avoiding repeated mention notifications. MCP returns every
created message ID and reports partial delivery explicitly if a later chunk
fails, so the agent does not resend chunks that already landed.

Accepted inbound attachments are described with an opaque attachment ID,
filename, content type, size, and image dimensions. The `download_attachment`
MCP tool downloads only the newest 256 attachments observed on accepted
messages, only from Discord's CDN, and writes a world-readable file under
`/persist/discord-attachments`. Replies accept absolute file paths under
`/persist`; symlink escapes, non-regular files, and files over 10 MiB are
rejected before upload. Downloads use the same 10 MiB bound and sanitize the
Discord filename. This capability state is in memory and resets with the
sidecar.

The MCP server also exposes `edit_message` for replacing text on a message the
bot sent and `react` for adding or removing the bot's emoji reaction. Both are
restricted to configured channel IDs. Edits retain Discord's single-message
2,000-unit bound; use a new reply for longer content and completion notices.

Messages in a Discord thread are accepted when either the thread itself or its
parent channel is allowlisted. The inbound envelope keeps the thread ID as
`channel_id` (so replies stay in the thread) and also supplies `thread_id`,
`thread_name`, and `parent_channel_id`. The sidecar remembers observed
thread-to-parent scope so `reply`, `edit_message`, and `react` remain allowed.

Inbound prompts also carry `referenced_message` when the author used Discord's
Reply action and `recent_context` with at most five preceding messages. Each
context body is capped at 500 UTF-16 units; this bounds prompt growth while
preserving enough local conversation to resolve pronouns and follow-ups.

> **Upgrading from the direct-REST outbound path.** Agents wired before the bot
> token moved into the sidecar carry an Action telling them to call
> `discord.com/api/...` with `$DISCORD_BOT_TOKEN`. That env var is no longer
> injected, so the old text can only produce failed replies. The reconciler
> rewrites it in place on the next reconcile — no operator action, no pod roll,
> and it logs `migrated legacy Discord binding action…` when it does. A
> hand-tuned Action that does **not** reference `DISCORD_BOT_TOKEN` is treated
> as deliberate and left alone; if yours drives the old REST path under a
> different phrasing, update it by hand or re-`PUT` the channel.

### 3. Roll the pod

> **Agent pods do not auto-roll on a `spec.channels` change.** The sidecar is
> injected at pod-build time, so an existing pod keeps running without it.

```bash
kubectl -n kyber-system delete pod agent-barf
```

## `mentionOnly` — dedicated vs shared channels

| | `mentionOnly: false` (default) | `mentionOnly: true` |
| --- | --- | --- |
| Every allowlisted message in the channel | wakes the agent | ignored |
| `@Agent ...` (the bot user) | wakes the agent | wakes the agent |
| `@Agent ...` (a role the bot holds) | wakes the agent | wakes the agent |
| Reply to one of the agent's own messages | wakes the agent | wakes the agent |
| `@everyone` / `@here` | wakes the agent | ignored |

Default off is right for a **dedicated** agent channel, where every message is
for the agent anyway. Turn it **on** for a **shared** channel where humans also
talk to each other — otherwise every side conversation costs the agent a turn.

It is orthogonal to the allowlist: `allowed-user-ids` says **who** may drive the
agent, `mentionOnly` says **which of their messages count**. When it's on, the
agent's own mention token is stripped from the forwarded text, so the agent sees
`status?` rather than `<@456789012345678901> status?`.

**Both "@Agent" forms count.** Adding a bot to a server auto-creates a *managed
role* with the bot's name, so typing `@Barf` in the composer offers two
autocomplete entries — the user (`<@id>`) and that role (`<@&id>`) — which look
identical once posted. Both wake the agent, and both are stripped from the
forwarded text. The sidecar learns which roles the bot holds from
`GET /guilds/{guild}/members/{bot}` (no privileged intent required), caching the
answer per guild and re-checking a failed guild every 5 minutes, so granting the
bot a new role takes effect without a pod restart. If that lookup fails, it logs
`bot role lookup failed` and role mentions stop counting — user mentions and
replies still work.

`@everyone` deliberately does not count, including via the `@everyone` role
(whose ID is the guild ID) — a server-wide ping must not wake every agent.

## One agent, several servers

Comma-separate the allowlists — the agent is reachable from both, with one
identity and one memory:

```bash
kubectl -n kyber-system patch secret barf-discord --type=merge -p "{\"stringData\":{
  \"guild-id\": \"<server-a>,<server-b>\",
  \"channel-id\": \"<channel-a>,<channel-b>\",
  \"allowed-user-ids\": \"<person-a>,<person-b>\"
}}"
kubectl -n kyber-system delete pod agent-<name>   # picks up the new env
```

Worth saying out loud before you do it: **context crosses servers.** The agent
remembers what was said in server A while talking in server B. If the two
audiences shouldn't share context, run two agents with two bots, not one.

## Verify

```bash
# 1. Sidecar is up and connected.
kubectl -n kyber-system logs agent-<name> -c kyber-mcp-discord | tail
#    discord-sidecar: starting agent=… allowed_users=1 … mention_only=true
#    discord-sidecar: gateway connected

# 2. Send a message in the channel, then confirm the hop.
kubectl -n kyber-system logs agent-<name> -c kyber-mcp-discord | grep forwarded
kubectl -n kyber-system logs deploy/<release>-control-plane | grep 'inbound: dispatched'
#    → POST /webhooks/inbound/<agent>/discord status=200

# 3. Confirm the agent actually replied (proves outbound, without a screenshot).
TOKEN=$(kubectl -n kyber-system get secret <agent>-discord -o jsonpath='{.data.bot-token}' | base64 -d)
curl -s -H "Authorization: Bot $TOKEN" \
  "https://discord.com/api/v10/channels/<channel-id>/messages?limit=10" \
  | jq -r '.[] | "\(.author.username): \(.content)"'
```

## Troubleshooting

| Symptom | Cause |
| --- | --- |
| `ErrImagePull` on `kyber-mcp-discord` | The GHCR package is private. New packages default private and there is **no REST API** to flip it — an owner must do it in the GHCR web UI. |
| Sidecar connects, nothing forwards | Author not in `allowed-user-ids` (empty = deny-all), or wrong channel/guild ID. Sidecar logs the drop at debug level; the 5-minute `dropped inbound messages` summary counts it at info level. |
| Messages forward with empty `content` | Message Content Intent not enabled on the bot. |
| Nothing forwards but mentions work | `mentionOnly` is on. That's the feature. |
| Agent answers some people and ignores others | Check the `dropped inbound messages` summary. `unaddressed_mention_only` climbing while people insist they tagged the agent means the tags aren't landing as mentions — confirm the bot's roles resolved (`resolved bot roles` at startup-ish, or `bot role lookup failed`). |
| Pod has no sidecar container | The pod predates the `spec.channels` change — delete it. |
| Messages sent while the agent slept are lost | Expected: Gateway-only delivery. Keep Discord agents warm. |

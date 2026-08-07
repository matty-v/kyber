# Discord webhooks for agent notifications

> kyber#132 Phase 1: outbound, one-way "the build finished" / "deploy
> done" pings from an agent into a Discord channel. No bot, no Gateway
> connection, no sidecar — just a webhook URL.
>
> **For two-way conversation** (operator messages → agent → reply), see
> [agents-discord-two-way.md](agents-discord-two-way.md) — shipped in
> kyber#646 on the per-channel sidecar architecture (kyber#138).

## When you'd use this

You want an agent to broadcast progress or status into a Discord channel
without standing up a bot or maintaining a Gateway connection. Examples:

- Long-running refactor: agent posts "I've finished the migration on
  branch X" when its job completes.
- CI watcher: an agent triggered by a webhook posts the build outcome
  back into the team's `#deploys` channel.
- Status pings: nightly cron job posts a one-line "kyber-laptop health
  is green" summary into `#kyber-status`.

If you want the agent to *listen* as well as post, you want
[two-way Discord](agents-discord-two-way.md) instead — that needs a bot
and a Gateway-holding sidecar, which this path deliberately avoids.

## Mint a webhook URL in Discord

In Discord, on the channel where the agent will post:

1. **Edit Channel** → **Integrations** → **Webhooks** → **New Webhook**
2. Name it after the agent (e.g. `chewie`) and pick the channel you want
3. Click **Copy Webhook URL**

The URL looks like
`https://discord.com/api/webhooks/<id>/<token>`. Treat it as a bearer
secret — anyone with it can post as the bot.

## Wire it onto an agent

Two steps: enable the spec field, supply the URL.

### At agent-create time

Send the URL alongside the rest of the create request body:

```json
POST /api/v1/agents
{
  "name": "chewie",
  ...
  "secrets": {
    "authType": "oauth",
    "oauthCode": "...",
    "pkceVerifier": "...",
    "discordEnabled": true,
    "discordWebhookUrl": "https://discord.com/api/webhooks/123/abc"
  }
}
```

The control plane:

1. Sets `spec.secrets.discordEnabled: true` on the new Agent CR.
2. Creates a `<agent-name>-discord` k8s Secret with key `webhook-url`
   holding the URL value.
3. The runtime adapter injects `KYBER_DISCORD_WEBHOOK` into the agent
   container's env via `valueFrom.secretKeyRef`.

PWA wizard wire-up is a Phase 1B follow-up — for now, agent creation
via the wizard skips the Discord field; supply the URL via the API
directly or via post-create steps below.

### Retrofit onto an existing agent

If the agent already exists:

```bash
# 1. Create the Secret with the webhook URL.
kubectl -n kyber-system create secret generic chewie-discord \
    --from-literal=webhook-url='https://discord.com/api/webhooks/123/abc'

# 2. Flip the spec field. (Triggers a pod roll on next reconcile.)
kubectl -n kyber-system patch agent chewie --type=merge \
    -p '{"spec":{"secrets":{"discordEnabled":true}}}'
```

The pod re-rolls and lands with `KYBER_DISCORD_WEBHOOK` set inside the
runtime container.

## Posting from the agent

The webhook URL is exposed inside the runtime container as the env var
`KYBER_DISCORD_WEBHOOK`. The simplest invocation is curl:

```bash
curl -X POST -H 'Content-Type: application/json' \
  -d '{"content":"Build complete on branch refactor/auth — diff +124 / -89"}' \
  "$KYBER_DISCORD_WEBHOOK"
```

Discord supports a small JSON body shape. The most useful fields:

- `content` — plain text message (max 2000 chars).
- `username` — override the bot name shown in the channel for this post.
- `embeds` — array of rich embeds (title, description, color, fields).
  See [Discord docs](https://discord.com/developers/docs/resources/webhook#execute-webhook).

A simple wrapper agents can drop into their session:

```bash
discord_post() {
  local message="$1"
  curl -fsS -X POST -H 'Content-Type: application/json' \
    -d "$(jq -nc --arg c "$message" '{content:$c}')" \
    "$KYBER_DISCORD_WEBHOOK" >/dev/null
}
```

If `KYBER_DISCORD_WEBHOOK` is unset (the operator hasn't enabled
Discord for this agent), the curl call exits non-zero and the agent
should treat it as "no Discord channel configured" — not an error.

## Out of scope (will land later)

- ~~**Bidirectional comms**~~ — shipped in kyber#646; see
  [agents-discord-two-way.md](agents-discord-two-way.md).
- **Wizard PWA wire-up** — Phase 1B follow-up; the API is ready, the
  wizard form is not.
- **Slash-command integration** (`/kyber restart <agent>`) — a separate
  initiative; would talk to the control plane, not the agent.
- **Per-channel routing** — one webhook = one channel on this path. The
  two-way sidecar allowlists multiple channels per agent.

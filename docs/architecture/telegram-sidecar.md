# Telegram sidecar

## Scope

This page describes the runtime-neutral Telegram bridge. Operator-visible setup
and behavior live in [`../agents-comms.md`](../agents-comms.md#telegram).

Source of truth:

- `cmd/kyber-mcp-telegram/` — polling, envelopes, MCP tools, Telegram Bot API
- `pkg/controllers/agent/telegram_sidecar.go` — pod injection and binding contract
- `pkg/controllers/agent/telegram_migrate.go` — convergence of existing agents
- `pkg/api/routes_agent_comms.go` and `pkg/api/routes_agents.go` — configuration

## Components and flow

One `kyber-mcp-telegram` container is appended to an enabled agent pod. The bot
token, numeric user allowlist, and inbound HMAC secret come from the agent's
`<name>-telegram` Secret and are not mounted into the runtime container.

Inbound flow:

1. The sidecar long-polls `getUpdates` for messages, edits, callbacks, and
   reaction changes.
2. It rejects non-allowlisted senders before adding chat or file capabilities.
3. Album messages sharing `media_group_id` settle into one envelope.
4. It signs the JSON envelope and posts it to
   `/webhooks/inbound/<agent>/telegram`.
5. Kyber's normal inbound verification, deduplication, filtering, rate limit,
   queue, and tmux delivery path handles the prompt.

The offset advances only after synchronous handling succeeds. A failing update
is retried up to five times before being dropped loudly so one poison update
cannot wedge every later message. Album flushes are asynchronous after their
settle window and independently retry delivery up to five times before logging
a terminal failure.

Outbound flow uses the loopback MCP endpoint at port 14006. The text-only
`/send` endpoint at port 14004 remains a compatibility fallback. Both are scoped
to allowlisted users' direct chats and group chats previously observed through
accepted inbound traffic.

## State and bounds

The sidecar keeps only bounded in-memory capability state:

- newest 256 observed inbound file IDs;
- newest 256 opaque callback tokens;
- at most 32 pending albums with at most 10 items each;
- active typing leases by chat.

State intentionally disappears on pod restart. A callback or file ID from a
previous sidecar process is therefore expired rather than a durable authority.

Uploads must resolve to regular files below `/persist`; symlink traversal and
payloads above 20 MiB are rejected before calling Telegram. Downloads accept
only observed file IDs and enforce Telegram's 20 MiB bot-download limit.

## Callback and keyboard contract

The model supplies button labels and internal values. The sidecar generates
random callback tokens for Telegram and retains the mapping. A valid callback
must match both the token and chat, is consumed once, is acknowledged immediately,
and becomes a signed inbound `event_type=callback` envelope. Unknown or expired
tokens are acknowledged but not forwarded. A keyboard accepts at most 100
buttons; labels are limited to 64 characters and internal values to 1,024 bytes.

`edit_message.buttons` is tri-state: omitted preserves Telegram's current reply
markup, a non-empty array replaces it, and an empty array sends an empty inline
keyboard to remove it. Replacement and removal invalidate tokens associated
with the prior message keyboard.

## Runtime guidance

Both runtime images contain the same platform-owned
`images/shared/skills/telegram-messaging` skill. Their boot scripts link it into
the runtime's native skill directory only when `KYBER_TELEGRAM_MCP_URL` is set.
An existing identity-repo skill wins. The runtime image build path includes
`images/shared/**`, so changes to the cookbook rebuild both runtime images.

The inbound binding action is the smaller always-present contract. Migration
upgrades only exact historical default actions and appends missing canonical
fields; operator-customized action text and fields are preserved.

## Operational signals

- `TelegramUnavailable/NoTelegramSidecarImage`: the install has no sidecar image.
- `TelegramUnavailable/NoTelegramAllowlist`: the agent has no usable allowlist.
- `TelegramSidecarRollHeld`: controlled convergence is waiting on its canary gate.
- Sidecar logs distinguish unsupported, blocked, empty, forwarded, retried, and
  terminally dropped updates without logging the bot token.

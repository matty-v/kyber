# Telegram channel

> **Verification status:** verified against the local full stack with a live
> Telegram bot and Echo agent.

## Concept

Telegram is an optional two-way channel for any Claude Code or Codex agent. Each
agent uses its own bot token and numeric-user allowlist. The runtime never sees
the bot credential.

## Observable behavior

An allowlisted user can send text, edits, supported files, photo/video albums,
inline-button selections, and reaction changes. While the agent works, Telegram
shows its typing indicator. The agent can reply with text and files, thread a
reply, react, edit earlier text, and create, replace, or clear inline keyboards.

Images, animations, video, audio, voice, and documents use Telegram-native
presentation. Two to ten compatible photos/videos sent together appear as one
media album. Incoming albums arrive to the agent as one task rather than one
task per item.

## States

- **Disabled:** no Telegram sidecar is present.
- **Enabled and available:** the configured bridge is running and polling.
- **Unavailable:** the install lacks a Telegram sidecar image or the agent has
  no usable allowlist; the Agent condition explains which requirement is absent.
- **Update pending:** the controller waits until the agent is idle and the
  sidecar image canary gate permits a safe pod roll.

## Operator actions

Configure, update, or disable Telegram from the agent's Comms surface or its
`/api/v1/agents/{name}/comms/telegram` endpoint. Bot credentials are write-only.
Existing agents converge to the configured sidecar automatically; operators do
not need to delete their pods manually.

See [`../agents-comms.md`](../agents-comms.md#telegram) for setup, limits, tool
behavior, migration, and troubleshooting. See
[`../architecture/telegram-sidecar.md`](../architecture/telegram-sidecar.md)
for the implementation contracts.

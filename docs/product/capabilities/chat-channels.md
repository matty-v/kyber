# Chat channels

You can talk to any Kyber agent over Telegram or Discord, in both directions. That means you can assign work, answer an agent's question, and get results from your phone, without opening a terminal.

## What a channel is

A comms channel connects one agent to one bot. You configure it from the agent's Comms tab in the [fleet console](fleet-console.md), or through the same API it calls. Each channel needs a bot token and an allowlist of user IDs; nobody outside the allowlist can drive the agent, and an empty allowlist is fail-closed rather than open. Credentials are write-only: tokens go in but are never returned by any endpoint, and the agent runtime itself never sees the bot token. The token lives in a sidecar that bridges the chat service to Kyber's signed inbound dispatcher, and a second save with a new token rotates it, which is the path to take when a token leaks.

Once a channel is configured, the agent converges to it on its own: Kyber rolls the pod onto the right bridge when the agent is idle, never interrupting a live session.

## What conversations look like

Over Telegram, you can send text, edits, files, photo and video albums, inline-button selections, and reactions. While the agent works, Telegram shows its typing indicator. The agent can reply with text and files, thread a reply, react, edit earlier messages, and offer inline button keyboards. Media uses Telegram-native presentation, so photos arrive as photos and two to ten compatible photos or videos arrive as one album; an incoming album reaches the agent as one task rather than one per item.

Over Discord, an accepted message gets an eyes reaction and a typing indicator while the agent works, then a checkmark once it replies. Long replies split cleanly at Discord's message limit with code blocks preserved. A `mentionOnly` setting lets an agent share a channel with humans and respond only when tagged: the allowlist says who may drive the agent, `mentionOnly` says which of their messages count. Threads, reply references, and recent conversation context are forwarded so follow-ups make sense, and files flow in both directions.

Every accepted message, from either service, enters the same signed, rate-limited inbound dispatcher that all inbound work uses, so a chat message is verified the same way as any other sender.

## Channels need a running agent

Both channels deliver to a running agent. A stopped agent does not receive chat messages; you bring it back by starting it from the [fleet console](fleet-console.md) or the API. Keep chat-driven agents running. Discord in particular delivers messages only over a live gateway connection held by the agent's pod, so messages sent while a Discord agent is down are lost.

There is also a lighter, one-way Discord option: an outbound webhook the agent posts progress and status into, with no bot and no gateway connection. Use it when you want notifications, not conversation.

Both channels work with either the Claude Code or Codex [runtime](runtimes.md).

## Learn more

- [Configuring an agent's comms channels](../../agents-comms.md): setup, limits, and troubleshooting for both channels.
- [Two-way Discord for agents](../../agents-discord-two-way.md)
- [Outbound-only Discord webhooks](../../agents-discord-webhooks.md)
- [Scheduled jobs and handoffs](scheduled-jobs.md): the other ways work reaches an agent.

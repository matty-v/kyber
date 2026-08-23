# Ops from your phone

Assign work, approve decisions, and get results over Telegram or Discord, without opening a laptop.

Most agent setups assume you are at a terminal. Kyber assumes you are not. Every agent in the fleet can hold a two-way conversation over Telegram or Discord, so the phone in your pocket becomes the interface to your infrastructure.

That turns dead time into working time. You can kick off a migration from the couch, answer an agent's clarifying question from the grocery store, and read the result before you are back at a desk.

## What a phone-first day looks like

- You message an agent an idea in the morning: "move the backup job to the new bucket and verify a restore."
- The agent works in its own pod, with its own tools and credentials. When it hits a real decision, it asks you, and you answer in chat.
- When something breaks at 9pm, the on-call agent tells you what it found, not just that something is wrong. Often the message is "found it, fixed it, here is the diff."

The conversation is the audit trail. What you asked for, what the agent decided, and what it shipped are all in one thread.

## Not just notifications

Plenty of tools can page you. The difference here is that the thing messaging you can also do the work. Channels are two-way: an inbound message lands as the agent's next prompt, and the agent replies from inside the pod where the work actually happened. Both channels need the agent running to receive; a stopped agent comes back when you start it from the console or the API, not from a chat message.

When you do want a screen, the [fleet console](../capabilities/fleet-console.md) shows the whole fleet: status, activity, and a shell into any agent, from a browser.

## Start here

- [Chat: Telegram and Discord](../capabilities/chat-channels.md): how channels bind to agents.
- [Quickstart](../getting-started/quickstart.md): get a fleet console running first.

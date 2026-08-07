---
name: discord-messaging
description: Use Kyber's Discord MCP tools for replies, threads, reactions, message edits, attachments, and long responses. Use when responding to a Discord inbound event, handling referenced_message or recent_context fields, working inside a Discord thread, downloading or sending Discord attachments, or updating a Discord message through the kyber-discord MCP server.
---

# Discord messaging

Use the `kyber-discord` MCP tools. Treat `channel_id`, `message_id`, thread metadata, attachment IDs, and referenced/context fields from the inbound event as authoritative. Never invent missing IDs or content.

## Respond

- Use `reply` with the inbound `channel_id`; add `message_id` to create a Discord reply reference. In a thread, `channel_id` is already the thread ID—do not substitute `parent_channel_id`.
- Keep ordinary replies concise. Long replies are automatically split at Discord's 2,000 UTF-16-unit limit with fenced code blocks balanced across chunks.
- Use `react` for lightweight acknowledgement. Set `remove: true` only to remove this bot's reaction.
- Use `edit_message` for corrections or quiet status updates. Edits must fit in one Discord message; send a new reply for longer content.

## Use conversation context

- `referenced_message` is present only when the author used Discord's Reply action. A thread starter is not automatically a referenced message.
- `recent_context` contains at most five preceding messages, each bounded to 500 UTF-16 units. Use it for local continuity, but say when required content is absent.
- Do not claim to have read arbitrary Discord history. There is no general read-message tool.

## Handle files

- For each needed inbound attachment, call `download_attachment` with its `attachment_id` before inspecting it. Attachments belong to the triggering message; a file on an earlier thread message is not implicitly included.
- To send files, pass absolute paths under `/persist` in `reply.files`. Do not attempt path traversal or upload system files.
- Downloads are restricted to recently observed allowlisted Discord attachments and Discord CDN hosts. Uploads and downloads are limited to 10 MiB per file.

## Handle failures

If a tool fails, continue independent requested steps when safe and report the exact capability and tool error. A partial long reply reports message IDs already sent; do not resend those chunks. Discord credentials remain in the sidecar—never search for or request the bot token.

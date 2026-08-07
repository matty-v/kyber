---
name: telegram-messaging
description: Use Kyber's Telegram MCP tools for conversations, inline buttons and callbacks, reactions, message edits, attachments, albums, and native media. Use when responding to a Telegram inbound event, designing a Telegram interaction, handling callback_value or reaction fields, downloading Telegram attachments, or sending files through the kyber-telegram MCP server.
---

# Telegram messaging

Use the `kyber-telegram` MCP tools. Treat `chat_id`, `message_id`, attachment file IDs, and callback values from the inbound event as authoritative.

## Choose an interaction

- Send normal conversation with `reply`. Keep messages concise and mobile-friendly.
- Ask a bounded question with `reply.buttons`, using a short visible `text` and a stable internal `value`.
- On a callback, act on `callback_value`, then call `edit_message` on the original `message_id` with `buttons: []` to prevent repeat choices. Update the text to show the selection when useful.
- Report meaningful progress with a new `reply`; use `edit_message` for quiet corrections or live status that should not trigger another notification.
- Use `react` for lightweight acknowledgement, not as a substitute for a requested answer.

## Handle attachments and albums

For a single attachment, call `download_attachment` with `attachment_file_id` before inspecting it. For an album, parse `attachments` as a JSON array and download each needed `file_id`. Do not invent file IDs or paths.

To send files, pass absolute paths under `/persist` in `reply.files`. Kyber chooses Telegram-native photo, animation, video, audio, voice, or document methods by extension. Two to ten compatible photos/videos are sent as one native media group.

## Button lifecycle

`edit_message.buttons` has three distinct states:

- Omit `buttons` to preserve the current keyboard.
- Pass a non-empty array to replace the keyboard.
- Pass `buttons: []` to remove it and invalidate its callbacks.

Use button values as internal routing identifiers, not secrets. Callback tokens are opaque, bounded, chat-scoped, and one-shot. Use at most 100 buttons per keyboard, keep labels to 64 characters, and keep each internal value under 1,024 bytes.

## Constraints

Only message chats already in this agent's scope. Only download file IDs observed from allowlisted inbound events. Upload only files under `/persist`; the aggregate upload limit is 20 MB. Prefer plain text unless Telegram MarkdownV2 formatting materially improves readability and escape its special characters correctly.

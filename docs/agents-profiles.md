# Agent profiles and private avatars

General settings in Agent Detail edit an agent's alias and description through
`PATCH /api/v1/agents/{name}/profile`. Avatar storage is currently an API feature;
the console has no avatar upload control.

## Avatar API

`GET`, `PUT`, and `DELETE /api/v1/agents/{name}/profile/avatar` use the normal
operator API authentication. `PUT` accepts raw image bytes, not multipart data:

```bash
curl --fail-with-body -X PUT "$KYBER/api/v1/agents/alice/profile/avatar" \
  -H "Authorization: Bearer $KYBER_API_KEY" \
  -H 'Content-Type: image/png' \
  --data-binary @avatar.png
```

Accepted formats are PNG, JPEG, and WebP, up to 1 MiB. The handler checks the
content-type and image signature. `PUT` returns the updated Agent response;
`profile.avatarUrl` points to the authenticated API route rather than a public
bucket URL. `GET` returns image bytes with private caching. `DELETE` removes
metadata and the stored object and returns 204. These operations do not restart
the agent.

The current control-plane wiring initializes avatar storage through the task
object store only when `api.durableTasks.enabled` is true. Configure task object
storage (GCS or S3-compatible); the task configuration can fall back to the log
archive bucket/backend. See [durable tasks](architecture/durable-tasks.md) for
configuration and [logging](operator/logging.md) for archive setup. Without the
object store, avatar operations return `503 avatar_unavailable`; an absent
avatar returns 404. An unsupported format returns 415. Oversized or unreadable
bodies are rejected before upload.

Source: `pkg/api/routes_agents.go` (`handleAgentAvatar`, `validatedAvatarType`),
`pkg/api/v1/agent_types.go` (profile metadata), and `cmd/control-plane/main.go`
(task object-store initialization). The Agent CRD stores an opaque object key
and content type, never the image bytes.

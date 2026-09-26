# Agent profiles and avatars

General settings in Agent Detail edit an agent's alias and description through
`PATCH /api/v1/agents/{name}` with a `profile` object. The same card has an
**Avatar** row: upload or replace an image (pick a file or drop it on the
preview), or remove it. Avatars appear in the agent list and the Agent Detail
header; an agent without one shows its initials on a stable color.

## How avatars are stored

An uploaded image is **normalized** before it is stored:

- **Crop and scale:** it is center-cropped to a square and scaled to at most
  256 px. A smaller image is never upscaled.
- **Re-encode:** it is re-encoded as PNG if it has any transparency, and as
  JPEG (quality 85) otherwise.
- **No metadata:** re-encoding drops EXIF, which includes GPS, and every other
  metadata block.

The result is kept in a ConfigMap named `<agent>-avatar` that is owned by the
Agent:

- **Always available:** no object store or bucket is needed, so avatars work
  on every installation.
- **Deleted with the agent:** deleting the agent deletes its avatar.
- **Small by design:** a stored avatar is at most 256 KiB, typically 20–60 KB.
  The control plane caches the namespace's ConfigMaps, so avatars are kept
  small on purpose.

The Agent CRD records only an opaque key and the content type, never image
bytes or a public URL.

## Avatar API

`GET`, `PUT`, and `DELETE /api/v1/agents/{name}/profile/avatar` use the normal
operator API authentication. `PUT` takes raw image bytes, not multipart data:

```bash
curl --fail-with-body -X PUT "$KYBER/api/v1/agents/alice/profile/avatar" \
  -H "Authorization: Bearer $KYBER_API_KEY" \
  -H 'Content-Type: image/png' \
  --data-binary @avatar.png
```

**`PUT` input and response**
- Accepts PNG, JPEG or WebP, up to 1 MiB and 4096 px per side.
- The dimension limit is checked from the image header before decoding, so a
  small file can't claim huge dimensions and force a huge decode.
- Returns the updated Agent. `profile.avatarUrl` is
  `/api/v1/agents/<name>/profile/avatar?v=<version>`; the version changes
  whenever the image does.

**`GET` caching**
- With a matching `v`, the image is cacheable as `immutable`, so a browser
  fetches each version once.
- Without it, the response carries an `ETag` and must be revalidated
  (`If-None-Match` returns 304).

**`DELETE`**
- Removes the avatar and returns 204, even when there was none.
- None of these calls restart the agent.

**Errors**

| Status | Code | Meaning |
|---|---|---|
| 413 | `avatar_too_large` | Over 1 MiB |
| 415 | `invalid_avatar_type` | Declared type isn't PNG, JPEG or WebP |
| 415 | `invalid_avatar` | Bytes don't match the declared type |
| 400 | `invalid_avatar` | Dimensions over the limit, or the image can't be processed |
| 404 | | The agent has no avatar |

The avatar URL needs the caller's credentials:

- **Embedded console:** it uses its session cookie, so a plain `<img>` works.
- **Hub** (API-key) clients: they fetch the image with the `Authorization`
  header. The `AgentAvatar` component in `@matty-v/kyber-pwa-views` does this
  for you.

## Avatars from before MAT-91

Before MAT-91, avatars lived in the durable-tasks object store under
`agent-avatars/<name>`. Those avatars:

- still display while that store is configured;
- move to a ConfigMap on the next upload, which also removes the old object;
- have their old object removed when the agent is deleted.

Disk exports carry the avatar, and an import normalizes it and stores it in a
ConfigMap owned by the new agent.

Source:
- `pkg/api/avatar.go`: normalization and storage
- `pkg/api/routes_agents.go`: `handleAgentAvatar`
- `pkg/api/v1/agent_types.go`: profile metadata
- `packages/pwa-views/src/components/AgentAvatar.tsx` and `AgentAvatarSettings.tsx`: the console

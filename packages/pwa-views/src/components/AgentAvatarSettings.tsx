import { useRef, useState, type DragEvent } from 'react'
import { Button } from './Button'
import { ConfirmDialog } from './ConfirmDialog'
import { AgentAvatar } from './AgentAvatar'
import { useDeleteAgentAvatar, useUploadAgentAvatar } from '../hooks/useAPI'
import { AVATAR_TYPES, avatarErrorMessage, avatarFileProblem } from '../lib/avatar'

interface Props {
  name: string
  alias?: string
  avatarUrl?: string
}

// AgentAvatarSettings is the Avatar row of an agent's profile: a preview that
// also takes a dropped image, upload or replace, and remove. The server squares
// and shrinks the image, so any reasonable photo works.
export function AgentAvatarSettings({ name, alias, avatarUrl }: Props) {
  const upload = useUploadAgentAvatar()
  const remove = useDeleteAgentAvatar()
  const input = useRef<HTMLInputElement>(null)
  const [error, setError] = useState<string | null>(null)
  const [confirming, setConfirming] = useState(false)
  const [dragging, setDragging] = useState(false)
  const busy = upload.isPending || remove.isPending

  function send(file: File | undefined) {
    if (!file) return
    const problem = avatarFileProblem(file)
    if (problem) {
      setError(problem)
      return
    }
    setError(null)
    upload.mutate({ name, file }, { onError: (err) => setError(avatarErrorMessage(err)) })
  }

  function onDrop(event: DragEvent) {
    event.preventDefault()
    setDragging(false)
    if (!busy) send(event.dataTransfer.files[0])
  }

  return (
    <div>
      <span className="block text-xs font-medium text-text-muted">Avatar</span>
      <div className="mt-1 flex items-center gap-3">
        <div
          onDragOver={(event) => { event.preventDefault(); setDragging(true) }}
          onDragLeave={() => setDragging(false)}
          onDrop={onDrop}
          className={`rounded-lg ${dragging ? 'ring-2 ring-accent' : ''}`}
          data-testid="avatar-dropzone"
        >
          <AgentAvatar name={name} alias={alias} avatarUrl={avatarUrl} size="lg" />
        </div>
        <div className="flex flex-wrap gap-2">
          <Button type="button" variant="secondary" size="sm" loading={upload.isPending} disabled={busy} onClick={() => input.current?.click()}>
            {avatarUrl ? 'Replace' : 'Upload'}
          </Button>
          {avatarUrl && (
            <Button type="button" variant="ghost" size="sm" disabled={busy} onClick={() => setConfirming(true)}>
              Remove
            </Button>
          )}
        </div>
        <input
          ref={input}
          type="file"
          accept={AVATAR_TYPES.join(',')}
          className="hidden"
          aria-label="Avatar image file"
          onChange={(event) => {
            send(event.target.files?.[0])
            event.target.value = ''
          }}
        />
      </div>
      <p className="mt-1 text-xs text-text-muted">PNG, JPEG, or WebP up to 1 MiB. Cropped to a square. Drop an image on the preview or choose a file.</p>
      {error && <p role="alert" className="mt-1 text-xs text-danger">{error}</p>}
      <ConfirmDialog
        open={confirming}
        title="Remove this avatar?"
        message="The agent goes back to showing its initials. This doesn't restart the agent."
        confirmLabel="Remove"
        dangerous
        loading={remove.isPending}
        onCancel={() => setConfirming(false)}
        onConfirm={() => {
          setError(null)
          remove.mutate(
            { name },
            {
              onSuccess: () => setConfirming(false),
              onError: (err) => {
                setConfirming(false)
                setError(avatarErrorMessage(err))
              },
            },
          )
        }}
      />
    </div>
  )
}

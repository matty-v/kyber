import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'

const { upload, remove } = vi.hoisted(() => ({
  upload: { mutate: vi.fn(), isPending: false },
  remove: { mutate: vi.fn(), isPending: false },
}))

vi.mock('../hooks/useAPI', () => ({
  useUploadAgentAvatar: () => upload,
  useDeleteAgentAvatar: () => remove,
}))

import { AgentAvatarSettings } from './AgentAvatarSettings'

const png = (bytes = 10) => new File([new Uint8Array(bytes)], 'me.png', { type: 'image/png' })

describe('AgentAvatarSettings', () => {
  beforeEach(() => {
    vi.clearAllMocks()
  })

  it('uploads a chosen image', async () => {
    const user = userEvent.setup()
    render(<AgentAvatarSettings name="vault" />)
    expect(screen.getByRole('button', { name: 'Upload' })).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Remove' })).not.toBeInTheDocument()
    const file = png()
    await user.upload(screen.getByLabelText('Avatar image file'), file)
    expect(upload.mutate).toHaveBeenCalledWith({ name: 'vault', file }, expect.anything())
  })

  it('refuses a wrong type or oversized file before uploading', async () => {
    const user = userEvent.setup({ applyAccept: false })
    render(<AgentAvatarSettings name="vault" />)
    await user.upload(screen.getByLabelText('Avatar image file'), new File(['x'], 'a.gif', { type: 'image/gif' }))
    expect(screen.getByRole('alert')).toHaveTextContent('PNG, JPEG, or WebP')
    await user.upload(screen.getByLabelText('Avatar image file'), png((1 << 20) + 1))
    expect(screen.getByRole('alert')).toHaveTextContent('1 MiB')
    expect(upload.mutate).not.toHaveBeenCalled()
  })

  it('shows the server error for a rejected upload', async () => {
    const user = userEvent.setup()
    upload.mutate.mockImplementation((_vars, opts: { onError: (e: unknown) => void }) =>
      opts.onError({ status: 400, code: 'invalid_avatar', message: 'image is larger than 4096 px' }),
    )
    render(<AgentAvatarSettings name="vault" />)
    await user.upload(screen.getByLabelText('Avatar image file'), png())
    expect(screen.getByRole('alert')).toHaveTextContent('image is larger than 4096 px')
  })

  it('replaces and removes an existing avatar after confirming', async () => {
    const user = userEvent.setup()
    remove.mutate.mockImplementation((_vars, opts: { onSuccess: () => void }) => opts.onSuccess())
    render(<AgentAvatarSettings name="vault" avatarUrl="/api/v1/agents/vault/profile/avatar?v=1" />)
    expect(screen.getByRole('button', { name: 'Replace' })).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: 'Remove' }))
    expect(screen.getByText('Remove this avatar?')).toBeInTheDocument()
    const confirm = screen.getAllByRole('button', { name: 'Remove' }).at(-1)!
    await user.click(confirm)
    expect(remove.mutate).toHaveBeenCalledWith({ name: 'vault' }, expect.anything())
    expect(screen.queryByText('Remove this avatar?')).not.toBeInTheDocument()
  })
})

import { describe, it, expect, vi, afterEach } from 'vitest'
import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { AgentAvatar } from './AgentAvatar'
import { ClusterProvider, type Cluster } from '../lib/cluster-context'

const embedded: Cluster = { id: 'local', name: 'local', baseURL: '/', apiKey: '', version: '', capabilities: [] }
const hub: Cluster = { id: 'c1', name: 'gcp', baseURL: 'https://kyber.example/', apiKey: 'k3y', version: '', capabilities: [] }
const url = '/api/v1/agents/vault/profile/avatar?v=abc123'

afterEach(() => {
  vi.unstubAllGlobals()
  vi.restoreAllMocks()
})

describe('AgentAvatar', () => {
  it('shows initials from the alias when there is no avatar', () => {
    render(<AgentAvatar name="vault" alias="Vault Keeper" />)
    const el = screen.getByTestId('agent-avatar-initials')
    expect(el).toHaveTextContent('VK')
    expect(el).toHaveAccessibleName('Vault Keeper avatar')
  })

  it('shows initials outside any cluster provider', () => {
    render(<AgentAvatar name="vault" avatarUrl={url} />)
    // No cluster: embedded-style relative URL.
    expect(screen.getByTestId('agent-avatar-image')).toHaveAttribute('src', url)
  })

  it('uses a plain image in embedded cookie mode', () => {
    const fetchSpy = vi.fn()
    vi.stubGlobal('fetch', fetchSpy)
    render(
      <ClusterProvider value={embedded}>
        <AgentAvatar name="vault" avatarUrl={url} />
      </ClusterProvider>,
    )
    expect(screen.getByTestId('agent-avatar-image')).toHaveAttribute('src', url)
    expect(fetchSpy).not.toHaveBeenCalled()
  })

  it('fetches with the bearer key in hub mode and shows a blob URL', async () => {
    const fetchSpy = vi.fn().mockResolvedValue({ ok: true, status: 200, blob: async () => new Blob(['img'], { type: 'image/png' }) })
    vi.stubGlobal('fetch', fetchSpy)
    const create = vi.spyOn(URL, 'createObjectURL').mockReturnValue('blob:avatar-1')
    const revoke = vi.spyOn(URL, 'revokeObjectURL').mockImplementation(() => {})
    const { unmount } = render(
      <ClusterProvider value={hub}>
        <AgentAvatar name="vault" avatarUrl={url} />
      </ClusterProvider>,
    )
    // Initials while it loads.
    expect(screen.getByTestId('agent-avatar-initials')).toBeInTheDocument()
    await waitFor(() => expect(screen.getByTestId('agent-avatar-image')).toHaveAttribute('src', 'blob:avatar-1'))
    expect(fetchSpy).toHaveBeenCalledWith(`https://kyber.example${url}`, { headers: { Authorization: 'Bearer k3y' } })
    expect(create).toHaveBeenCalledOnce()
    unmount()
    await waitFor(() => expect(revoke).toHaveBeenCalledWith('blob:avatar-1'))
  })

  it('falls back to initials when the hub fetch fails', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue({ ok: false, status: 404 }))
    render(
      <ClusterProvider value={hub}>
        <AgentAvatar name="vault" avatarUrl={`${url}-missing`} />
      </ClusterProvider>,
    )
    await new Promise((r) => setTimeout(r, 0))
    expect(screen.getByTestId('agent-avatar-initials')).toHaveTextContent('VA')
  })

  it('falls back to initials when the image fails to load', () => {
    render(
      <ClusterProvider value={embedded}>
        <AgentAvatar name="vault" avatarUrl={url} />
      </ClusterProvider>,
    )
    fireEvent.error(screen.getByTestId('agent-avatar-image'))
    expect(screen.getByTestId('agent-avatar-initials')).toBeInTheDocument()
  })
})

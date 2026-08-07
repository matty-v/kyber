import { describe, it, expect, vi, beforeEach } from 'vitest'
import { act, render, screen, waitFor } from '@testing-library/react'
import { EmbeddedClusterProvider } from './EmbeddedClusterProvider'
import { useCluster } from '@matty-v/kyber-pwa-views'

function ShowCluster() {
  const c = useCluster()
  return (
    <div>
      <span data-testid="name">{c.name}</span>
      <span data-testid="capabilities">{c.capabilities.join(',')}</span>
    </div>
  )
}

function ShowApiKey() {
  const c = useCluster()
  return <span data-testid="apikey">{c.apiKey}</span>
}

const validResponse = {
  name: 'kyber-local',
  version: '1.6.0',
  capabilities: ['agents', 'machines', 'shell'],
}

beforeEach(() => {
  localStorage.clear()
  localStorage.setItem('kyber_api_key', 'sk-test')
  vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo) => {
    if (String(input).endsWith('/api/v1/cluster-info')) {
      return new Response(JSON.stringify(validResponse), { status: 200 })
    }
    return new Response('not found', { status: 404 })
  }))
})

describe('EmbeddedClusterProvider', () => {
  it('fetches cluster-info on mount and provides the Cluster value', async () => {
    render(
      <EmbeddedClusterProvider>
        <ShowCluster />
      </EmbeddedClusterProvider>,
    )

    await waitFor(() => {
      expect(screen.getByTestId('name').textContent).toBe('kyber-local')
    })
    expect(screen.getByTestId('capabilities').textContent).toBe('agents,machines,shell')
  })

  it('shows a loading state before cluster-info resolves', () => {
    render(
      <EmbeddedClusterProvider>
        <ShowCluster />
      </EmbeddedClusterProvider>,
    )
    expect(screen.getByText(/loading cluster/i)).toBeInTheDocument()
  })

  it('shows an error state when cluster-info fetch fails', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => new Response('boom', { status: 500 })))

    render(
      <EmbeddedClusterProvider>
        <ShowCluster />
      </EmbeddedClusterProvider>,
    )

    await waitFor(() => {
      expect(screen.getByText(/could not reach the kyber control plane/i)).toBeInTheDocument()
    })
  })

  it('updates cluster.apiKey when setEmbeddedApiKey dispatches the kyber:apikey-changed event', async () => {
    render(
      <EmbeddedClusterProvider>
        <ShowApiKey />
      </EmbeddedClusterProvider>,
    )
    await waitFor(() => {
      expect(screen.getByTestId('apikey').textContent).toBe('sk-test')
    })

    // Simulate a Save: write to localStorage and dispatch the event.
    await act(async () => {
      localStorage.setItem('kyber_api_key', 'sk-rotated')
      window.dispatchEvent(new CustomEvent('kyber:apikey-changed'))
    })

    await waitFor(() => {
      expect(screen.getByTestId('apikey').textContent).toBe('sk-rotated')
    })
  })

  it('renders without crashing when no api key is set (first-install path)', async () => {
    localStorage.clear()  // override the beforeEach
    // The fetch stub still returns the valid response — cluster-info is on
    // the public mux per Task A4, so an empty key is acceptable.
    render(
      <EmbeddedClusterProvider>
        <ShowCluster />
      </EmbeddedClusterProvider>,
    )
    await waitFor(() => {
      expect(screen.getByTestId('name').textContent).toBe('kyber-local')
    })
  })
})

import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { BrowserRouter } from 'react-router-dom'
import { QueryClient, QueryClientProvider, MutationCache } from '@tanstack/react-query'
import { toast } from 'sonner'
import {
  App,
  DensityProvider,
  TooltipProvider,
  Toaster,
  enableMocks,
} from '@matty-v/kyber-pwa-views'
// ./index.css is the single Tailwind v4 entry — it @imports
// @matty-v/kyber-pwa-views/styles.css internally so Tailwind processes the
// package's @theme block + this app's @source directives in one pass.
import './index.css'
import { EmbeddedClusterProvider } from './EmbeddedClusterProvider'

type MutationMeta = {
  successMessage?: string | ((data: unknown, variables: unknown) => string)
  errorPrefix?: string
}

function messageFromError(err: unknown): string {
  if (err instanceof Error && err.message) return err.message
  return 'Something went wrong'
}

const queryClient = new QueryClient({
  mutationCache: new MutationCache({
    onSuccess: (data, variables, _ctx, mutation) => {
      const meta = mutation.meta as MutationMeta | undefined
      if (!meta?.successMessage) return
      const text =
        typeof meta.successMessage === 'function'
          ? meta.successMessage(data, variables)
          : meta.successMessage
      toast.success(text)
    },
    onError: (err, _variables, _ctx, mutation) => {
      const meta = mutation.meta as MutationMeta | undefined
      const prefix = meta?.errorPrefix ?? 'Action failed'
      toast.error(prefix, { description: messageFromError(err) })
    },
  }),
  defaultOptions: { queries: { staleTime: 10_000, retry: 2 } },
})

const rootEl = document.getElementById('root')
if (!rootEl) throw new Error('Root element not found')

// An auto-updated service worker can take control while an already-open tab
// continues executing the previous hashed JavaScript bundle. Reload once when
// an existing controller is replaced so the UI and control plane stay on the
// same local build. Do not reload on the app's first service-worker install.
if ('serviceWorker' in navigator && navigator.serviceWorker.controller) {
  let refreshing = false
  navigator.serviceWorker.addEventListener('controllerchange', () => {
    if (refreshing) return
    refreshing = true
    window.location.reload()
  })
}

if (import.meta.env.VITE_ENABLE_MOCKS === '1') {
  ;(window as unknown as { __kyberQueryClient: typeof queryClient }).__kyberQueryClient = queryClient
}

void enableMocks().then(() => {
  createRoot(rootEl).render(
    <StrictMode>
      <DensityProvider>
        <QueryClientProvider client={queryClient}>
          <TooltipProvider delayDuration={200}>
            <BrowserRouter>
              <EmbeddedClusterProvider>
                <App />
              </EmbeddedClusterProvider>
            </BrowserRouter>
            <Toaster />
          </TooltipProvider>
        </QueryClientProvider>
      </DensityProvider>
    </StrictMode>,
  )
})

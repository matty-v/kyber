import { useContext, useEffect, useState } from 'react'
import { ClusterContext, type Cluster } from '../lib/cluster-context'
import { avatarColor, avatarInitials } from '../lib/avatar'

type Size = 'sm' | 'md' | 'lg'

const SIZES: Record<Size, string> = {
  sm: 'h-7 w-7 text-[11px]',
  md: 'h-9 w-9 text-xs',
  lg: 'h-16 w-16 text-lg',
}

interface Props {
  /** Agent name; seeds the fallback color. */
  name: string
  /** Operator alias, preferred for the initials and alt text. */
  alias?: string
  /** profile.avatarUrl: a versioned API path, absent when there is none. */
  avatarUrl?: string
  size?: Size
  className?: string
}

// AgentAvatar shows an agent's avatar, or its initials on a stable color when
// it has none or the image fails to load.
//
// The avatar route needs the caller's credentials. Embedded mode rides the
// HttpOnly session cookie, so a plain <img> works and keeps browser caching.
// Hub mode authenticates with a bearer key an <img> cannot send, so the image
// is fetched and shown from a blob URL. The URL carries a content version, so
// either way a replaced avatar is fetched fresh.
export function AgentAvatar({ name, alias, avatarUrl, size = 'md', className = '' }: Props) {
  // Read the context directly: a list rendered outside a ClusterProvider (tests,
  // previews) still gets initials rather than a crash.
  const cluster = useContext(ClusterContext)
  const label = alias || name
  const src = useAvatarSrc(cluster, avatarUrl)
  const [failed, setFailed] = useState(false)
  useEffect(() => setFailed(false), [src])

  const box = `inline-flex shrink-0 select-none items-center justify-center overflow-hidden rounded-lg ${SIZES[size]} ${className}`
  if (src && !failed) {
    return (
      <img
        src={src}
        alt={`${label} avatar`}
        className={`${box} object-cover`}
        onError={() => setFailed(true)}
        data-testid="agent-avatar-image"
      />
    )
  }
  return (
    <span
      role="img"
      aria-label={`${label} avatar`}
      className={`${box} font-semibold text-white`}
      style={{ backgroundColor: avatarColor(name) }}
      data-testid="agent-avatar-initials"
    >
      {avatarInitials(label)}
    </span>
  )
}

function useAvatarSrc(cluster: Cluster | null, avatarUrl?: string): string | undefined {
  const url = avatarUrl ? `${(cluster?.baseURL ?? '').replace(/\/$/, '')}${avatarUrl}` : undefined
  const bearer = Boolean(cluster?.apiKey)
  const [blob, setBlob] = useState<string>()

  useEffect(() => {
    setBlob(undefined)
    if (!url || !bearer || !cluster) return
    let released = false
    acquireBlob(url, cluster.apiKey)
      .then((objectURL) => { if (!released) setBlob(objectURL) })
      .catch(() => { if (!released) setBlob('') })
    return () => {
      released = true
      releaseBlob(url)
    }
  }, [url, bearer, cluster?.apiKey])

  if (!url) return undefined
  if (!bearer) return url
  // Initials show while the blob loads and if it fails.
  return blob || undefined
}

// Blob URLs are shared by every avatar showing the same versioned URL (a list
// and a detail header, say) and revoked when the last one unmounts.
const blobs = new Map<string, { refs: number; objectURL: Promise<string> }>()

function acquireBlob(url: string, apiKey: string): Promise<string> {
  let entry = blobs.get(url)
  if (!entry) {
    const objectURL = fetch(url, { headers: { Authorization: `Bearer ${apiKey}` } }).then(async (res) => {
      if (!res.ok) throw new Error(`avatar HTTP ${res.status}`)
      return URL.createObjectURL(await res.blob())
    })
    entry = { refs: 0, objectURL }
    blobs.set(url, entry)
    // A failed fetch is not cached: the next mount retries.
    objectURL.catch(() => { if (blobs.get(url) === entry) blobs.delete(url) })
  }
  entry.refs++
  return entry.objectURL
}

function releaseBlob(url: string) {
  const entry = blobs.get(url)
  if (!entry) return
  entry.refs--
  if (entry.refs > 0) return
  blobs.delete(url)
  entry.objectURL.then((u) => URL.revokeObjectURL(u)).catch(() => {})
}

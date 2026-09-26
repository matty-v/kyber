// Agent avatar limits and messages. The server (pkg/api handleAgentAvatar)
// is the source of truth; these checks only spare an operator a round trip.

export const AVATAR_TYPES = ['image/png', 'image/jpeg', 'image/webp'] as const
export const MAX_AVATAR_BYTES = 1 << 20

/** avatarFileProblem returns why a file cannot be an avatar, or null. */
export function avatarFileProblem(file: Blob): string | null {
  if (!(AVATAR_TYPES as readonly string[]).includes(file.type)) {
    return 'Choose a PNG, JPEG, or WebP image.'
  }
  if (file.size > MAX_AVATAR_BYTES) {
    return 'The image must be 1 MiB or smaller.'
  }
  return null
}

/** avatarErrorMessage turns an upload or remove failure into operator copy. */
export function avatarErrorMessage(err: unknown): string {
  const e = err as { status?: number; code?: string; message?: string } | null
  switch (e?.code) {
    case 'avatar_too_large':
      return 'The image must be 1 MiB or smaller.'
    case 'invalid_avatar_type':
      return 'Choose a PNG, JPEG, or WebP image.'
    case 'invalid_avatar':
      return e.message ? `The image could not be used: ${e.message}` : 'The image could not be read. Try another file.'
    case 'avatar_unavailable':
      return 'Avatar storage is not available on this installation.'
  }
  if (e?.status === 413) return 'The image must be 1 MiB or smaller.'
  if (e?.status === 415) return 'Choose a PNG, JPEG, or WebP image.'
  return e?.message || 'Something went wrong. Try again.'
}

const PALETTE = ['#2B6964', '#5E6B2A', '#A5601A', '#6A4C93', '#1F6F8B', '#8C3B5E', '#3F6E3A', '#7A5230']

/** avatarInitials derives up to two initials from an alias or agent name. */
export function avatarInitials(label: string): string {
  const words = label.trim().split(/[\s._-]+/).filter(Boolean)
  if (words.length === 0) return '?'
  if (words.length === 1) return words[0].slice(0, 2).toUpperCase()
  return (words[0][0] + words[1][0]).toUpperCase()
}

/** avatarColor picks a stable background for an agent's initials. */
export function avatarColor(seed: string): string {
  let h = 0
  for (let i = 0; i < seed.length; i++) h = (h * 31 + seed.charCodeAt(i)) >>> 0
  return PALETTE[h % PALETTE.length]
}

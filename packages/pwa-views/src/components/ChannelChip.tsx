import type { ChannelInfo } from '../lib/transcript'

// Map a raw channel source (e.g. "plugin:telegram:telegram") to a friendly name.
function label(source: string): string {
  if (source.includes('telegram')) return 'Telegram'
  const parts = source.split(':')
  return parts[parts.length - 1] || source
}

export function ChannelChip({ channel }: { channel: ChannelInfo }) {
  return (
    <span className="inline-flex items-center gap-1 rounded-full border border-border-subtle bg-surface-sunken px-2 py-0.5 text-xs text-text-muted">
      via {label(channel.source)}
    </span>
  )
}

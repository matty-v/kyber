// Keep persisted diagnostics readable even when older servers include a whole
// file inventory in a single sentence. The complete text remains available.
export function ExpandableText({ text }: { text: string }) {
  const limit = 240
  if (text.length <= limit) return <span className="[overflow-wrap:anywhere]">{text}</span>

  return (
    <details className="min-w-0 [overflow-wrap:anywhere]">
      <summary className="cursor-pointer">
        {text.slice(0, limit).trimEnd()}…{' '}
        <span className="underline">Show full text</span>
      </summary>
      <p className="mt-2 max-h-48 overflow-y-auto whitespace-pre-wrap [overflow-wrap:anywhere]" tabIndex={0}>
        {text}
      </p>
    </details>
  )
}

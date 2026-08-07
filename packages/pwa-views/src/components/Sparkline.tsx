// Hand-rolled SVG sparkline — no chart library per design decision #5
// (docs/design/2026-04-19-pwa-design-system.md). One polyline scaled to the
// component's viewport. Used by the FleetOverview SummaryCards to show
// per-phase count trends across the rolling poll window.

interface SparklineProps {
  // Series points, oldest first. Empty or single-point arrays render
  // empty (no line). Values < 0 are clamped to 0.
  points: readonly number[]
  // Tailwind/CSS color applied to the stroke. Pass a token like
  // 'text-success' so the line picks up theme-mode colours via
  // currentColor.
  className?: string
  // Pixel viewport. Sparkline scales to fit; pick small (~50x14) for
  // inline usage in dense rows. Defaults align with the SummaryCard
  // phase rows (12px high × 48px wide).
  width?: number
  height?: number
  // Stroke width in pixels. Defaults to 1 — anything thicker pixelates
  // at this size.
  strokeWidth?: number
  // Max value to use for scaling. When unset, the line auto-scales to
  // the series max — the more honest "trend" view. Pass a fixed value
  // when you want vertical alignment across multiple sparklines.
  yMax?: number
  // Accessible label read by screen readers. Defaults to "trend" so
  // sparklines never go un-labelled.
  ariaLabel?: string
}

export function Sparkline({
  points,
  className,
  width = 48,
  height = 12,
  strokeWidth = 1,
  yMax,
  ariaLabel = 'trend',
}: SparklineProps) {
  // <2 points → nothing to draw. Render a placeholder dot at mid-line
  // so the row layout stays consistent (no width jitter as the buffer
  // fills) without lying about a trend that isn't there.
  if (points.length < 2) {
    return (
      <svg
        className={className}
        width={width}
        height={height}
        viewBox={`0 0 ${width} ${height}`}
        role="img"
        aria-label={ariaLabel}
      >
        <circle cx={width / 2} cy={height / 2} r={1} fill="currentColor" opacity={0.4} />
      </svg>
    )
  }

  const series = points.map((v) => Math.max(0, v))
  const max = yMax ?? Math.max(...series, 1)
  const min = 0

  // Map (i, value) → (x, y). Y is inverted because SVG's origin is top-
  // left. A 1-pixel stroke needs a 0.5px inset top + bottom so the line
  // doesn't clip at the viewport edge.
  const inset = strokeWidth / 2
  const usableH = height - 2 * inset
  const denom = max - min || 1
  const d = series
    .map((v, i) => {
      const x = (i / (series.length - 1)) * width
      const y = inset + (1 - (v - min) / denom) * usableH
      return `${i === 0 ? 'M' : 'L'} ${x.toFixed(2)} ${y.toFixed(2)}`
    })
    .join(' ')

  return (
    <svg
      className={className}
      width={width}
      height={height}
      viewBox={`0 0 ${width} ${height}`}
      role="img"
      aria-label={ariaLabel}
    >
      <path
        d={d}
        fill="none"
        stroke="currentColor"
        strokeWidth={strokeWidth}
        strokeLinecap="round"
        strokeLinejoin="round"
      />
    </svg>
  )
}

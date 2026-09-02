/**
 * Durations and dates, in the shapes the CLI already prints.
 *
 * Nothing here needs a library: Intl covers the locale-aware half, and the
 * duration format is this project's own - `4m21s`, not `4 minutes 21 seconds`
 * - so a report read in a terminal and a report read in a browser say the same
 * thing.
 */

/** Renders a number of seconds the way the CLI does: 45s, 4m21s, 1h02m. */
export function formatDuration(seconds: number): string {
  if (!Number.isFinite(seconds) || seconds <= 0) return "0s"
  const total = Math.round(seconds)
  if (total < 60) return `${total}s`
  const minutes = Math.floor(total / 60)
  if (minutes < 60) return `${minutes}m${String(total % 60).padStart(2, "0")}s`
  const hours = Math.floor(minutes / 60)
  return `${hours}h${String(minutes % 60).padStart(2, "0")}m`
}

/** Renders an instant as an age: just now, 15m ago, 3h ago, 2d ago. */
export function formatRelative(iso: string, now: Date = new Date()): string {
  const then = new Date(iso).getTime()
  if (Number.isNaN(then)) return "—"
  const seconds = Math.round((now.getTime() - then) / 1000)
  if (seconds < 60) return "just now"
  if (seconds < 3600) return `${Math.floor(seconds / 60)}m ago`
  if (seconds < 86_400) return `${Math.floor(seconds / 3600)}h ago`
  return `${Math.floor(seconds / 86_400)}d ago`
}

/** Renders an instant as a short local date and time. */
export function formatAbsolute(iso: string): string {
  const at = new Date(iso)
  if (Number.isNaN(at.getTime())) return "—"
  return new Intl.DateTimeFormat(undefined, {
    dateStyle: "medium",
    timeStyle: "short",
  }).format(at)
}

/** Seconds since an instant, floored at zero - a clock never runs backwards. */
export function elapsedSeconds(startedAt: string, now: Date = new Date()): number {
  const then = new Date(startedAt).getTime()
  if (Number.isNaN(then)) return 0
  return Math.max(0, Math.round((now.getTime() - then) / 1000))
}

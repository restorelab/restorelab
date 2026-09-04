/**
 * The numbers a drill measured, rendered for a human.
 *
 * Separate from time.ts because these are counts, not durations, and the one
 * rule that matters here has nothing to do with locales: a count must arrive
 * on screen as digits somebody can read at a glance.
 */

/**
 * What this product prints where it has no number. Mirrors report.NoValue.
 *
 * The same glyph the confidence score uses for a workload nobody has ever
 * tested. "We have no idea" and "we know it is zero" are different answers,
 * and a screen that prints 0 for the first one is telling an operator their
 * database is empty.
 */
export const NO_VALUE = "--"

/**
 * U+00A0, between each group of three digits.
 *
 * A no-break space rather than a plain one so a row count never wraps across
 * two lines halfway through, which would read as two numbers. It is not a
 * comma because a comma is a decimal point in half of Europe, and the one
 * thing this screen cannot afford is a reader who is unsure by a factor of a
 * thousand.
 */
const GROUP = "\u00a0"

/**
 * Renders a measured number in full: 1206890 becomes 1 206 890.
 *
 * Never `String(n)` and never a template literal. Both switch to scientific
 * notation past 1e21, so a count would reach the screen as 1.204331e+06 and
 * the reader would have to do arithmetic to find out whether their database
 * is empty, which is the question they came to ask. Intl is used only to
 * expand the number to plain digits - the grouping is done here so that the
 * separator is this product's choice and not the host's locale, which in a
 * browser is whatever the viewer's operating system says.
 *
 * A value that is not a finite number renders as the no-value glyph. Nothing
 * upstream should produce one - internal/checks refuses NaN and Inf at the
 * point of capture, precisely because they compare false against every bound
 * - but a screen is the wrong place to find out.
 */
export function formatCount(value: number): string {
  if (!Number.isFinite(value)) return NO_VALUE

  const plain = value.toLocaleString("en-US", {
    useGrouping: false,
    maximumFractionDigits: 20,
  })

  const negative = plain.startsWith("-")
  const [whole = "", fraction] = (negative ? plain.slice(1) : plain).split(".")
  const grouped = whole.replace(/\B(?=(\d{3})+(?!\d))/g, GROUP)

  return `${negative ? "-" : ""}${grouped}${fraction ? `.${fraction}` : ""}`
}

/** The figure a value is compared against, or the no-value glyph. */
export function formatBaseline(baseline: number | null | undefined): string {
  if (baseline === null || baseline === undefined) return NO_VALUE
  return formatCount(baseline)
}

/**
 * How far a value moved from its baseline, signed: +2 559, -1 204 327.
 *
 * Null when there is nothing to compare against, and null when nothing moved:
 * a "+0" beside two identical numbers is noise, and the two numbers already
 * say it.
 *
 * Deliberately an absolute difference and not a percentage. A percentage from
 * a baseline of zero is a division nobody has a good answer for, and the
 * difference is exact at every scale.
 */
export function formatDelta(
  value: number,
  baseline: number | null | undefined,
): string | null {
  if (baseline === null || baseline === undefined) return null
  const delta = value - baseline
  if (!Number.isFinite(delta) || delta === 0) return null
  return delta > 0 ? `+${formatCount(delta)}` : formatCount(delta)
}

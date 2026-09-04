import { describe, expect, it } from "vitest"
import { NO_VALUE, formatBaseline, formatCount, formatDelta } from "./number"

// The assertions are written with plain spaces because that is what a reader
// types. The separator is a no-break space, so every expectation goes through
// this: the test is about the digits and the grouping, not about which space.
function plain(s: string): string {
  return s.replace(/\s/g, " ")
}

describe("formatCount", () => {
  it("groups a count in threes", () => {
    expect(plain(formatCount(1206890))).toBe("1 206 890")
    expect(plain(formatCount(4821))).toBe("4 821")
    expect(plain(formatCount(821))).toBe("821")
    expect(plain(formatCount(0))).toBe("0")
  })

  it("groups a negative count without stranding the sign", () => {
    expect(plain(formatCount(-1204327))).toBe("-1 204 327")
  })

  it("keeps a fraction, and never groups it", () => {
    expect(plain(formatCount(1.5))).toBe("1.5")
    expect(plain(formatCount(1234.56789))).toBe("1 234.56789")
  })

  // The whole reason this function exists rather than a template literal: a
  // count rendered as 1.204331e+06 makes the reader do arithmetic to find out
  // whether their database is empty, which is the question they came to ask.
  it("never falls back to scientific notation, at either end of the scale", () => {
    expect(formatCount(1e21)).not.toMatch(/e[+-]/i)
    expect(plain(formatCount(1e21))).toBe("1 000 000 000 000 000 000 000")
    expect(formatCount(0.0000001)).not.toMatch(/e[+-]/i)
    expect(formatCount(0.0000001)).toBe("0.0000001")
  })

  // Nothing upstream should produce one - internal/checks refuses NaN and Inf
  // at the point of capture - but a screen is the wrong place to find out.
  it("renders what is not a number as the no-value glyph", () => {
    expect(formatCount(Number.NaN)).toBe(NO_VALUE)
    expect(formatCount(Number.POSITIVE_INFINITY)).toBe(NO_VALUE)
  })
})

describe("formatBaseline", () => {
  it("renders a figure when there is one", () => {
    expect(plain(formatBaseline(1204331))).toBe("1 204 331")
  })

  // "no previous drill measured this" and "the previous drill measured zero"
  // are opposite pieces of news.
  it("renders no history as the glyph, and a zero as a zero", () => {
    expect(formatBaseline(null)).toBe(NO_VALUE)
    expect(formatBaseline(undefined)).toBe(NO_VALUE)
    expect(formatBaseline(0)).toBe("0")
  })
})

describe("formatDelta", () => {
  it("signs a rise and a fall", () => {
    expect(plain(formatDelta(1206890, 1204331) ?? "")).toBe("+2 559")
    expect(plain(formatDelta(4, 1204331) ?? "")).toBe("-1 204 327")
  })

  it("says nothing when there is nothing to compare against", () => {
    expect(formatDelta(4821, null)).toBeNull()
    expect(formatDelta(4821, undefined)).toBeNull()
  })

  // Two identical numbers already say it; "+0" beside them is noise.
  it("says nothing when nothing moved", () => {
    expect(formatDelta(4821, 4821)).toBeNull()
  })

  // A baseline of zero is where a percentage would divide by nothing. The
  // difference is exact at every scale, including this one.
  it("compares against a baseline of zero without dividing", () => {
    expect(plain(formatDelta(1206890, 0) ?? "")).toBe("+1 206 890")
    expect(formatDelta(0, 0)).toBeNull()
  })
})

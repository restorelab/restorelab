import { describe, expect, it } from "vitest"
import { elapsedSeconds, formatDuration, formatRelative } from "./time"

describe("formatDuration", () => {
  const cases: [number, string][] = [
    [0, "0s"],
    [0.4, "0s"],
    [45, "45s"],
    // Rounding carries into the minutes branch, and that is the right answer:
    // "1m00s" reads as a duration, "60s" reads as a stopwatch that forgot to
    // roll over.
    [59.6, "1m00s"],
    [60, "1m00s"],
    [261, "4m21s"],
    [3600, "1h00m"],
    [3720, "1h02m"],
    [86_400, "24h00m"],
  ]
  for (const [seconds, want] of cases) {
    it(`renders ${seconds}s as ${want}`, () => {
      expect(formatDuration(seconds)).toBe(want)
    })
  }

  it("refuses to render a negative duration as a clock", () => {
    expect(formatDuration(-5)).toBe("0s")
  })
})

describe("formatRelative", () => {
  const now = new Date("2026-09-02T12:00:00Z")
  const cases: [string, string][] = [
    ["2026-09-02T11:59:30Z", "just now"],
    ["2026-09-02T11:45:00Z", "15m ago"],
    ["2026-09-02T09:00:00Z", "3h ago"],
    ["2026-08-31T12:00:00Z", "2d ago"],
  ]
  for (const [iso, want] of cases) {
    it(`renders ${iso} as ${want}`, () => {
      expect(formatRelative(iso, now)).toBe(want)
    })
  }

  it("renders an unreadable instant as an em dash rather than Invalid Date", () => {
    expect(formatRelative("not a date", now)).toBe("—")
  })
})

describe("elapsedSeconds", () => {
  it("counts from the start to now", () => {
    const now = new Date("2026-09-02T12:00:00Z")
    expect(elapsedSeconds("2026-09-02T11:58:00Z", now)).toBe(120)
  })

  it("never runs backwards, even if the clocks disagree", () => {
    const now = new Date("2026-09-02T12:00:00Z")
    expect(elapsedSeconds("2026-09-02T12:05:00Z", now)).toBe(0)
  })
})

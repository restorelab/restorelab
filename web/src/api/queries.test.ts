import { describe, expect, it } from "vitest"
import { first, runsPageFixture } from "./fixtures"
import { FAST_MS, SLOW_MS, anyRunActive, runQuery, runsQuery } from "./queries"
import type { Page, RunDocument, RunSummary } from "./types"

// The shape comes from the wire: runsPageFixture is the captured body of
// GET /recovery-runs. Only the state under test is set here.
const capturedRun = first(runsPageFixture.items, "runs page")

function run(state: string): RunSummary {
  return { ...capturedRun, state: state as RunSummary["state"] }
}

describe("anyRunActive", () => {
  it("is true while a run is in a non-terminal state", () => {
    expect(anyRunActive({ items: [run("RESTORING")] })).toBe(true)
  })

  it("is false once every run has reached a terminal state", () => {
    expect(anyRunActive({ items: [run("SUCCESS"), run("FAILED")] })).toBe(false)
  })

  it("is false for an empty page", () => {
    expect(anyRunActive({ items: [] })).toBe(false)
  })

  it("is false when nothing has loaded yet, so a first paint does not poll fast", () => {
    expect(anyRunActive(undefined)).toBe(false)
  })
})

describe("runsQuery", () => {
  it("puts every filter in the key, so two filtered views do not share a cache entry", () => {
    const a = runsQuery({ state: "FAILED" }).queryKey
    const b = runsQuery({ state: "SUCCESS" }).queryKey
    expect(a).not.toEqual(b)
  })

  it("polls fast while something runs and slow when nothing does", () => {
    const interval = runsQuery({}).refetchInterval as (arg: {
      state: { data?: Page<RunSummary> }
    }) => number
    expect(interval({ state: { data: { items: [run("RESTORING")] } } })).toBe(FAST_MS)
    expect(interval({ state: { data: { items: [run("SUCCESS")] } } })).toBe(SLOW_MS)
  })

  it("omits an unset filter from the URL rather than sending an empty value", () => {
    expect(runsQuery({}).queryKey).toEqual(["runs", {}])
  })
})

describe("runQuery", () => {
  it("stops polling a finished run entirely", () => {
    const interval = runQuery("r1").refetchInterval as (arg: {
      state: { data?: RunDocument }
    }) => number | false
    const doc = (state: string) => ({ state }) as RunDocument
    expect(interval({ state: { data: doc("SUCCESS") } })).toBe(false)
    expect(interval({ state: { data: doc("RESTORING") } })).toBe(FAST_MS)
  })
})

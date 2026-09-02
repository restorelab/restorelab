import { first, providersFixture, workloadsPageFixture } from "@/api/fixtures"
import { describe, expect, it } from "vitest"
import { defaultProviderID, planSkeleton } from "./plan-skeleton"

const workload = first(workloadsPageFixture.items, "workloads")
const providerID = defaultProviderID(providersFixture.items) ?? "pve"

describe("defaultProviderID", () => {
  it("prefers the one marked default", () => {
    expect(
      defaultProviderID([
        { ...first(providersFixture.items, "providers"), id: "other", default: false },
        { ...first(providersFixture.items, "providers"), id: "main", default: true },
      ]),
    ).toBe("main")
  })

  it("falls back to the first when none is marked", () => {
    expect(
      defaultProviderID([
        { ...first(providersFixture.items, "providers"), id: "only", default: false },
      ]),
    ).toBe("only")
  })

  it("has no answer when nothing is configured", () => {
    expect(defaultProviderID([])).toBeUndefined()
  })
})

describe("planSkeleton", () => {
  it("names the machine it was built for", () => {
    const doc = planSkeleton(workload, providerID)
    expect(doc).toContain(`id: "${workload.id}"`)
    expect(doc).toContain(workload.name)
  })

  // A plan must name its provider. An ad-hoc drill may leave it out and let
  // the server fall back to its configured default; a plan may not, and only
  // the real validator says so. The skeleton without it was refused with
  // "workload.provider is required".
  it("names the provider, which a plan cannot leave out", () => {
    expect(planSkeleton(workload, providerID)).toContain(`provider: ${providerID}`)
  })

  // A skeleton with no comments would teach the format as though comments
  // were not part of it - and they are: the catalogue stores a document
  // verbatim precisely so a human's explanation survives being edited here.
  it("carries the comments that explain it", () => {
    expect(planSkeleton(workload, providerID).startsWith("#")).toBe(true)
  })

  it("has the sections a plan is made of", () => {
    const doc = planSkeleton(workload, providerID)
    for (const key of ["name:", "workload:", "backup:", "checks:", "cleanup:"]) {
      expect(doc).toContain(key)
    }
  })

  // A field the workload does not carry must not become the literal word
  // "undefined" in a document somebody is about to save.
  it("never writes undefined into the document", () => {
    expect(planSkeleton(workload, providerID)).not.toContain("undefined")
    expect(planSkeleton({ ...workload, node: undefined }, providerID)).not.toContain(
      "undefined",
    )
  })

  // The check the editor runs on the very first keystroke has to have
  // something valid to run on: an empty or half-written skeleton would make
  // the panel open on a refusal.
  it("is a document, not a fragment", () => {
    const doc = planSkeleton(workload, providerID)
    expect(doc.trim().length).toBeGreaterThan(200)
    expect(doc.endsWith("\n")).toBe(true)
  })
})

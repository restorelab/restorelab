import { confidenceFixture, first, workloadsPageFixture } from "@/api/fixtures"
import type { Confidence, Page, Workload } from "@/api/types"
import { render, screen } from "@testing-library/react"
import { describe, expect, it } from "vitest"
import { WorkloadsContent } from "./index"

// The shape comes from the wire, not from this file: workloadsPageFixture is
// the captured body of GET /workloads. Only the identity a given assertion
// reads is overridden, because which machine it is has nothing to do with
// what these tests check.
const capturedWorkload = first(workloadsPageFixture.items, "workloads")

function workload(over: Partial<Workload> = {}): Workload {
  return { ...capturedWorkload, id: "101", name: "web-01", ...over }
}

const tested: Confidence = { ...confidenceFixture, workload_id: "101", score: 82 }
const untested: Confidence = {
  ...confidenceFixture,
  workload_id: "202",
  score: null,
  tested: false,
  reasons: ["never drilled"],
  last_run_id: undefined,
  runs_considered: 0,
}

describe("WorkloadsContent", () => {
  it("renders a row per workload", () => {
    const workloads: Page<Workload> = {
      items: [workload(), workload({ id: "202", name: "db-02" })],
    }
    render(
      <WorkloadsContent
        workloads={workloads}
        confidences={
          new Map([
            ["101", tested],
            ["202", untested],
          ])
        }
      />,
    )
    expect(screen.getByText("web-01")).toBeInTheDocument()
    expect(screen.getByText("db-02")).toBeInTheDocument()
  })

  it("shows a score for a tested workload and an em dash for an untested one", () => {
    const workloads: Page<Workload> = {
      items: [workload(), workload({ id: "202", name: "db-02" })],
    }
    render(
      <WorkloadsContent
        workloads={workloads}
        confidences={
          new Map([
            ["101", tested],
            ["202", untested],
          ])
        }
      />,
    )
    expect(screen.getByText("82")).toBeInTheDocument()
    expect(screen.getByText("—")).toBeInTheDocument()
  })

  it("does not invent a score while one is still loading", () => {
    render(
      <WorkloadsContent workloads={{ items: [workload()] }} confidences={new Map()} />,
    )
    expect(screen.queryByText("0")).toBeNull()
  })

  it("explains an empty inventory rather than showing an empty table", () => {
    render(<WorkloadsContent workloads={{ items: [] }} confidences={new Map()} />)
    expect(screen.getByText(/no workloads/i)).toBeInTheDocument()
    expect(screen.getByText("restorelab connect")).toBeInTheDocument()
  })
})

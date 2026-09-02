import type { Confidence, Page, Workload } from "@/api/types"
import { render, screen } from "@testing-library/react"
import { describe, expect, it } from "vitest"
import { WorkloadsContent } from "./index"

function workload(over: Partial<Workload> = {}): Workload {
  return {
    id: "101",
    name: "web-01",
    kind: "qemu",
    node: "pve1",
    cpu_cores: 2,
    memory_bytes: 4 * 1024 ** 3,
    disk_bytes: 32 * 1024 ** 3,
    power_state: "running",
    template: false,
    managed: false,
    ...over,
  }
}

const tested: Confidence = {
  workload_id: "101",
  score: 82,
  tested: true,
  reasons: [],
  runs_considered: 3,
}
const untested: Confidence = {
  workload_id: "202",
  score: null,
  tested: false,
  reasons: ["never drilled"],
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

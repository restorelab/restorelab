import type { Doctor, Page, QueueEntry, RunSummary, Workload } from "@/api/types"
import { QueryClient, QueryClientProvider } from "@tanstack/react-query"
import { render, screen } from "@testing-library/react"
import { describe, expect, it } from "vitest"
import { OverviewContent } from "./index"

const doctor: Doctor = { provider_id: "pve", ok: true, problems: 0, findings: [] }
const noRuns: Page<RunSummary> = { items: [] }
const noQueue: Page<QueueEntry> = { items: [] }
const workloads: Page<Workload> = {
  items: [
    {
      id: "101",
      name: "web-01",
      kind: "qemu",
      cpu_cores: 2,
      memory_bytes: 0,
      disk_bytes: 0,
      power_state: "running",
      template: false,
      managed: false,
    },
  ],
}

function wrap(ui: React.ReactNode) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return <QueryClientProvider client={qc}>{ui}</QueryClientProvider>
}

describe("OverviewContent", () => {
  it("teaches the next step when no drill has ever run", () => {
    render(
      wrap(
        <OverviewContent
          doctor={doctor}
          queue={noQueue}
          runs={noRuns}
          workloads={workloads}
        />,
      ),
    )
    expect(screen.getByText(/no drill has run yet/i)).toBeInTheDocument()
    expect(screen.getByText("restorelab drill --workload 101")).toBeInTheDocument()
  })

  it("keeps the health strip in the empty case, because it has something to say", () => {
    render(
      wrap(
        <OverviewContent
          doctor={doctor}
          queue={noQueue}
          runs={noRuns}
          workloads={workloads}
        />,
      ),
    )
    expect(screen.getByText(/1 workload/i)).toBeInTheDocument()
  })

  it("lists a running drill with its elapsed time", () => {
    const queue: Page<QueueEntry> = {
      items: [
        {
          id: "r1",
          plan_name: "nightly",
          source_workload_id: "101",
          source_name: "web-01",
          state: "RESTORING",
          started_at: new Date(Date.now() - 134_000).toISOString(),
          completed_at: null,
          rto_seconds: 0,
          rto: "0s",
          rto_exceeded: false,
          cleanup_done: false,
        },
      ],
    }
    render(
      wrap(
        <OverviewContent
          doctor={doctor}
          queue={queue}
          runs={noRuns}
          workloads={workloads}
        />,
      ),
    )
    expect(screen.getByText("web-01")).toBeInTheDocument()
    expect(screen.getByText("Restoring")).toBeInTheDocument()
    expect(screen.getByText("2m14s")).toBeInTheDocument()
  })

  it("does not show the day-one empty state once a drill exists", () => {
    const runs: Page<RunSummary> = {
      items: [
        {
          id: "r0",
          plan_name: "nightly",
          source_workload_id: "101",
          source_name: "web-01",
          state: "SUCCESS",
          result: "SUCCESS",
          started_at: "2026-09-01T03:12:00Z",
          completed_at: "2026-09-01T03:16:21Z",
          rto_seconds: 261,
          rto: "4m21s",
          rto_exceeded: false,
          cleanup_done: true,
        },
      ],
    }
    render(
      wrap(
        <OverviewContent
          doctor={doctor}
          queue={noQueue}
          runs={runs}
          workloads={workloads}
        />,
      ),
    )
    expect(screen.queryByText(/no drill has run yet/i)).toBeNull()
    expect(screen.getByText("4m21s")).toBeInTheDocument()
  })

  it("says the cluster has problems when the diagnostic does", () => {
    const sick: Doctor = {
      provider_id: "pve",
      ok: false,
      problems: 2,
      findings: [{ level: "error", area: "network", title: "no isolated bridge" }],
    }
    render(
      wrap(
        <OverviewContent
          doctor={sick}
          queue={noQueue}
          runs={noRuns}
          workloads={workloads}
        />,
      ),
    )
    expect(screen.getByText(/2 problems/i)).toBeInTheDocument()
  })
})

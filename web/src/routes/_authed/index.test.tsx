import {
  doctorFixture,
  first,
  queueFixture,
  runsPageFixture,
  workloadsPageFixture,
} from "@/api/fixtures"
import type { Doctor, Page, QueueEntry, RunSummary, Workload } from "@/api/types"
import { QueryClient, QueryClientProvider } from "@tanstack/react-query"
import { render, screen } from "@testing-library/react"
import { describe, expect, it } from "vitest"
import { OverviewContent } from "./index"

// Every payload below starts from a captured response. Only the identity an
// assertion reads is overridden - which machine it is has nothing to do with
// what these tests check.
const doctor: Doctor = { ...doctorFixture, ok: true, problems: 0, findings: [] }
const noop = () => undefined
const orphans = workloadsPageFixture.items.filter((w) => w.managed)
const noRuns: Page<RunSummary> = { items: [] }
const noQueue: Page<QueueEntry> = { items: [] }
const workloads: Page<Workload> = {
  items: [
    {
      ...first(workloadsPageFixture.items, "workloads"),
      id: "101",
      name: "web-01",
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
          orphans={[]}
          canOperate
          onStarted={noop}
        />,
      ),
    )
    expect(screen.getByText(/no drill has run yet/i)).toBeInTheDocument()
    // The button is the point of C3: day one ends on an action, not on a line
    // to copy into a terminal somewhere else.
    expect(screen.getByRole("button", { name: /run a drill/i })).toBeInTheDocument()
    // And the command stays, because someone who drives this from a terminal
    // must not lose the line they were going to copy.
    expect(screen.getByText("restorelab drill --workload 101")).toBeInTheDocument()
  })

  // A read-only session gets the explanation and the command, and no button.
  it("offers no button to a session that cannot operate", () => {
    render(
      wrap(
        <OverviewContent
          doctor={doctor}
          queue={noQueue}
          runs={noRuns}
          workloads={workloads}
          orphans={[]}
          canOperate={false}
          onStarted={noop}
        />,
      ),
    )
    expect(screen.queryByRole("button", { name: /run a drill/i })).toBeNull()
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
          orphans={[]}
          canOperate
          onStarted={noop}
        />,
      ),
    )
    expect(screen.getByText(/1 workload/i)).toBeInTheDocument()
  })

  it("lists a running drill with its elapsed time", () => {
    const queue: Page<QueueEntry> = {
      items: [
        {
          ...first(queueFixture.items, "queue"),
          source_name: "web-01",
          state: "RESTORING",
          // Relative on purpose: the assertion below is about the elapsed
          // time the row computes, which only means something against now.
          started_at: new Date(Date.now() - 134_000).toISOString(),
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
          orphans={[]}
          canOperate
          onStarted={noop}
        />,
      ),
    )
    expect(screen.getByText("web-01")).toBeInTheDocument()
    expect(screen.getByText("Restoring")).toBeInTheDocument()
    expect(screen.getByText("2m14s")).toBeInTheDocument()
  })

  it("does not show the day-one empty state once a drill exists", () => {
    const captured =
      runsPageFixture.items.find((r) => r.state === "SUCCESS") ??
      first(runsPageFixture.items, "runs page")
    const runs: Page<RunSummary> = {
      items: [{ ...captured, source_name: "web-01", rto: "4m21s" }],
    }
    render(
      wrap(
        <OverviewContent
          doctor={doctor}
          queue={noQueue}
          runs={runs}
          workloads={workloads}
          orphans={[]}
          canOperate
          onStarted={noop}
        />,
      ),
    )
    expect(screen.queryByText(/no drill has run yet/i)).toBeNull()
    expect(screen.getByText("4m21s")).toBeInTheDocument()
  })

  // A machine a drill forgot on the cluster is an anomaly, and the overview
  // is the screen for anomalies. It appears there, above everything else.
  it("reports the temporary workloads a drill left behind", () => {
    render(
      wrap(
        <OverviewContent
          doctor={doctor}
          queue={noQueue}
          runs={noRuns}
          workloads={workloads}
          orphans={orphans}
          canOperate
          onStarted={noop}
        />,
      ),
    )
    expect(screen.getByText(/temporary workload/i)).toBeInTheDocument()
  })

  it("says nothing about orphans when the cluster is clean", () => {
    render(
      wrap(
        <OverviewContent
          doctor={doctor}
          queue={noQueue}
          runs={noRuns}
          workloads={workloads}
          orphans={[]}
          canOperate
          onStarted={noop}
        />,
      ),
    )
    expect(screen.queryByText(/temporary workload/i)).toBeNull()
  })

  // The counter C2 had to drop for want of a cheap way to compute it.
  it("counts the machines that have never been drilled", () => {
    render(
      wrap(
        <OverviewContent
          doctor={doctor}
          queue={noQueue}
          runs={noRuns}
          workloads={{
            items: [{ ...first(workloads.items, "workloads"), last_run_id: undefined }],
          }}
          orphans={[]}
          canOperate
          onStarted={noop}
        />,
      ),
    )
    expect(screen.getByText(/1 never tested/i)).toBeInTheDocument()
  })

  it("says the cluster has problems when the diagnostic does", () => {
    const sick: Doctor = {
      ...doctorFixture,
      ok: false,
      problems: 2,
      findings: [{ level: "fail", area: "network", title: "no isolated bridge" }],
    }
    render(
      wrap(
        <OverviewContent
          doctor={sick}
          queue={noQueue}
          runs={noRuns}
          workloads={workloads}
          orphans={[]}
          canOperate
          onStarted={noop}
        />,
      ),
    )
    expect(screen.getByText(/2 problems/i)).toBeInTheDocument()
  })
})

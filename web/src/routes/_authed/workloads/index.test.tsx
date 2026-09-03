import { confidenceFixture, first, workloadsPageFixture } from "@/api/fixtures"
import type { Confidence, Page, Workload } from "@/api/types"
import { QueryClient, QueryClientProvider } from "@tanstack/react-query"
import { render, screen } from "@testing-library/react"
import type { ReactNode } from "react"
import { describe, expect, it } from "vitest"
import { WorkloadsContent } from "./index"

// TriggerDrill lives in a cell of this table and holds a mutation, so the
// table needs a client even in the tests that never press its button.
function wrap(ui: ReactNode) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return <QueryClientProvider client={qc}>{ui}</QueryClientProvider>
}

const noop = () => undefined
const noActiveRuns = new Map<string, string>()

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
      wrap(
        <WorkloadsContent
          workloads={workloads}
          confidences={
            new Map([
              ["101", tested],
              ["202", untested],
            ])
          }
          canOperate
          activeRuns={noActiveRuns}
          onStarted={noop}
        />,
      ),
    )
    expect(screen.getByText("web-01")).toBeInTheDocument()
    expect(screen.getByText("db-02")).toBeInTheDocument()
  })

  it("shows a score for a tested workload and an em dash for an untested one", () => {
    const workloads: Page<Workload> = {
      items: [workload(), workload({ id: "202", name: "db-02" })],
    }
    render(
      wrap(
        <WorkloadsContent
          workloads={workloads}
          confidences={
            new Map([
              ["101", tested],
              ["202", untested],
            ])
          }
          canOperate
          activeRuns={noActiveRuns}
          onStarted={noop}
        />,
      ),
    )
    expect(screen.getByText("82")).toBeInTheDocument()
    expect(screen.getByText("—")).toBeInTheDocument()
  })

  it("does not invent a score while one is still loading", () => {
    render(
      wrap(
        <WorkloadsContent
          workloads={{ items: [workload()] }}
          confidences={new Map()}
          canOperate
          activeRuns={noActiveRuns}
          onStarted={noop}
        />,
      ),
    )
    expect(screen.queryByText("0")).toBeNull()
  })

  /**
   * A score with no idea what was proven is the mystery this badge ends: 60
   * beside "Boot only" is the row that gets somebody to write a real check.
   */
  it("says what the last drill proved, beside the score", () => {
    const workloads: Page<Workload> = {
      items: [workload({ last_run_proof: "BOOT", last_run_state: "SUCCESS" })],
    }
    render(
      wrap(
        <WorkloadsContent
          workloads={workloads}
          confidences={new Map([["101", tested]])}
          canOperate
          activeRuns={noActiveRuns}
          onStarted={noop}
        />,
      ),
    )
    expect(screen.getByText("Boot only")).toBeInTheDocument()
  })

  // A machine nobody has drilled has proven nothing and had nothing asked of
  // it. Showing a level there would be a claim; showing none is the truth.
  it("claims no level for a machine that was never drilled", () => {
    const workloads: Page<Workload> = {
      items: [
        workload({
          id: "202",
          name: "db-02",
          last_run_proof: undefined,
          last_run_state: undefined,
        }),
      ],
    }
    render(
      wrap(
        <WorkloadsContent
          workloads={workloads}
          confidences={new Map([["202", untested]])}
          canOperate
          activeRuns={noActiveRuns}
          onStarted={noop}
        />,
      ),
    )
    expect(screen.queryByText(/nothing proven/i)).toBeNull()
    expect(screen.queryByText(/boot only/i)).toBeNull()
  })

  /**
   * A drill in flight carries NONE, honestly - it has established nothing
   * yet. Painting the row "nothing proven" would deliver a verdict on a drill
   * that has not reached one.
   */
  it("waits for the drill to finish before saying what it proved", () => {
    const workloads: Page<Workload> = {
      items: [workload({ last_run_proof: "NONE", last_run_state: "RESTORING" })],
    }
    render(
      wrap(
        <WorkloadsContent
          workloads={workloads}
          confidences={new Map([["101", tested]])}
          canOperate
          activeRuns={noActiveRuns}
          onStarted={noop}
        />,
      ),
    )
    expect(screen.queryByText(/nothing proven/i)).toBeNull()
  })

  it("offers a drill on every row", () => {
    const workloads: Page<Workload> = { items: [workload()] }
    render(
      wrap(
        <WorkloadsContent
          workloads={workloads}
          confidences={new Map()}
          canOperate
          activeRuns={noActiveRuns}
          onStarted={noop}
        />,
      ),
    )
    expect(screen.getByRole("button", { name: /run a drill/i })).toBeInTheDocument()
  })

  // A read-only session sees no button at all, not a dead one.
  it("offers nothing to a session that cannot operate", () => {
    const workloads: Page<Workload> = { items: [workload()] }
    render(
      wrap(
        <WorkloadsContent
          workloads={workloads}
          confidences={new Map()}
          canOperate={false}
          activeRuns={noActiveRuns}
          onStarted={noop}
        />,
      ),
    )
    expect(screen.queryByRole("button", { name: /run a drill/i })).toBeNull()
  })

  // A machine already being drilled points at that drill rather than offering
  // a second one the backend would refuse with a 409.
  it("points at the drill already running on a row", () => {
    const workloads: Page<Workload> = { items: [workload()] }
    render(
      wrap(
        <WorkloadsContent
          workloads={workloads}
          confidences={new Map()}
          canOperate
          activeRuns={new Map([["101", "run-in-flight"]])}
          onStarted={noop}
        />,
      ),
    )
    expect(screen.queryByRole("button", { name: /run a drill/i })).toBeNull()
    expect(screen.getByText(/already running/i)).toBeInTheDocument()
  })

  it("explains an empty inventory rather than showing an empty table", () => {
    render(
      wrap(
        <WorkloadsContent
          workloads={{ items: [] }}
          confidences={new Map()}
          canOperate
          activeRuns={noActiveRuns}
          onStarted={noop}
        />,
      ),
    )
    expect(screen.getByText(/no workloads/i)).toBeInTheDocument()
    expect(screen.getByText("restorelab connect")).toBeInTheDocument()
  })
})

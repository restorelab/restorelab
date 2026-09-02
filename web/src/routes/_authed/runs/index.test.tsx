import type { Page, RunSummary } from "@/api/types"
import { QueryClient, QueryClientProvider } from "@tanstack/react-query"
import { render, screen } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { describe, expect, it, vi } from "vitest"
import { RunsContent } from "./index"

function run(over: Partial<RunSummary> = {}): RunSummary {
  return {
    id: "r1",
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
    ...over,
  }
}

function wrap(ui: React.ReactNode) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return <QueryClientProvider client={qc}>{ui}</QueryClientProvider>
}

const noop = () => undefined

describe("RunsContent", () => {
  it("renders a row per drill", () => {
    const page: Page<RunSummary> = {
      items: [run(), run({ id: "r2", source_name: "db-02" })],
    }
    render(wrap(<RunsContent page={page} filter={{}} onFilter={noop} onPage={noop} />))
    expect(screen.getByText("web-01")).toBeInTheDocument()
    expect(screen.getByText("db-02")).toBeInTheDocument()
  })

  it("falls back to the workload id when the name is unknown", () => {
    const page: Page<RunSummary> = { items: [run({ source_name: undefined })] }
    render(wrap(<RunsContent page={page} filter={{}} onFilter={noop} onPage={noop} />))
    expect(screen.getByText("101")).toBeInTheDocument()
  })

  it("marks a breached RTO", () => {
    const page: Page<RunSummary> = {
      items: [run({ rto_exceeded: true, rto: "12m04s" })],
    }
    render(wrap(<RunsContent page={page} filter={{}} onFilter={noop} onPage={noop} />))
    expect(screen.getByText("12m04s")).toHaveClass("text-state-failed")
  })

  it("offers the next page only when the API gave a cursor", () => {
    const { rerender } = render(
      wrap(
        <RunsContent
          page={{ items: [run()] }}
          filter={{}}
          onFilter={noop}
          onPage={noop}
        />,
      ),
    )
    expect(screen.queryByRole("button", { name: /next/i })).toBeNull()

    rerender(
      wrap(
        <RunsContent
          page={{ items: [run()], next_cursor: "abc" }}
          filter={{}}
          onFilter={noop}
          onPage={noop}
        />,
      ),
    )
    expect(screen.getByRole("button", { name: /next/i })).toBeInTheDocument()
  })

  it("hands the cursor back when the next page is asked for", async () => {
    const onPage = vi.fn()
    render(
      wrap(
        <RunsContent
          page={{ items: [run()], next_cursor: "abc" }}
          filter={{}}
          onFilter={noop}
          onPage={onPage}
        />,
      ),
    )
    await userEvent.click(screen.getByRole("button", { name: /next/i }))
    expect(onPage).toHaveBeenCalledWith("abc")
  })

  it("says so when a filter matches nothing, which is not the same as having no drills", () => {
    render(
      wrap(
        <RunsContent
          page={{ items: [] }}
          filter={{ state: "FAILED" }}
          onFilter={noop}
          onPage={noop}
        />,
      ),
    )
    expect(screen.getByText(/no drill matches/i)).toBeInTheDocument()
  })
})

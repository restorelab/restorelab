import {
  cancel200Fixture,
  cancel202Fixture,
  runFinishedFixture,
  runRunningFixture,
} from "@/api/fixtures"
import { QueryClient, QueryClientProvider } from "@tanstack/react-query"
import { render, screen, waitFor } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import type { ReactNode } from "react"
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"
import { CancelRun } from "./cancel-run"

function wrap(ui: ReactNode) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return <QueryClientProvider client={qc}>{ui}</QueryClientProvider>
}

function json(status: number, body: unknown) {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "content-type": "application/json" },
  })
}

async function openAndConfirm() {
  await userEvent.click(screen.getByRole("button", { name: /cancel drill/i }))
  await userEvent.click(screen.getByRole("button", { name: /stop it/i }))
}

describe("CancelRun", () => {
  beforeEach(() => vi.stubGlobal("fetch", vi.fn()))
  afterEach(() => vi.unstubAllGlobals())

  // The dialog names the machine it is about to destroy. It can, because
  // GET /recovery-runs/{id} answers with the report document, which carries
  // temp_workload_id and node - the listing, built from store.RunSummary,
  // does not, which is why this button lives only here.
  it("names the temporary workload it will destroy", async () => {
    render(wrap(<CancelRun run={runRunningFixture} canOperate />))
    await userEvent.click(screen.getByRole("button", { name: /cancel drill/i }))

    expect(
      screen.getByText(new RegExp(String(runRunningFixture.temp_workload_id))),
    ).toBeInTheDocument()
    expect(
      screen.getByText(new RegExp(String(runRunningFixture.node))),
    ).toBeInTheDocument()
  })

  // 200 and 202 are different states of the world. Saying "it is over" on a
  // 202 would announce a machine gone while it still exists.
  it("says the drill is still being torn down on a 202", async () => {
    vi.mocked(fetch).mockResolvedValue(json(202, cancel202Fixture))
    render(wrap(<CancelRun run={runRunningFixture} canOperate />))

    await openAndConfirm()

    expect(await screen.findByText(/still tearing/i)).toBeInTheDocument()
    expect(screen.queryByText(/the drill is over/i)).toBeNull()
  })

  it("says the drill is over on a 200", async () => {
    vi.mocked(fetch).mockResolvedValue(json(200, cancel200Fixture))
    render(wrap(<CancelRun run={runRunningFixture} canOperate />))

    await openAndConfirm()

    expect(await screen.findByText(/the drill is over/i)).toBeInTheDocument()
    expect(screen.queryByText(/still tearing/i)).toBeNull()
  })

  // A run with no temporary workload yet has created nothing on the cluster,
  // and the dialog must not name a machine that does not exist.
  it("says nothing was created when there is no temporary workload yet", async () => {
    const queued = {
      ...runRunningFixture,
      temp_workload_id: undefined,
      node: undefined,
    }
    render(wrap(<CancelRun run={queued} canOperate />))
    await userEvent.click(screen.getByRole("button", { name: /cancel drill/i }))

    expect(screen.getByText(/nothing has been created/i)).toBeInTheDocument()
  })

  it("posts to the run's cancel route", async () => {
    vi.mocked(fetch).mockResolvedValue(json(202, cancel202Fixture))
    render(wrap(<CancelRun run={runRunningFixture} canOperate />))

    await openAndConfirm()

    await waitFor(() => expect(fetch).toHaveBeenCalled())
    const [url, init] = vi.mocked(fetch).mock.calls[0] ?? []
    expect(url).toBe(`/api/v1/recovery-runs/${runRunningFixture.run_id}/cancel`)
    expect((init as RequestInit).method).toBe("POST")
  })

  it("is absent on a finished drill", () => {
    const { container } = render(
      wrap(<CancelRun run={runFinishedFixture} canOperate />),
    )
    expect(container).toBeEmptyDOMElement()
  })

  it("is absent without the operate scope", () => {
    const { container } = render(
      wrap(<CancelRun run={runRunningFixture} canOperate={false} />),
    )
    expect(container).toBeEmptyDOMElement()
  })

  // The backend's refusal is the useful message; it must land in the dialog
  // rather than closing it.
  it("shows a refusal without closing", async () => {
    vi.mocked(fetch).mockResolvedValue(
      new Response(
        JSON.stringify({
          type: "conflict",
          title: "This drill has already finished",
          status: 409,
        }),
        { status: 409, headers: { "content-type": "application/problem+json" } },
      ),
    )
    render(wrap(<CancelRun run={runRunningFixture} canOperate />))

    await openAndConfirm()

    expect(await screen.findByText(/already finished/i)).toBeInTheDocument()
    expect(screen.getByRole("button", { name: /stop it/i })).toBeInTheDocument()
  })
})

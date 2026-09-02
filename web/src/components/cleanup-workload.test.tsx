import { cleanupFixture, workloadsPageFixture } from "@/api/fixtures"
import type { Workload } from "@/api/types"
import { QueryClient, QueryClientProvider } from "@tanstack/react-query"
import { render, screen, waitFor } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import type { ReactNode } from "react"
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"
import { CleanupWorkload } from "./cleanup-workload"

// The capture carries a managed workload on purpose: this component only ever
// renders for one, and a fixture without one would let it be written blind.
const orphan = workloadsPageFixture.items.find((w) => w.managed) as Workload

function wrap(ui: ReactNode) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return <QueryClientProvider client={qc}>{ui}</QueryClientProvider>
}

async function openAndConfirm() {
  await userEvent.click(screen.getByRole("button", { name: /^destroy$/i }))
  const buttons = screen.getAllByRole("button", { name: /^destroy$/i })
  await userEvent.click(buttons[buttons.length - 1] as HTMLElement)
}

describe("CleanupWorkload", () => {
  beforeEach(() => vi.stubGlobal("fetch", vi.fn()))
  afterEach(() => vi.unstubAllGlobals())

  it("has a managed workload to work with", () => {
    expect(orphan).toBeDefined()
    expect(orphan.managed).toBe(true)
  })

  it("names the machine, its node and the drill that left it", async () => {
    render(wrap(<CleanupWorkload workload={orphan} canOperate />))
    await userEvent.click(screen.getByRole("button", { name: /^destroy$/i }))

    // The id appears in the title too; this is the sentence that has to carry
    // the whole identity of the machine, so it is found by its own wording
    // and then read in full.
    const described = screen.getByText(/cannot be undone/i)
    expect(described).toHaveTextContent(orphan.id)
    expect(described).toHaveTextContent(orphan.name)
    expect(described).toHaveTextContent(String(orphan.node))
    expect(described).toHaveTextContent(
      orphan.recovery_run_id ? /left behind by drill/i : /no drill claims it/i,
    )
  })

  // No id to retype: the backend has already refused anything outside the
  // reserved range and anything it does not own. A string typed by reflex
  // would be theatre.
  it("asks for no confirmation text", async () => {
    render(wrap(<CleanupWorkload workload={orphan} canOperate />))
    await userEvent.click(screen.getByRole("button", { name: /^destroy$/i }))
    expect(screen.queryByRole("textbox")).toBeNull()
  })

  it("posts to the cleanup route", async () => {
    vi.mocked(fetch).mockResolvedValue(
      new Response(JSON.stringify(cleanupFixture), {
        status: 200,
        headers: { "content-type": "application/json" },
      }),
    )
    render(wrap(<CleanupWorkload workload={orphan} canOperate />))

    await openAndConfirm()

    await waitFor(() => expect(fetch).toHaveBeenCalled())
    const [url, init] = vi.mocked(fetch).mock.calls[0] ?? []
    expect(url).toBe(`/api/v1/cleanup/${orphan.id}`)
    expect((init as RequestInit).method).toBe("POST")
  })

  // The backend's own refusal is the useful message - it is the one that says
  // the machine is not one of ours - so it has to be readable.
  it("shows the refusal in the dialog", async () => {
    vi.mocked(fetch).mockResolvedValue(
      new Response(
        JSON.stringify({
          type: "bad-request",
          title: "Bad request",
          status: 400,
          detail: "RestoreLab only ever creates workloads in its reserved range",
        }),
        { status: 400, headers: { "content-type": "application/problem+json" } },
      ),
    )
    render(wrap(<CleanupWorkload workload={orphan} canOperate />))

    await openAndConfirm()

    expect(await screen.findByText(/reserved range/i)).toBeInTheDocument()
  })

  it("is absent without the operate scope", () => {
    const { container } = render(
      wrap(<CleanupWorkload workload={orphan} canOperate={false} />),
    )
    expect(container).toBeEmptyDOMElement()
  })
})

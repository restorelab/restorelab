import { planFixture } from "@/api/fixtures"
import { QueryClient, QueryClientProvider } from "@tanstack/react-query"
import { render, screen, waitFor } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import type { ReactNode } from "react"
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"
import { DeletePlan } from "./delete-plan"

function wrap(ui: ReactNode) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return <QueryClientProvider client={qc}>{ui}</QueryClientProvider>
}

async function openAndConfirm() {
  await userEvent.click(screen.getByRole("button", { name: /^delete$/i }))
  const buttons = screen.getAllByRole("button", { name: /^delete$/i })
  await userEvent.click(buttons[buttons.length - 1] as HTMLElement)
}

describe("DeletePlan", () => {
  beforeEach(() => vi.stubGlobal("fetch", vi.fn()))
  afterEach(() => vi.unstubAllGlobals())

  it("names the plan it is about to remove", async () => {
    render(wrap(<DeletePlan plan={planFixture} canManage onDeleted={() => {}} />))
    await userEvent.click(screen.getByRole("button", { name: /^delete$/i }))
    expect(screen.getByText(new RegExp(planFixture.name))).toBeInTheDocument()
  })

  // The reassuring half is the one worth saying, because it is the one
  // nobody expects: B3 proved a deleted plan costs its runs exactly one
  // field, plan_id, and nothing else.
  it("says what deleting does not destroy", async () => {
    render(wrap(<DeletePlan plan={planFixture} canManage onDeleted={() => {}} />))
    await userEvent.click(screen.getByRole("button", { name: /^delete$/i }))
    expect(screen.getByText(/past drills keep their name/i)).toBeInTheDocument()
  })

  it("deletes by name", async () => {
    vi.mocked(fetch).mockResolvedValue(new Response(null, { status: 204 }))
    const onDeleted = vi.fn()
    render(wrap(<DeletePlan plan={planFixture} canManage onDeleted={onDeleted} />))

    await openAndConfirm()

    await waitFor(() => expect(onDeleted).toHaveBeenCalled())
    const [url, init] = vi.mocked(fetch).mock.calls[0] ?? []
    expect(url).toBe(`/api/v1/plans/${planFixture.name}`)
    expect((init as RequestInit).method).toBe("DELETE")
  })

  it("shows a refusal in the dialog", async () => {
    vi.mocked(fetch).mockResolvedValue(
      new Response(
        JSON.stringify({
          type: "about:blank",
          title: "Not found",
          status: 404,
          detail: "no such plan",
        }),
        { status: 404, headers: { "content-type": "application/problem+json" } },
      ),
    )
    render(wrap(<DeletePlan plan={planFixture} canManage onDeleted={() => {}} />))

    await openAndConfirm()

    expect(await screen.findByText(/no such plan/i)).toBeInTheDocument()
  })

  it("is absent without the manage scope", () => {
    const { container } = render(
      wrap(<DeletePlan plan={planFixture} canManage={false} onDeleted={() => {}} />),
    )
    expect(container).toBeEmptyDOMElement()
  })
})

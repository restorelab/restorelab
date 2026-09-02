import { plansPageFixture } from "@/api/fixtures"
import { QueryClient, QueryClientProvider } from "@tanstack/react-query"
import { render, screen } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import type { ReactNode } from "react"
import { describe, expect, it, vi } from "vitest"
import { PlansContent } from "./index"

function wrap(ui: ReactNode) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return <QueryClientProvider client={qc}>{ui}</QueryClientProvider>
}

const noop = () => undefined

describe("PlansContent", () => {
  it("renders a row per plan", () => {
    render(
      wrap(<PlansContent plans={plansPageFixture} canManage canOperate onRun={noop} />),
    )
    for (const p of plansPageFixture.items) {
      expect(screen.getByText(p.name)).toBeInTheDocument()
    }
  })

  it("explains an empty catalogue rather than showing an empty table", () => {
    render(
      wrap(<PlansContent plans={{ items: [] }} canManage canOperate onRun={noop} />),
    )
    expect(screen.getByText(/no plan yet/i)).toBeInTheDocument()
  })

  // Three powers meet on this screen and none implies another. What a session
  // cannot do is not rendered - never a disabled control.
  it("offers writing only with manage, and says why not", () => {
    render(
      wrap(
        <PlansContent
          plans={plansPageFixture}
          canManage={false}
          canOperate
          onRun={noop}
        />,
      ),
    )
    expect(screen.queryByText(/^new plan$/i)).toBeNull()
    expect(
      screen.getByText(/read the catalogue but not change it/i),
    ).toBeInTheDocument()
  })

  it("offers running only with operate", () => {
    render(
      wrap(
        <PlansContent
          plans={plansPageFixture}
          canManage
          canOperate={false}
          onRun={noop}
        />,
      ),
    )
    expect(screen.queryByRole("button", { name: /run this plan/i })).toBeNull()
  })

  it("runs the plan it was asked to run, by name", async () => {
    const onRun = vi.fn()
    render(
      wrap(
        <PlansContent plans={plansPageFixture} canManage canOperate onRun={onRun} />,
      ),
    )

    const [first] = plansPageFixture.items
    const buttons = screen.getAllByRole("button", { name: /run this plan/i })
    await userEvent.click(buttons[0] as HTMLElement)

    expect(onRun).toHaveBeenCalledWith(first?.name)
  })

  // A listing ships no documents - fifty plans must not become fifty YAML
  // blobs to draw a table - so the table must never try to show one.
  it("shows no document in the listing", () => {
    render(
      wrap(<PlansContent plans={plansPageFixture} canManage canOperate onRun={noop} />),
    )
    expect(screen.queryByText(/restored nightly/)).toBeNull()
  })
})

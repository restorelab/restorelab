import { workloadsPageFixture } from "@/api/fixtures"
import { QueryClient, QueryClientProvider } from "@tanstack/react-query"
import { render, screen } from "@testing-library/react"
import type { ReactNode } from "react"
import { describe, expect, it } from "vitest"
import { OrphanBanner } from "./orphan-banner"

const orphans = workloadsPageFixture.items.filter((w) => w.managed)

function wrap(ui: ReactNode) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return <QueryClientProvider client={qc}>{ui}</QueryClientProvider>
}

describe("OrphanBanner", () => {
  // A clean cluster says nothing. A banner that is always there is furniture,
  // and furniture is not read.
  it("says nothing when the cluster is clean", () => {
    const { container } = render(wrap(<OrphanBanner orphans={[]} canOperate />))
    expect(container).toBeEmptyDOMElement()
  })

  it("counts what is still on the cluster", () => {
    render(wrap(<OrphanBanner orphans={orphans} canOperate />))
    expect(screen.getByText(/temporary workload/i)).toBeInTheDocument()
  })

  it("offers to destroy each one", () => {
    render(wrap(<OrphanBanner orphans={orphans} canOperate />))
    expect(screen.getAllByRole("button", { name: /^destroy$/i })).toHaveLength(
      orphans.length,
    )
  })

  // A read-only session still needs to know: an orphan is a machine burning
  // storage on a production cluster, and saying nothing would be worse than
  // saying "you cannot fix this from here".
  it("still reports the orphans to a session that cannot act", () => {
    render(wrap(<OrphanBanner orphans={orphans} canOperate={false} />))
    expect(screen.getByText(/temporary workload/i)).toBeInTheDocument()
    expect(screen.queryByRole("button", { name: /^destroy$/i })).toBeNull()
  })
})

import { ApiError } from "@/api/client"
import { render, screen } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { describe, expect, it, vi } from "vitest"
import { EmptyState } from "./empty-state"
import { ErrorState } from "./error-state"

describe("EmptyState", () => {
  it("shows the explanation and a copyable command", async () => {
    const write = vi.fn().mockResolvedValue(undefined)
    vi.stubGlobal("navigator", { clipboard: { writeText: write } })

    render(
      <EmptyState
        title="No drills yet"
        description="Run the first one from the machine running restorelab."
        command="restorelab recovery test web-01"
      />,
    )
    expect(screen.getByText("No drills yet")).toBeInTheDocument()
    expect(screen.getByText("restorelab recovery test web-01")).toBeInTheDocument()

    await userEvent.click(screen.getByRole("button", { name: /copy/i }))
    expect(write).toHaveBeenCalledWith("restorelab recovery test web-01")
    vi.unstubAllGlobals()
  })

  it("renders without a command", () => {
    render(<EmptyState title="Nothing here" />)
    expect(screen.queryByRole("button", { name: /copy/i })).toBeNull()
  })
})

describe("ErrorState", () => {
  it("shows the server's own words, not an invented message", () => {
    render(
      <ErrorState
        error={
          new ApiError({
            type: "no-such-run",
            title: "No such recovery run",
            status: 404,
            detail: "run 94bce70d is not in the history",
          })
        }
      />,
    )
    expect(screen.getByText("No such recovery run")).toBeInTheDocument()
    expect(screen.getByText("run 94bce70d is not in the history")).toBeInTheDocument()
  })

  it("falls back to its own title for an error that is not from the API", () => {
    render(<ErrorState error={new Error("boom")} />)
    expect(screen.getByText("Something went wrong")).toBeInTheDocument()
    expect(screen.getByText("boom")).toBeInTheDocument()
  })

  it("offers a retry when given one", async () => {
    const retry = vi.fn()
    render(<ErrorState error={new Error("boom")} onRetry={retry} />)
    await userEvent.click(screen.getByRole("button", { name: /try again/i }))
    expect(retry).toHaveBeenCalledOnce()
  })
})

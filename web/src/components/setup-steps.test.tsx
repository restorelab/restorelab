import { setupFailedFixture, setupResultFixture } from "@/api/fixtures"
import { render, screen } from "@testing-library/react"
import { describe, expect, it } from "vitest"
import { SetupSteps } from "./setup-steps"

describe("SetupSteps", () => {
  it("lists every step in the order it was performed", () => {
    render(<SetupSteps steps={setupResultFixture.steps} />)
    for (const s of setupResultFixture.steps) {
      expect(screen.getByText(s.description)).toBeInTheDocument()
    }
  })

  it("shows each step's status", () => {
    render(<SetupSteps steps={setupResultFixture.steps} />)
    expect(screen.getByText("created")).toBeInTheDocument()
    expect(screen.getByText("already exists")).toBeInTheDocument()
  })

  // A failure that showed nothing would be a dead end. Every step is
  // idempotent, so knowing how far it got is what makes "fix it and run it
  // again" a real instruction.
  it("shows the steps a failed setup got through", () => {
    const steps = setupFailedFixture.steps ?? []
    expect(steps.length).toBeGreaterThan(0)
    render(<SetupSteps steps={steps} />)
    expect(screen.getAllByRole("listitem")).toHaveLength(steps.length)
  })

  it("renders nothing when there is nothing yet", () => {
    const { container } = render(<SetupSteps steps={[]} />)
    expect(container).toBeEmptyDOMElement()
  })
})

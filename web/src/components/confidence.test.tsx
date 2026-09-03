import { render, screen } from "@testing-library/react"
import { describe, expect, it } from "vitest"
import { ConfidenceScore } from "./confidence"

describe("ConfidenceScore", () => {
  it("renders a never-tested workload as a dash, never as zero", () => {
    render(<ConfidenceScore value={null} tested={false} />)
    expect(screen.getByText("--")).toBeInTheDocument()
    expect(screen.queryByText("0")).toBeNull()
    expect(screen.queryByText("0%")).toBeNull()
  })

  it("renders a real score", () => {
    render(<ConfidenceScore value={82} tested={true} />)
    expect(screen.getByText("82")).toBeInTheDocument()
  })

  it("renders a genuine zero as zero: that is a measurement, not an absence", () => {
    render(<ConfidenceScore value={0} tested={true} />)
    expect(screen.getByText("0")).toBeInTheDocument()
  })

  it("colours a low score as a failure and a high one as a success", () => {
    const { rerender } = render(<ConfidenceScore value={31} tested={true} />)
    expect(screen.getByRole("meter")).toHaveAttribute("data-tone", "failed")
    rerender(<ConfidenceScore value={95} tested={true} />)
    expect(screen.getByRole("meter")).toHaveAttribute("data-tone", "success")
  })
})

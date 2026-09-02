import { initialStreamState } from "@/api/runStream"
import type { Check, Step } from "@/api/types"
import { render, screen } from "@testing-library/react"
import { describe, expect, it } from "vitest"
import { PhaseTimeline } from "./phase-timeline"

function step(over: Partial<Step> = {}): Step {
  return {
    name: "restore",
    state: "RESTORING",
    status: "done",
    started_at: "2026-09-02T12:00:00Z",
    completed_at: "2026-09-02T12:01:02Z",
    duration_seconds: 62,
    duration: "1m02s",
    ...over,
  }
}

function check(over: Partial<Check> = {}): Check {
  return {
    name: "ssh",
    type: "ssh",
    status: "pass",
    pass: true,
    started_at: "2026-09-02T12:03:00Z",
    completed_at: "2026-09-02T12:03:01Z",
    duration_seconds: 1,
    duration: "1s",
    attempts: 1,
    ...over,
  }
}

describe("PhaseTimeline", () => {
  it("renders a step with its duration", () => {
    render(<PhaseTimeline steps={[step()]} checks={[]} live={null} />)
    expect(screen.getByText("restore")).toBeInTheDocument()
    expect(screen.getByText("1m02s")).toBeInTheDocument()
  })

  it("lets the live stream override the document, because it is more recent", () => {
    const live = {
      ...initialStreamState,
      steps: new Map([
        [
          "restore",
          { name: "restore", status: "running" as const, at: "2026-09-02T12:00:30Z" },
        ],
      ]),
    }
    render(<PhaseTimeline steps={[step({ status: "done" })]} checks={[]} live={live} />)
    expect(screen.getByTitle("Running")).toBeInTheDocument()
    expect(screen.queryByTitle("Done")).toBeNull()
  })

  it("adds a step the document does not have yet", () => {
    const live = {
      ...initialStreamState,
      steps: new Map([
        [
          "boot",
          { name: "boot", status: "running" as const, at: "2026-09-02T12:01:10Z" },
        ],
      ]),
    }
    render(<PhaseTimeline steps={[step()]} checks={[]} live={live} />)
    expect(screen.getByText("restore")).toBeInTheDocument()
    expect(screen.getByText("boot")).toBeInTheDocument()
  })

  it("shows a failed check's message", () => {
    render(
      <PhaseTimeline
        steps={[step({ name: "checks", status: "failed" })]}
        checks={[
          check({
            name: "http",
            status: "fail",
            pass: false,
            message: "connection refused",
          }),
        ]}
        live={null}
      />,
    )
    expect(screen.getByText("http")).toBeInTheDocument()
    expect(screen.getByText("connection refused")).toBeInTheDocument()
  })

  it("says how many attempts a check needed, only when it needed more than one", () => {
    const { rerender } = render(
      <PhaseTimeline steps={[step()]} checks={[check({ attempts: 1 })]} live={null} />,
    )
    expect(screen.queryByText(/attempt/i)).toBeNull()

    rerender(
      <PhaseTimeline steps={[step()]} checks={[check({ attempts: 4 })]} live={null} />,
    )
    expect(screen.getByText("4 attempts")).toBeInTheDocument()
  })

  it("says so when a drill recorded no steps at all", () => {
    render(<PhaseTimeline steps={[]} checks={[]} live={null} />)
    expect(screen.getByText(/recorded no steps/i)).toBeInTheDocument()
  })
})

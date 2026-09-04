import { runFinishedFixture } from "@/api/fixtures"
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

// The captured body of GET /recovery-runs/{id}, which is where the shape of a
// captured value is settled. `orders` measured a row count against a baseline
// and `sessions` measured one with no history behind it; inventing either here
// would only prove the component agrees with this file.
function fixtureCheck(name: string): Check {
  const found = runFinishedFixture.checks.find((c) => c.name === name)
  if (!found) {
    throw new Error(
      `the captured run fixture has no ${name} check: fix internal/api/fixtures_test.go`,
    )
  }
  return found
}

function checksStep(): Step {
  return step({ name: "checks", status: "done" })
}

describe("PhaseTimeline captured values", () => {
  it("shows what a check measured beside what it used to measure", () => {
    render(
      <PhaseTimeline
        steps={[checksStep()]}
        checks={[fixtureCheck("orders")]}
        live={null}
      />,
    )
    expect(screen.getByText("rows: 1 206 890 (baseline 1 204 331)")).toBeInTheDocument()
  })

  // A count of 1204331 shown as 1.204331e+06 makes the reader do arithmetic to
  // find out whether their database is empty, which is the question they came
  // to ask.
  it("never renders a count in scientific notation", () => {
    const { container } = render(
      <PhaseTimeline
        steps={[checksStep()]}
        checks={[fixtureCheck("orders")]}
        live={null}
      />,
    )
    expect(container.textContent).not.toMatch(/e[+-]\d/i)
  })

  // "no previous drill measured this" and "the previous drill measured zero"
  // are opposite pieces of news, and printing 0 for the first one tells an
  // operator their database emptied overnight.
  it("renders an absent baseline as the no-value glyph, never as zero", () => {
    render(
      <PhaseTimeline
        steps={[checksStep()]}
        checks={[fixtureCheck("sessions")]}
        live={null}
      />,
    )
    expect(screen.getByText("rows: 4 821 (baseline --)")).toBeInTheDocument()
    expect(screen.queryByText(/baseline 0/)).toBeNull()
  })

  it("says which way the value moved, and by how much", () => {
    const measured = fixtureCheck("orders")
    const { rerender } = render(
      <PhaseTimeline steps={[checksStep()]} checks={[measured]} live={null} />,
    )
    expect(screen.getByText("+2 559")).toBeInTheDocument()

    rerender(
      <PhaseTimeline
        steps={[checksStep()]}
        checks={[
          { ...measured, values: [{ name: "rows", value: 4, baseline: 1204331 }] },
        ]}
        live={null}
      />,
    )
    expect(screen.getByText("-1 204 327")).toBeInTheDocument()
  })

  // The engine judges a value only against a bound the plan declared, so the
  // interface must not colour a drop as a failure. RestoreLab has no opinion
  // about this number and the screen must not invent one.
  it("does not colour the change, because nothing declared a threshold", () => {
    render(
      <PhaseTimeline
        steps={[checksStep()]}
        checks={[
          {
            ...fixtureCheck("orders"),
            values: [{ name: "rows", value: 4, baseline: 1204331 }],
          },
        ]}
        live={null}
      />,
    )
    const drop = screen.getByText("-1 204 327")
    expect(drop.className).not.toMatch(/text-state-/)
  })

  it("shows nothing at all for a check that measured nothing", () => {
    render(
      <PhaseTimeline
        steps={[checksStep()]}
        checks={[fixtureCheck("ssh")]}
        live={null}
      />,
    )
    expect(screen.queryByText(/baseline/)).toBeNull()
  })
})

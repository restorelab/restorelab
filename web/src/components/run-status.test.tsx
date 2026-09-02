import { RUN_STATES } from "@/api/types"
import en from "@/i18n/locales/en/common.json"
import { render, screen } from "@testing-library/react"
import { describe, expect, it } from "vitest"
import { RunStatusBadge, checkTone, runLabelKey, runTone, stepTone } from "./run-status"

const TERMINAL = ["SUCCESS", "FAILED", "CANCELLED", "CLEANUP_FAILED"]

describe("runTone", () => {
  it("colours a success green and a failure red", () => {
    expect(runTone("SUCCESS")).toBe("success")
    expect(runTone("FAILED")).toBe("failed")
    expect(runTone("CLEANUP_FAILED")).toBe("failed")
  })

  it("colours a cancelled run neutral, not as a failure", () => {
    expect(runTone("CANCELLED")).toBe("idle")
  })

  it("colours a degraded success amber: it recovered, but not cleanly", () => {
    expect(runTone("SUCCESS", "DEGRADED")).toBe("warning")
  })

  it("colours every in-flight state as running", () => {
    for (const s of RUN_STATES) {
      if (TERMINAL.includes(s) || s === "QUEUED") continue
      expect(runTone(s), `wrong tone for ${s}`).toBe("running")
    }
  })

  it("colours a queued run idle: nothing is happening to it yet", () => {
    expect(runTone("QUEUED")).toBe("idle")
  })
})

describe("the state vocabulary", () => {
  it("has a label for every state the API can send", () => {
    const labels = en.runState as Record<string, string>
    for (const s of RUN_STATES) {
      expect(labels[s], `no label for ${s}`).toBeTruthy()
    }
  })

  it("keys a label by the state itself", () => {
    expect(runLabelKey("RUNNING_CHECKS")).toBe("runState.RUNNING_CHECKS")
  })
})

describe("stepTone and checkTone", () => {
  it("maps every step status", () => {
    expect(stepTone("pending")).toBe("idle")
    expect(stepTone("running")).toBe("running")
    expect(stepTone("done")).toBe("success")
    expect(stepTone("failed")).toBe("failed")
    expect(stepTone("skipped")).toBe("idle")
  })

  it("maps every check status, and an error is not a failure", () => {
    expect(checkTone("pass")).toBe("success")
    expect(checkTone("fail")).toBe("failed")
    expect(checkTone("error")).toBe("warning")
    expect(checkTone("skipped")).toBe("idle")
  })
})

describe("RunStatusBadge", () => {
  it("renders the state's own words", () => {
    render(<RunStatusBadge state="RUNNING_CHECKS" />)
    expect(screen.getByText("Running checks")).toBeInTheDocument()
  })

  it("says Succeeded for a clean pass and still Succeeded for a degraded one", () => {
    const { rerender } = render(<RunStatusBadge state="SUCCESS" />)
    expect(screen.getByText("Succeeded")).toBeInTheDocument()
    rerender(<RunStatusBadge state="SUCCESS" result="DEGRADED" />)
    expect(screen.getByText("Succeeded")).toHaveClass("text-state-warning")
  })
})

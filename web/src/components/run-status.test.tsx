import { PROOF_LEVELS, RUN_STATES } from "@/api/types"
import en from "@/i18n/locales/en/common.json"
import { render, screen } from "@testing-library/react"
import { describe, expect, it } from "vitest"
import {
  ProofBadge,
  ProofPhrase,
  RunStatusBadge,
  checkTone,
  proofIsSettled,
  proofLabelKey,
  proofPhraseKey,
  proofTone,
  runLabelKey,
  runTone,
  stepTone,
} from "./run-status"

const TERMINAL = ["SUCCESS", "FAILED", "CANCELLED", "CLEANUP_FAILED", "INCONCLUSIVE"]

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

  /**
   * The one that matters most. A drill whose checks could not be evaluated -
   * a tcp: check dialled from a machine with no route into the isolated
   * recovery network - restored and booted the workload perfectly. Painting
   * it red tells somebody their backup is broken when it is not, and a
   * dashboard that cries wolf about backups is worse than no dashboard.
   */
  it("colours an inconclusive run amber, never red", () => {
    expect(runTone("INCONCLUSIVE")).toBe("warning")
    expect(runTone("INCONCLUSIVE")).not.toBe("failed")
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

describe("proofTone", () => {
  it("colours a proven service and proven data as a success", () => {
    expect(proofTone("SERVICE")).toBe("success")
    expect(proofTone("DATA")).toBe("success")
  })

  /**
   * The whole reason the scale exists. A machine drilled with the default
   * `cmd:hostname` succeeds and scores well, having proven that the kernel
   * came up and nothing else. Amber is where that stops being invisible.
   */
  it("colours a boot-only proof amber: it looks like a pass and is not", () => {
    expect(proofTone("BOOT")).toBe("warning")
  })

  // A low level is a fact about what was asked for, not a fault. Red belongs
  // to a drill that went badly, and a restore-only drill did not.
  it("never paints a level red, not even NONE", () => {
    for (const level of PROOF_LEVELS) {
      expect(proofTone(level), `${level} was painted as a failure`).not.toBe("failed")
    }
    expect(proofTone("NONE")).toBe("idle")
  })

  it("leaves an unrecorded level neutral: nothing follows from it either way", () => {
    expect(proofTone(undefined)).toBe("idle")
  })
})

describe("the proof vocabulary", () => {
  it("has a label and a sentence for every level the API can send", () => {
    const labels = en.proofLevel as Record<string, string>
    const phrases = en.proofPhrase as Record<string, string>
    for (const level of PROOF_LEVELS) {
      expect(labels[level], `no label for ${level}`).toBeTruthy()
      expect(phrases[level], `no sentence for ${level}`).toBeTruthy()
    }
  })

  /**
   * The distinction the whole slice turns on. A run from before the feature
   * carries no level, and saying "nothing was proven" about it would be an
   * assertion nobody is entitled to make.
   */
  it("keys an absent level as unrecorded, never as NONE", () => {
    expect(proofLabelKey(undefined)).toBe("proofLevel.unrecorded")
    expect(proofPhraseKey(undefined)).toBe("proofPhrase.unrecorded")
    expect(proofLabelKey(undefined)).not.toBe(proofLabelKey("NONE"))
    expect(proofPhraseKey(undefined)).not.toBe(proofPhraseKey("NONE"))
  })

  it("keys a level by the level itself", () => {
    expect(proofLabelKey("SERVICE")).toBe("proofLevel.SERVICE")
    expect(proofPhraseKey("BOOT")).toBe("proofPhrase.BOOT")
  })
})

/**
 * A drill still going carries NONE, honestly: it has established nothing yet.
 * Shown as-is it reads as a verdict on a run that has not reached one.
 */
describe("proofIsSettled", () => {
  it("holds back the level of a run that has not finished", () => {
    for (const state of RUN_STATES) {
      if (TERMINAL.includes(state)) continue
      expect(proofIsSettled(state), `${state} was treated as settled`).toBe(false)
    }
  })

  it("releases it on every terminal state, cancelled included", () => {
    for (const state of TERMINAL) {
      expect(proofIsSettled(state as (typeof RUN_STATES)[number])).toBe(true)
    }
  })

  it("says yes when no run is named: nothing is in flight to wait for", () => {
    expect(proofIsSettled(undefined)).toBe(true)
  })
})

describe("ProofBadge", () => {
  it("names what was proven", () => {
    render(<ProofBadge level="BOOT" state="SUCCESS" />)
    expect(screen.getByText("Boot only")).toHaveClass("text-state-warning")
  })

  it("says nothing when no level was recorded", () => {
    const { container } = render(<ProofBadge state="SUCCESS" />)
    expect(container).toBeEmptyDOMElement()
  })

  it("says nothing about a drill that is still running", () => {
    const { container } = render(<ProofBadge level="NONE" state="RESTORING" />)
    expect(container).toBeEmptyDOMElement()
    expect(screen.queryByText(/nothing proven/i)).toBeNull()
  })
})

describe("ProofPhrase", () => {
  it("says what a drill established, in words somebody can act on", () => {
    render(<ProofPhrase level="BOOT" state="SUCCESS" />)
    expect(screen.getByText("Only the boot was verified.")).toBeInTheDocument()
  })

  // "We did not write it down" and "nothing was proven" are different
  // statements, and this is the one screen with room to say which it is.
  it("distinguishes an unrecorded level from a proven nothing", () => {
    const { rerender } = render(<ProofPhrase state="SUCCESS" />)
    expect(screen.getByText(/never recorded/i)).toBeInTheDocument()
    rerender(<ProofPhrase level="NONE" state="SUCCESS" />)
    expect(
      screen.getByText(/nothing was verified inside the guest/i),
    ).toBeInTheDocument()
  })

  it("stays silent while the drill is still going", () => {
    const { container } = render(<ProofPhrase level="NONE" state="RUNNING_CHECKS" />)
    expect(container).toBeEmptyDOMElement()
  })
})

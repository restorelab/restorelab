import type { Doctor } from "@/api/types"
import { render, screen } from "@testing-library/react"
import { describe, expect, it } from "vitest"
import { DoctorContent, findingTone } from "./doctor"

describe("DoctorContent", () => {
  it("says everything is fine when there are no findings", () => {
    const d: Doctor = {
      provider_id: "pve",
      endpoint: "https://pve:8006",
      ok: true,
      problems: 0,
      findings: [],
    }
    render(<DoctorContent doctor={d} />)
    expect(screen.getByText(/no problems found/i)).toBeInTheDocument()
  })

  it("lists each finding with its area and detail", () => {
    const d: Doctor = {
      provider_id: "pve",
      ok: false,
      problems: 1,
      findings: [
        {
          level: "fail",
          area: "network",
          title: "No isolated bridge",
          detail: "create one with restorelab network create",
        },
      ],
    }
    render(<DoctorContent doctor={d} />)
    expect(screen.getByText("No isolated bridge")).toBeInTheDocument()
    expect(screen.getByText("network")).toBeInTheDocument()
    expect(screen.getByText(/restorelab network create/)).toBeInTheDocument()
  })

  it("distinguishes a warning from an error", () => {
    const d: Doctor = {
      provider_id: "pve",
      ok: false,
      problems: 1,
      findings: [
        { level: "warn", area: "backup", title: "Backups are not encrypted" },
        { level: "fail", area: "network", title: "No isolated bridge" },
      ],
    }
    render(<DoctorContent doctor={d} />)
    expect(screen.getByText("Backups are not encrypted").closest("li")).toHaveAttribute(
      "data-tone",
      "warning",
    )
    expect(screen.getByText("No isolated bridge").closest("li")).toHaveAttribute(
      "data-tone",
      "failed",
    )
  })
})

/**
 * The levels the Go side actually emits.
 *
 * internal/diag/diag.go defines exactly three: ok, warn, fail. The plan this
 * screen was written from said "error", which no diagnostic ever sends, so the
 * tests above used to assert against a level that cannot occur. This is the
 * guard that keeps the mapping honest - and it matters more than it looks,
 * because a healthy cluster comes back as a list of `ok` findings, and a rule
 * that sent every unknown level to `idle` would paint that list grey.
 */
describe("findingTone", () => {
  it("maps every level internal/diag can emit", () => {
    expect(findingTone("ok")).toBe("success")
    expect(findingTone("warn")).toBe("warning")
    expect(findingTone("fail")).toBe("failed")
  })

  it("tolerates the longer spellings without needing them", () => {
    expect(findingTone("warning")).toBe("warning")
    expect(findingTone("error")).toBe("failed")
    expect(findingTone("FAIL")).toBe("failed")
  })

  it("falls back to neutral for a level this build has never heard of", () => {
    expect(findingTone("something-new")).toBe("idle")
  })
})

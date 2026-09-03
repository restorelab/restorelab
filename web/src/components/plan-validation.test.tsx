import { ApiError } from "@/api/client"
import { validateInvalidFixture, validateOkFixture } from "@/api/fixtures"
import { render, screen } from "@testing-library/react"
import { describe, expect, it } from "vitest"
import { PlanValidation, proofSentence } from "./plan-validation"

describe("PlanValidation", () => {
  it("says what the document means when it is valid", () => {
    render(<PlanValidation result={validateOkFixture} />)
    expect(screen.getByText(validateOkFixture.name)).toBeInTheDocument()
    expect(screen.getByText(validateOkFixture.workload_id)).toBeInTheDocument()
  })

  // The normalized document is the useful half: it shows the difference
  // between a field left out and a field left out meaning something. The
  // capture proves it - it comes back longer than what was sent, carrying
  // restore.network and the check's retry policy nobody typed.
  it("shows the document with its defaults applied", () => {
    render(<PlanValidation result={validateOkFixture} />)
    expect(screen.getByText(/with defaults applied/i)).toBeInTheDocument()
    const shown = screen.getByText(/^name: web-tier/m)
    expect(shown).toHaveTextContent("network: isolated")
    expect(shown).toHaveTextContent("retry_interval")
  })

  // The refusal the Go side worded is the one that names the field. Rendering
  // a generic "invalid" instead would throw away the only useful part.
  it("relays the refusal the server worded", () => {
    render(<PlanValidation error={new ApiError(validateInvalidFixture)} />)
    expect(screen.getByText(/workload\.id is required/)).toBeInTheDocument()
  })

  /**
   * The editor is the only screen where `proves:` is ever discovered, and
   * "valid" was the only question it could answer until this appeared. A plan
   * that would establish nothing beyond the boot should say so while it is
   * still being written, not after five minutes of drilling.
   */
  it("says what the plan would prove, not only that it parses", () => {
    render(<PlanValidation result={validateOkFixture} />)
    expect(screen.getByText(/what it would prove/i)).toBeInTheDocument()
    expect(
      screen.getByText(/the service would be verified, the data would not/i),
    ).toBeInTheDocument()
  })

  it("points at the field that raises the level", () => {
    render(<PlanValidation result={validateOkFixture} />)
    expect(screen.getByText(/proves: data/i)).toBeInTheDocument()
  })

  // A capture from a server that does not word a summary - or an older one -
  // leaves the block out rather than showing an empty heading.
  it("says nothing about proof when the server said nothing", () => {
    render(
      <PlanValidation
        result={{
          ...validateOkFixture,
          proof_level: undefined,
          proof_summary: undefined,
        }}
      />,
    )
    expect(screen.queryByText(/what it would prove/i)).toBeNull()
  })

  it("says it is checking while a request is in flight", () => {
    render(<PlanValidation pending />)
    expect(screen.getByText(/checking/i)).toBeInTheDocument()
  })

  it("says nothing before anything has been typed", () => {
    const { container } = render(<PlanValidation />)
    expect(container).toBeEmptyDOMElement()
  })
})

describe("proofSentence", () => {
  // The badge already says SERVICE; the server's line repeats it because the
  // CLI has no badge to carry it.
  it("drops the level the badge next to it already shows", () => {
    expect(proofSentence(validateOkFixture)).toBe(
      "the service would be verified, the data would not",
    )
  })

  // The wording belongs to the API. Anything that does not start with the
  // level it declares is passed through untouched rather than trimmed on a
  // guess.
  it("leaves a summary worded differently exactly as it was written", () => {
    expect(
      proofSentence({
        ...validateOkFixture,
        proof_level: "DATA",
        proof_summary: "this plan reads the ledger back",
      }),
    ).toBe("this plan reads the ledger back")
  })

  it("has nothing to say when the server sent no summary", () => {
    expect(
      proofSentence({ ...validateOkFixture, proof_summary: undefined }),
    ).toBeUndefined()
  })
})

import { ApiError } from "@/api/client"
import { validateInvalidFixture, validateOkFixture } from "@/api/fixtures"
import { render, screen } from "@testing-library/react"
import { describe, expect, it } from "vitest"
import { PlanValidation } from "./plan-validation"

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

  it("says it is checking while a request is in flight", () => {
    render(<PlanValidation pending />)
    expect(screen.getByText(/checking/i)).toBeInTheDocument()
  })

  it("says nothing before anything has been typed", () => {
    const { container } = render(<PlanValidation />)
    expect(container).toBeEmptyDOMElement()
  })
})

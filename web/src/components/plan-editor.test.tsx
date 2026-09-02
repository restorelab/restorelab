import { ApiError } from "@/api/client"
import {
  planFixture,
  problem409VersionFixture,
  validateOkFixture,
} from "@/api/fixtures"
import { QueryClient, QueryClientProvider } from "@tanstack/react-query"
import { render, screen, waitFor } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import type { ReactNode } from "react"
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"
import { PlanEditor, conflictKind } from "./plan-editor"

function wrap(ui: ReactNode) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return <QueryClientProvider client={qc}>{ui}</QueryClientProvider>
}

function json(status: number, body: unknown, type = "application/json") {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "content-type": type },
  })
}

const document = planFixture.yaml ?? ""

const renameProblem = {
  type: "https://restorelab.dev/problems/rename-not-supported",
  title: "A plan cannot be renamed in place",
  status: 409,
  detail: "the document names other, but this URL is web-tier",
}

// The two 409s this route answers are unrelated, and telling them apart is
// what stops a rename being reported as somebody else's edit.
describe("conflictKind", () => {
  it("reads the problem type, not the title", () => {
    expect(conflictKind(new ApiError(problem409VersionFixture))).toBe("version")
    expect(conflictKind(new ApiError(renameProblem))).toBe("rename")
  })

  it("has no answer for anything that is not a 409", () => {
    expect(conflictKind(new ApiError({ ...renameProblem, status: 400 }))).toBeNull()
    expect(conflictKind(new Error("boom"))).toBeNull()
    expect(conflictKind(undefined)).toBeNull()
  })
})

describe("PlanEditor", () => {
  beforeEach(() => vi.stubGlobal("fetch", vi.fn()))
  afterEach(() => vi.unstubAllGlobals())

  it("opens on the document it was given", () => {
    render(wrap(<PlanEditor initialDocument={document} onSaved={() => {}} />))
    expect(screen.getByRole("textbox")).toHaveValue(document)
  })

  // The document goes back exactly as it came, comments and all. Recomposing
  // it from parsed fields is what a form would do, and it is why this is not
  // a form.
  it("posts the document verbatim when there is no plan yet", async () => {
    vi.mocked(fetch).mockResolvedValue(json(201, planFixture))
    render(wrap(<PlanEditor initialDocument={document} onSaved={() => {}} />))

    await userEvent.click(screen.getByRole("button", { name: /^save$/i }))

    await waitFor(() => expect(fetch).toHaveBeenCalled())
    const [url, init] = vi.mocked(fetch).mock.calls[0] ?? []
    expect(url).toBe("/api/v1/plans")
    expect((init as RequestInit).method).toBe("POST")
    expect((init as RequestInit).body).toBe(document)
  })

  // The version guard is the whole point of sending one.
  it("sends the version it loaded", async () => {
    vi.mocked(fetch).mockResolvedValue(json(200, planFixture))
    render(
      wrap(
        <PlanEditor initialDocument={document} plan={planFixture} onSaved={() => {}} />,
      ),
    )

    await userEvent.click(screen.getByRole("button", { name: /^save$/i }))

    await waitFor(() => expect(fetch).toHaveBeenCalled())
    const [url] = vi.mocked(fetch).mock.calls[0] ?? []
    expect(String(url)).toBe(
      `/api/v1/plans/${planFixture.name}?version=${planFixture.version}`,
    )
  })

  it("offers both ways out of a version conflict, and keeps what was typed", async () => {
    vi.mocked(fetch).mockResolvedValue(
      json(409, problem409VersionFixture, "application/problem+json"),
    )
    render(
      wrap(
        <PlanEditor initialDocument={document} plan={planFixture} onSaved={() => {}} />,
      ),
    )

    await userEvent.click(screen.getByRole("button", { name: /^save$/i }))

    expect(
      await screen.findByText(/changed while you were editing/i),
    ).toBeInTheDocument()
    expect(
      screen.getByRole("button", { name: /start from the server/i }),
    ).toBeInTheDocument()
    expect(screen.getByRole("button", { name: /overwrite/i })).toBeInTheDocument()
    // Nothing was thrown away.
    expect(screen.getByRole("textbox")).toHaveValue(document)
  })

  // Overwriting is possible and never accidental: it is a second, deliberate
  // request, and it drops the guard.
  it("overwrites without the guard, and only when asked", async () => {
    vi.mocked(fetch).mockResolvedValue(
      json(409, problem409VersionFixture, "application/problem+json"),
    )
    render(
      wrap(
        <PlanEditor initialDocument={document} plan={planFixture} onSaved={() => {}} />,
      ),
    )

    await userEvent.click(screen.getByRole("button", { name: /^save$/i }))
    await screen.findByRole("button", { name: /overwrite/i })
    await userEvent.click(screen.getByRole("button", { name: /overwrite/i }))

    await waitFor(() => expect(vi.mocked(fetch).mock.calls.length).toBe(2))
    const [url] = vi.mocked(fetch).mock.calls[1] ?? []
    expect(String(url)).toBe(`/api/v1/plans/${planFixture.name}`)
  })

  // Changing name: in the textarea is the most natural thing in the world,
  // and it is not somebody else's edit.
  it("explains a rename instead of calling it a conflict", async () => {
    vi.mocked(fetch).mockResolvedValue(
      json(409, renameProblem, "application/problem+json"),
    )
    render(
      wrap(
        <PlanEditor initialDocument={document} plan={planFixture} onSaved={() => {}} />,
      ),
    )

    await userEvent.click(screen.getByRole("button", { name: /^save$/i }))

    expect(await screen.findByText(/cannot be renamed in place/i)).toBeInTheDocument()
    expect(screen.queryByText(/changed while you were editing/i)).toBeNull()
  })

  // One request for a burst of typing: this endpoint parses a document on
  // every call.
  it("asks the server once for a burst of typing", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    try {
      vi.mocked(fetch).mockResolvedValue(json(200, validateOkFixture))
      render(wrap(<PlanEditor initialDocument="" onSaved={() => {}} />))

      const box = screen.getByRole("textbox")
      await userEvent.type(box, "name: x", { delay: null })
      await vi.advanceTimersByTimeAsync(600)

      await waitFor(() => expect(fetch).toHaveBeenCalledTimes(1))
      const [url] = vi.mocked(fetch).mock.calls[0] ?? []
      expect(url).toBe("/api/v1/plans/validate")
    } finally {
      vi.useRealTimers()
    }
  })
})

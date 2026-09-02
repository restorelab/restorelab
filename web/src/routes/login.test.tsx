import { QueryClient, QueryClientProvider } from "@tanstack/react-query"
import { render, screen, waitFor } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import type { ReactNode } from "react"
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"
import { LoginForm } from "./login"

function wrap(ui: ReactNode) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return <QueryClientProvider client={qc}>{ui}</QueryClientProvider>
}

function problem(status: number, body: Record<string, unknown>) {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "content-type": "application/problem+json" },
  })
}

describe("LoginForm", () => {
  beforeEach(() => vi.stubGlobal("fetch", vi.fn()))
  afterEach(() => vi.unstubAllGlobals())

  it("exchanges the pasted token for a session", async () => {
    vi.mocked(fetch).mockResolvedValue(
      new Response(JSON.stringify({ token_name: "ops", scopes: ["read"] }), {
        status: 201,
        headers: { "content-type": "application/json" },
      }),
    )
    const onDone = vi.fn()
    render(wrap(<LoginForm onSignedIn={onDone} />))

    await userEvent.type(screen.getByLabelText(/api token/i), "rl_secret")
    await userEvent.click(screen.getByRole("button", { name: /sign in/i }))

    await waitFor(() => expect(onDone).toHaveBeenCalled())
    const [url, init] = vi.mocked(fetch).mock.calls[0] ?? []
    expect(url).toBe("/api/v1/session")
    expect(JSON.parse(String((init as RequestInit).body))).toEqual({
      token: "rl_secret",
    })
  })

  it("cannot be submitted with an empty token", () => {
    render(wrap(<LoginForm onSignedIn={vi.fn()} />))
    expect(screen.getByRole("button", { name: /sign in/i })).toBeDisabled()
  })

  it("shows the server's refusal rather than a message of its own", async () => {
    vi.mocked(fetch).mockResolvedValue(
      problem(401, {
        type: "invalid-token",
        title: "That token is not valid",
        status: 401,
      }),
    )
    render(wrap(<LoginForm onSignedIn={vi.fn()} />))
    await userEvent.type(screen.getByLabelText(/api token/i), "wrong")
    await userEvent.click(screen.getByRole("button", { name: /sign in/i }))

    expect(await screen.findByText("That token is not valid")).toBeInTheDocument()
  })

  it("explains a 503 as a missing history database, because without one there are no tokens", async () => {
    vi.mocked(fetch).mockResolvedValue(
      problem(503, {
        type: "history-unavailable",
        title: "The drill history is unavailable",
        status: 503,
        detail:
          "this RestoreLab has no usable history database; see `restorelab db status`",
      }),
    )
    render(wrap(<LoginForm onSignedIn={vi.fn()} />))
    await userEvent.type(screen.getByLabelText(/api token/i), "any")
    await userEvent.click(screen.getByRole("button", { name: /sign in/i }))

    expect(await screen.findByText(/drill history is unavailable/i)).toBeInTheDocument()
    // and it hands over a command that exists, rather than the form again
    expect(screen.getByText("restorelab db status")).toBeInTheDocument()
    expect(screen.queryByLabelText(/api token/i)).toBeNull()
  })
})

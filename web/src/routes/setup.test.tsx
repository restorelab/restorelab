import { ApiError } from "@/api/client"
import { setupFailedFixture, setupResultFixture } from "@/api/fixtures"
import { QueryClient, QueryClientProvider } from "@tanstack/react-query"
import { render, screen, waitFor } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import type { ReactNode } from "react"
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"
import { SetupForm, setupRequest, stepsOf } from "./setup"

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

const fields = {
  endpoint: "https://192.0.2.10:8006",
  adminUser: "root@pam",
  adminPassword: "hunter2",
  storages: "local-zfs\n\n  local  \n",
  insecure: true,
  bridge: "create" as const,
}

describe("setupRequest", () => {
  it("reads one storage per line and drops the blank ones", () => {
    expect(setupRequest(fields).storages).toEqual(["local-zfs", "local"])
  })

  // The three bridge choices are the whole of what that screen decides, and
  // they ride in this one request: the setup token is spent by it, so there
  // is no second call to ask anything else in.
  it("turns the bridge choice into the two flags the server reads", () => {
    expect(setupRequest({ ...fields, bridge: "create" })).toMatchObject({
      create_bridge: true,
      apply_bridge: true,
    })
    expect(setupRequest({ ...fields, bridge: "defer" })).toMatchObject({
      create_bridge: true,
      apply_bridge: false,
    })
    expect(setupRequest({ ...fields, bridge: "skip" })).toMatchObject({
      create_bridge: false,
      apply_bridge: false,
    })
  })
})

describe("stepsOf", () => {
  it("finds the steps a refusal carried", () => {
    const err = new ApiError(setupFailedFixture, setupFailedFixture)
    expect(stepsOf(err)).toHaveLength(setupFailedFixture.steps?.length ?? 0)
  })

  it("has nothing to say about anything else", () => {
    expect(stepsOf(new Error("boom"))).toEqual([])
    expect(stepsOf(undefined)).toEqual([])
  })
})

describe("SetupForm", () => {
  beforeEach(() => vi.stubGlobal("fetch", vi.fn()))
  afterEach(() => vi.unstubAllGlobals())

  async function fill() {
    await userEvent.type(
      screen.getByLabelText(/cluster address/i),
      "https://192.0.2.10:8006",
    )
    await userEvent.type(screen.getByLabelText(/administrator password/i), "hunter2", {
      delay: null,
    })
    await userEvent.type(screen.getByLabelText(/storage for restores/i), "local-zfs", {
      delay: null,
    })
  }

  // Without a storage no real drill can run, and an installation that ends
  // there has failed while looking like it succeeded.
  it("will not submit without a storage", async () => {
    render(wrap(<SetupForm token="rls_t" onDone={() => {}} />))
    await userEvent.type(
      screen.getByLabelText(/cluster address/i),
      "https://192.0.2.10:8006",
    )
    await userEvent.type(screen.getByLabelText(/administrator password/i), "hunter2", {
      delay: null,
    })

    expect(screen.getByRole("button", { name: /connect the cluster/i })).toBeDisabled()
    expect(fetch).not.toHaveBeenCalled()
  })

  it("sends the setup token as a bearer, and the password only in the body", async () => {
    vi.mocked(fetch).mockResolvedValue(json(200, setupResultFixture))
    render(wrap(<SetupForm token="rls_t" onDone={() => {}} />))
    await fill()

    await userEvent.click(screen.getByRole("button", { name: /connect the cluster/i }))

    await waitFor(() => expect(fetch).toHaveBeenCalled())
    const [url, init] = vi.mocked(fetch).mock.calls[0] ?? []
    expect(url).toBe("/api/v1/setup")
    expect(String(url)).not.toContain("hunter2")
    const headers = new Headers((init as RequestInit).headers)
    expect(headers.get("authorization")).toBe("Bearer rls_t")
    expect(String((init as RequestInit).body)).toContain("hunter2")
  })

  it("hands the outcome up when it succeeds", async () => {
    vi.mocked(fetch).mockResolvedValue(json(200, setupResultFixture))
    const onDone = vi.fn()
    render(wrap(<SetupForm token="rls_t" onDone={onDone} />))
    await fill()

    await userEvent.click(screen.getByRole("button", { name: /connect the cluster/i }))

    await waitFor(() => expect(onDone).toHaveBeenCalled())
    expect(onDone.mock.calls[0]?.[0]).toMatchObject({ token: setupResultFixture.token })
  })

  it("shows how far a failed setup got, and says it can be run again", async () => {
    vi.mocked(fetch).mockResolvedValue(
      json(502, setupFailedFixture, "application/problem+json"),
    )
    render(wrap(<SetupForm token="rls_t" onDone={() => {}} />))
    await fill()

    await userEvent.click(screen.getByRole("button", { name: /connect the cluster/i }))

    expect(await screen.findByText(/create role RestoreLabDrill/)).toBeInTheDocument()
    expect(screen.getByText(/nothing here is created twice/i)).toBeInTheDocument()
    // And it says the console token is gone, because it is.
    expect(screen.getByText(/restart restorelab to get a new one/i)).toBeInTheDocument()
  })

  // The bridge is the only choice that touches a production node's network,
  // so the screen names the consequence before it happens.
  it("says what creating the isolated bridge does", () => {
    render(wrap(<SetupForm token="rls_t" onDone={() => {}} />))
    expect(screen.getByText(/touches no existing interface/i)).toBeInTheDocument()
    expect(screen.getByText(/network configuration is reloaded/i)).toBeInTheDocument()
    expect(screen.getByText(/reach nothing at all/i)).toBeInTheDocument()
  })
})

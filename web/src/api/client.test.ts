import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"
import {
  ApiError,
  UnauthorizedError,
  apiGet,
  apiSend,
  eventsUrl,
  reportUrl,
} from "./client"

function respond(status: number, body: unknown, contentType: string) {
  return new Response(typeof body === "string" ? body : JSON.stringify(body), {
    status,
    headers: { "content-type": contentType },
  })
}

/** Awaits a rejection and asserts it is one of ours, so the fields are typed. */
async function rejection(p: Promise<unknown>): Promise<ApiError> {
  const err = await p.then(
    () => null,
    (e: unknown) => e,
  )
  expect(err).toBeInstanceOf(ApiError)
  return err as ApiError
}

describe("apiGet", () => {
  beforeEach(() => {
    vi.stubGlobal("fetch", vi.fn())
  })
  afterEach(() => {
    vi.unstubAllGlobals()
  })

  it("returns the parsed body on success", async () => {
    vi.mocked(fetch).mockResolvedValue(respond(200, { items: [] }, "application/json"))
    await expect(apiGet("/recovery-runs")).resolves.toEqual({ items: [] })
  })

  it("sends the session cookie", async () => {
    vi.mocked(fetch).mockResolvedValue(respond(200, {}, "application/json"))
    await apiGet("/session")
    expect(vi.mocked(fetch).mock.calls[0]?.[1]).toMatchObject({
      credentials: "same-origin",
    })
  })

  it("prefixes every path with the versioned API root", async () => {
    vi.mocked(fetch).mockResolvedValue(respond(200, {}, "application/json"))
    await apiGet("/session")
    expect(vi.mocked(fetch).mock.calls[0]?.[0]).toBe("/api/v1/session")
  })

  it("turns problem+json into a typed ApiError", async () => {
    vi.mocked(fetch).mockResolvedValue(
      respond(
        404,
        {
          type: "no-such-run",
          title: "No such recovery run",
          status: 404,
          detail: "run 94bce70d is not in the history",
        },
        "application/problem+json",
      ),
    )
    const err = await rejection(apiGet("/recovery-runs/94bce70d"))
    expect(err.status).toBe(404)
    expect(err.type).toBe("no-such-run")
    expect(err.title).toBe("No such recovery run")
    expect(err.detail).toBe("run 94bce70d is not in the history")
  })

  it("wraps an error that is not problem+json in the same shape", async () => {
    vi.mocked(fetch).mockResolvedValue(respond(502, "upstream is down", "text/plain"))
    const err = await rejection(apiGet("/doctor"))
    expect(err.status).toBe(502)
    expect(err.title).toBeTruthy()
  })

  it("raises UnauthorizedError on 401, so the session layer can catch it alone", async () => {
    vi.mocked(fetch).mockResolvedValue(
      respond(
        401,
        { type: "unauthenticated", title: "Not authenticated", status: 401 },
        "application/problem+json",
      ),
    )
    const err = await rejection(apiGet("/session"))
    expect(err).toBeInstanceOf(UnauthorizedError)
  })

  it("reports a network failure as an ApiError rather than a raw TypeError", async () => {
    vi.mocked(fetch).mockRejectedValue(new TypeError("Failed to fetch"))
    const err = await rejection(apiGet("/session"))
    expect(err.status).toBe(0)
  })

  it("returns undefined for a 204", async () => {
    vi.mocked(fetch).mockResolvedValue(new Response(null, { status: 204 }))
    await expect(apiSend("DELETE", "/session")).resolves.toBeUndefined()
  })

  it("sends a JSON body when given one, and none when not", async () => {
    vi.mocked(fetch).mockResolvedValue(respond(201, {}, "application/json"))
    await apiSend("POST", "/session", { token: "rl_secret" })
    const init = vi.mocked(fetch).mock.calls[0]?.[1] as RequestInit
    expect(JSON.parse(String(init.body))).toEqual({ token: "rl_secret" })

    vi.mocked(fetch).mockClear()
    vi.mocked(fetch).mockResolvedValue(new Response(null, { status: 204 }))
    await apiSend("DELETE", "/session")
    expect((vi.mocked(fetch).mock.calls[0]?.[1] as RequestInit).body).toBeUndefined()
  })
})

describe("URL helpers", () => {
  it("escapes a run id rather than pasting it into a path", () => {
    expect(reportUrl("a/b")).toBe("/api/v1/recovery-runs/a%2Fb/report?format=html")
    expect(eventsUrl("a/b")).toBe("/api/v1/recovery-runs/a%2Fb/events")
  })
})

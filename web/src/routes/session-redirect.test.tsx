import { QueryClient, QueryClientProvider } from "@tanstack/react-query"
import {
  RouterProvider,
  createMemoryHistory,
  createRouter,
} from "@tanstack/react-router"
import { render, screen } from "@testing-library/react"
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"
import { routeTree } from "../routeTree.gen"

/**
 * The authenticated boundary is the one decision that lives in a route rather
 * than a component, so it is the one test in this suite that needs a router.
 */
function build(initialPath: string) {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  const router = createRouter({
    routeTree,
    context: { queryClient },
    history: createMemoryHistory({ initialEntries: [initialPath] }),
  })
  return { queryClient, router }
}

function show(initialPath: string) {
  const { queryClient, router } = build(initialPath)
  render(
    <QueryClientProvider client={queryClient}>
      <RouterProvider router={router} />
    </QueryClientProvider>,
  )
  return router
}

const UNAUTHENTICATED = () =>
  new Response(
    JSON.stringify({
      type: "unauthenticated",
      title: "Not authenticated",
      status: 401,
    }),
    { status: 401, headers: { "content-type": "application/problem+json" } },
  )

const SESSION = () =>
  new Response(
    JSON.stringify({
      token_name: "ops",
      scopes: ["read"],
      expires_at: "2026-09-03T00:00:00Z",
    }),
    { status: 200, headers: { "content-type": "application/json" } },
  )

const json = (body: unknown) =>
  new Response(JSON.stringify(body), {
    status: 200,
    headers: { "content-type": "application/json" },
  })

/**
 * An authenticated visitor, answered per endpoint.
 *
 * Answering every request with the session payload was simpler and quietly
 * wrong: the overview's five queries each got a body with no `items`, threw
 * inside their queryFn, and rendered through error boundaries. The shell
 * still appeared and the test still passed - through a storm of thrown
 * exceptions, which is what made it slow enough to start losing races under a
 * loaded suite. Empty but well-formed answers are both faster and closer to
 * what the boundary under test actually sees.
 */
const authenticated = async (input: RequestInfo | URL) => {
  const url = String(input instanceof Request ? input.url : input)
  if (url.includes("/session")) return SESSION()
  if (url.includes("/doctor")) {
    return json({ provider_id: "pve", ok: true, problems: 0, findings: [] })
  }
  return json({ items: [], next_cursor: null })
}

describe("the authenticated boundary", () => {
  beforeEach(() => vi.stubGlobal("fetch", vi.fn()))
  afterEach(() => vi.unstubAllGlobals())

  it("sends an unauthenticated visitor to the login screen", async () => {
    vi.mocked(fetch).mockImplementation(async () => UNAUTHENTICATED())
    const router = show("/")
    expect(await screen.findByLabelText(/api token/i)).toBeInTheDocument()
    expect(router.state.location.pathname).toBe("/login")
  })

  it("remembers where the visitor was going", async () => {
    vi.mocked(fetch).mockImplementation(async () => UNAUTHENTICATED())
    const router = show("/")
    await screen.findByLabelText(/api token/i)
    expect(router.state.location.search).toMatchObject({ redirect: "/" })
  })

  it("lets an authenticated visitor through to the shell", async () => {
    vi.mocked(fetch).mockImplementation(authenticated)
    show("/")
    // The default one second is a bet, not an assertion: this is the only
    // test here that boots a router, resolves the overview's five suspense
    // queries and renders the shell. What is being checked is the boundary,
    // never the speed.
    expect(
      await screen.findByRole("button", { name: /sign out/i }, { timeout: 5000 }),
    ).toBeInTheDocument()
  })
})

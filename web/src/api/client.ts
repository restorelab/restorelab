import type { Problem } from "./types"

const BASE = "/api/v1"

/**
 * Every failure this client raises, in one shape.
 *
 * A caller must never have two error cases to handle - one for problem+json
 * and one for everything else - so a plain-text 502 and a dropped connection
 * are wrapped into the same fields the API's own documents carry.
 */
export class ApiError extends Error {
  readonly type: string
  readonly title: string
  readonly status: number
  readonly detail?: string

  constructor(p: Problem) {
    super(p.detail ? `${p.title}: ${p.detail}` : p.title)
    this.name = "ApiError"
    this.type = p.type
    this.title = p.title
    this.status = p.status
    this.detail = p.detail
  }
}

/**
 * A 401, and only a 401.
 *
 * It is its own class so the session layer can catch exactly this and send the
 * viewer back to the login screen, without inspecting status codes in a dozen
 * components.
 */
export class UnauthorizedError extends ApiError {
  constructor(p: Problem) {
    super(p)
    this.name = "UnauthorizedError"
  }
}

async function toProblem(res: Response): Promise<Problem> {
  const type = res.headers.get("content-type") ?? ""
  if (type.includes("problem+json") || type.includes("application/json")) {
    try {
      const body = (await res.json()) as Partial<Problem>
      if (body && typeof body.title === "string") {
        return {
          type: body.type ?? "about:blank",
          title: body.title,
          status: body.status ?? res.status,
          detail: body.detail,
          instance: body.instance,
        }
      }
    } catch {
      // A body that will not parse is not worth a second error on top of the
      // first: fall through to the status line.
    }
  }
  return {
    type: "about:blank",
    title: res.statusText || `Request failed with status ${res.status}`,
    status: res.status,
  }
}

/**
 * Sends, and raises the one error shape on failure.
 *
 * request() and apiSendWithStatus() are both built on this, so there is one
 * place that turns a 401 into an UnauthorizedError and no second copy of the
 * problem+json handling to keep in step.
 */
async function send(path: string, init: RequestInit): Promise<Response> {
  let res: Response
  try {
    res = await fetch(BASE + path, {
      // The session cookie is __Host-prefixed and HttpOnly. There is no token
      // to carry: this one line is the whole of the client's authentication.
      credentials: "same-origin",
      ...init,
    })
  } catch (cause) {
    throw new ApiError({
      type: "network-error",
      title: "The server could not be reached",
      status: 0,
      detail: cause instanceof Error ? cause.message : undefined,
    })
  }

  if (!res.ok) {
    const problem = await toProblem(res)
    throw problem.status === 401
      ? new UnauthorizedError(problem)
      : new ApiError(problem)
  }
  return res
}

/** Reads a successful response's body, or undefined when it has none. */
async function decode<T>(res: Response): Promise<T> {
  if (res.status === 204 || res.headers.get("content-length") === "0") {
    return undefined as T
  }
  return (await res.json()) as T
}

async function request<T>(path: string, init: RequestInit): Promise<T> {
  return decode<T>(await send(path, init))
}

/** GET a JSON resource. */
export function apiGet<T>(path: string, init?: RequestInit): Promise<T> {
  return request<T>(path, { method: "GET", ...init })
}

/**
 * POST or DELETE.
 *
 * The browser sends Origin on its own, and the API's CSRF guard compares it to
 * Host - which is why the dev-server proxy has to rewrite that header. Nothing
 * to set here.
 */
export function apiSend<T>(
  method: "POST" | "DELETE",
  path: string,
  body?: unknown,
): Promise<T> {
  return request<T>(path, {
    method,
    ...(body === undefined
      ? {}
      : {
          headers: { "content-type": "application/json" },
          body: JSON.stringify(body),
        }),
  })
}

/**
 * POST or DELETE, keeping the status.
 *
 * For the one route whose two successes are different states of the world:
 * cancelling a queued drill ends it (200), while cancelling a running one only
 * asks (202) and a worker is still tearing the temporary workload down. A
 * caller that reported "done" on a 202 would announce a machine gone while it
 * still exists.
 */
export async function apiSendWithStatus<T>(
  method: "POST" | "DELETE",
  path: string,
  body?: unknown,
): Promise<{ status: number; data: T }> {
  const res = await send(path, {
    method,
    ...(body === undefined
      ? {}
      : {
          headers: { "content-type": "application/json" },
          body: JSON.stringify(body),
        }),
  })
  return { status: res.status, data: await decode<T>(res) }
}

/** The URL of a run's HTML report, for a download link. */
export function reportUrl(runId: string): string {
  return `${BASE}/recovery-runs/${encodeURIComponent(runId)}/report?format=html`
}

/** The URL of a run's event stream, for EventSource. */
export function eventsUrl(runId: string): string {
  return `${BASE}/recovery-runs/${encodeURIComponent(runId)}/events`
}

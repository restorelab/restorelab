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

  /**
   * The problem document as it arrived.
   *
   * A problem+json may carry fields beyond the five every problem has, and
   * throwing them away costs the caller the useful half: first-run setup
   * answers its refusals with the provisioning steps it got through, and a
   * screen that only had the message would show a dead end instead of how
   * far it got. Kept as unknown so this file learns nothing about any one
   * endpoint's extras.
   */
  readonly body?: unknown

  constructor(p: Problem, body?: unknown) {
    super(p.detail ? `${p.title}: ${p.detail}` : p.title)
    this.name = "ApiError"
    this.type = p.type
    this.title = p.title
    this.status = p.status
    this.detail = p.detail
    this.body = body
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
  constructor(p: Problem, body?: unknown) {
    super(p, body)
    this.name = "UnauthorizedError"
  }
}

async function toProblem(res: Response): Promise<[Problem, unknown]> {
  const type = res.headers.get("content-type") ?? ""
  if (type.includes("problem+json") || type.includes("application/json")) {
    try {
      const body = (await res.json()) as Partial<Problem>
      if (body && typeof body.title === "string") {
        return [
          {
            type: body.type ?? "about:blank",
            title: body.title,
            status: body.status ?? res.status,
            detail: body.detail,
            instance: body.instance,
          },
          body,
        ]
      }
    } catch {
      // A body that will not parse is not worth a second error on top of the
      // first: fall through to the status line.
    }
  }
  return [
    {
      type: "about:blank",
      title: res.statusText || `Request failed with status ${res.status}`,
      status: res.status,
    },
    undefined,
  ]
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
    const [problem, body] = await toProblem(res)
    throw problem.status === 401
      ? new UnauthorizedError(problem, body)
      : new ApiError(problem, body)
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

/**
 * POST carrying a bearer token of its own.
 *
 * Everything else this client sends is authenticated by the session cookie,
 * which the browser attaches without being asked. First-run setup is the one
 * exception: there is no session yet, and there cannot be - the token it
 * carries was printed on a console and is spent by this very request.
 *
 * A separate function rather than an options bag on apiSend, so the ordinary
 * path stays the obvious one and this exception stays visible.
 */
export function apiSendWithToken<T>(
  path: string,
  token: string,
  body: unknown,
): Promise<T> {
  return request<T>(path, {
    method: "POST",
    headers: {
      "content-type": "application/json",
      authorization: `Bearer ${token}`,
    },
    body: JSON.stringify(body),
  })
}

/**
 * POST or PUT of a plan document, sent verbatim.
 *
 * The catalogue routes read the request body as the YAML itself, not as a
 * JSON envelope around it. Sending it through apiSend would wrap it in quotes
 * and escape its newlines, and the server would refuse a document nobody
 * wrote that way.
 *
 * It is a separate function rather than a wider apiSend because the
 * difference is the body's nature, not the method: everything else this
 * client sends is JSON, and that should stay the obvious path.
 */
export function apiSendText<T>(
  method: "POST" | "PUT",
  path: string,
  body: string,
): Promise<T> {
  return request<T>(path, {
    method,
    headers: { "content-type": "application/yaml" },
    body,
  })
}

/** The URL of a run's HTML report, for a download link. */
export function reportUrl(runId: string): string {
  return `${BASE}/recovery-runs/${encodeURIComponent(runId)}/report?format=html`
}

/** The URL of a run's event stream, for EventSource. */
export function eventsUrl(runId: string): string {
  return `${BASE}/recovery-runs/${encodeURIComponent(runId)}/events`
}

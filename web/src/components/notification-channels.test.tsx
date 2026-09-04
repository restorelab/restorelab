import {
  first,
  notificationCreatedFixture,
  notificationTestFixture,
  notificationsPageFixture,
} from "@/api/fixtures"
import { QueryClient, QueryClientProvider } from "@tanstack/react-query"
import { render, screen, waitFor } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import type { ReactNode } from "react"
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"
import { NotificationChannels } from "./notification-channels"

function wrap(ui: ReactNode) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return <QueryClientProvider client={qc}>{ui}</QueryClientProvider>
}

const channel = first(notificationsPageFixture.items, "notifications page")

/** The body of the nth request this test provoked, parsed. */
function sentBody(call: number): Record<string, unknown> {
  const [, init] = vi.mocked(fetch).mock.calls[call] ?? []
  return JSON.parse(String((init as RequestInit).body)) as Record<string, unknown>
}

function problem(status: number, detail: string): Response {
  return new Response(
    JSON.stringify({ type: "about:blank", title: "Refused", status, detail }),
    { status, headers: { "content-type": "application/problem+json" } },
  )
}

function answer(body: unknown, status: number): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "content-type": "application/json" },
  })
}

function clickFirst(name: RegExp) {
  const buttons = screen.getAllByRole("button", { name })
  return userEvent.click(buttons[0] as HTMLElement)
}

function view(canManage: boolean, canOperate: boolean) {
  return render(
    wrap(
      <NotificationChannels
        channels={notificationsPageFixture}
        canManage={canManage}
        canOperate={canOperate}
      />,
    ),
  )
}

describe("NotificationChannels", () => {
  beforeEach(() => vi.stubGlobal("fetch", vi.fn()))
  afterEach(() => vi.unstubAllGlobals())

  it("names each channel by its kind and its destination", () => {
    view(true, true)
    for (const c of notificationsPageFixture.items) {
      expect(screen.getByText(c.id)).toBeInTheDocument()
      expect(screen.getByText(c.host)).toBeInTheDocument()
    }
    expect(screen.getByText(/^discord$/i)).toBeInTheDocument()
    expect(screen.getByText(/^slack$/i)).toBeInTheDocument()
  })

  it("explains an empty list rather than showing an empty table", () => {
    render(wrap(<NotificationChannels channels={{ items: [] }} canManage canOperate />))
    expect(screen.getByText(/no channel yet/i)).toBeInTheDocument()
  })

  // A channel that quietly stopped working leaves an operator believing they
  // are being watched. The row has to say so where the row is.
  it("says out loud that a channel's last delivery failed", () => {
    view(true, true)
    expect(screen.getByText(/failed with 404/i)).toBeInTheDocument()
    expect(screen.getByText(/no_service/i)).toBeInTheDocument()
  })

  it("creates a channel, sending the url in the body", async () => {
    vi.mocked(fetch).mockResolvedValue(answer(notificationCreatedFixture, 201))
    view(true, true)

    await userEvent.click(screen.getByRole("button", { name: /add a channel/i }))
    await userEvent.type(screen.getByLabelText(/^id$/i), "ops-webhook")
    await userEvent.selectOptions(screen.getByLabelText(/^kind$/i), "webhook")
    await userEvent.type(
      screen.getByLabelText(/webhook url/i),
      "https://example.test/hooks/abc",
    )
    await userEvent.click(screen.getByRole("button", { name: /^save$/i }))

    await waitFor(() => expect(fetch).toHaveBeenCalled())
    const [url, init] = vi.mocked(fetch).mock.calls[0] ?? []
    expect(url).toBe("/api/v1/notifications")
    expect((init as RequestInit).method).toBe("POST")
    expect(sentBody(0)).toMatchObject({
      id: "ops-webhook",
      kind: "webhook",
      url: "https://example.test/hooks/abc",
    })
  })

  // The trap this screen exists to avoid. The API never hands a webhook URL
  // back, so the field is always blank when an edit opens, and sending that
  // blank would wipe a channel that works.
  it("omits the url entirely when the field was left alone", async () => {
    vi.mocked(fetch).mockResolvedValue(answer(notificationCreatedFixture, 200))
    view(true, true)

    await clickFirst(/^edit$/i)
    expect(screen.getByLabelText(/webhook url/i)).toHaveValue("")
    await userEvent.click(screen.getByRole("button", { name: /^save$/i }))

    await waitFor(() => expect(fetch).toHaveBeenCalled())
    const [url, init] = vi.mocked(fetch).mock.calls[0] ?? []
    expect(url).toBe(`/api/v1/notifications/${channel.id}`)
    expect((init as RequestInit).method).toBe("PUT")
    expect(sentBody(0)).not.toHaveProperty("url")
  })

  it("sends the url on an edit only when one was typed", async () => {
    vi.mocked(fetch).mockResolvedValue(answer(notificationCreatedFixture, 200))
    view(true, true)

    await clickFirst(/^edit$/i)
    await userEvent.type(
      screen.getByLabelText(/webhook url/i),
      "https://example.test/new",
    )
    await userEvent.click(screen.getByRole("button", { name: /^save$/i }))

    await waitFor(() => expect(fetch).toHaveBeenCalled())
    expect(sentBody(0)).toMatchObject({ url: "https://example.test/new" })
  })

  // The URL is a bearer credential: typed once, never displayed back.
  it("hides the url and says a blank field keeps the stored one", async () => {
    view(true, true)

    await clickFirst(/^edit$/i)
    const field = screen.getByLabelText(/webhook url/i)
    expect(field).toHaveAttribute("type", "password")
    expect(field.getAttribute("placeholder")).toMatch(/keeps the stored url/i)
  })

  // Nobody trusts an alerting path they have never seen fire.
  it("sends one message on purpose and reports what the far end answered", async () => {
    vi.mocked(fetch).mockResolvedValue(answer(notificationTestFixture, 200))
    view(true, true)

    await clickFirst(/^test$/i)

    await waitFor(() => expect(fetch).toHaveBeenCalled())
    const [url, init] = vi.mocked(fetch).mock.calls[0] ?? []
    expect(url).toBe(`/api/v1/notifications/${channel.id}/test`)
    expect((init as RequestInit).method).toBe("POST")
    expect(
      await screen.findByText(new RegExp(String(notificationTestFixture.status))),
    ).toBeInTheDocument()
  })

  it("shows a refused test beside the channel that refused", async () => {
    vi.mocked(fetch).mockResolvedValue(
      problem(502, "the channel answered Not Found: unknown_webhook"),
    )
    view(true, true)

    await clickFirst(/^test$/i)

    expect(await screen.findByText(/unknown_webhook/i)).toBeInTheDocument()
  })

  it("shows a refused save inline", async () => {
    vi.mocked(fetch).mockResolvedValue(
      problem(409, "a channel called ops already exists"),
    )
    view(true, true)

    await userEvent.click(screen.getByRole("button", { name: /add a channel/i }))
    await userEvent.type(screen.getByLabelText(/^id$/i), "ops")
    await userEvent.type(
      screen.getByLabelText(/webhook url/i),
      "https://example.test/x",
    )
    await userEvent.click(screen.getByRole("button", { name: /^save$/i }))

    expect(await screen.findByText(/already exists/i)).toBeInTheDocument()
  })

  // What a session cannot do is absent, never a disabled control: a greyed
  // button sends somebody looking for the fault in the wrong place.
  it("offers writing only with manage, and says why not", () => {
    view(false, true)
    expect(screen.queryByRole("button", { name: /add a channel/i })).toBeNull()
    expect(screen.queryByRole("button", { name: /^edit$/i })).toBeNull()
    expect(screen.queryByRole("button", { name: /^remove$/i })).toBeNull()
    expect(screen.getByText(/but not change them/i)).toBeInTheDocument()
  })

  it("offers the test send only with operate", () => {
    view(true, false)
    expect(screen.queryByRole("button", { name: /^test$/i })).toBeNull()
  })
})

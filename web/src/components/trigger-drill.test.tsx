import { backupsFixture, first, workloadsPageFixture } from "@/api/fixtures"
import { QueryClient, QueryClientProvider } from "@tanstack/react-query"
import { render, screen, waitFor } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import type { ReactNode } from "react"
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"
import { TriggerDrill, triggerBody } from "./trigger-drill"

const workload = first(workloadsPageFixture.items, "workloads")
const backups = backupsFixture.items

function wrap(ui: ReactNode) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return <QueryClientProvider client={qc}>{ui}</QueryClientProvider>
}

function json(status: number, body: unknown) {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "content-type": "application/json" },
  })
}

describe("triggerBody", () => {
  // A field left alone is left out entirely rather than sent empty: the server
  // resolves the latest backup, the isolated network and the configured node,
  // storage and pool, and it resolves them better than this form can guess.
  it("sends the workload and nothing else when no option was touched", () => {
    expect(
      triggerBody("110", {
        backup: "",
        checks: "",
        network: "",
        node: "",
        storage: "",
        pool: "",
        rto_target: "",
      }),
    ).toEqual({ workload_id: "110" })
  })

  it("sends only the options that were filled in", () => {
    expect(
      triggerBody("110", {
        backup: "",
        checks: "",
        network: "vmbr99",
        node: "",
        storage: "",
        pool: "",
        rto_target: "5m",
      }),
    ).toEqual({ workload_id: "110", network: "vmbr99", rto_target: "5m" })
  })

  it("reads one check per line and drops the blank ones", () => {
    expect(
      triggerBody("110", {
        backup: "",
        checks: "cmd:hostname\n\n  cmd:systemctl is-active ssh  \n",
        network: "",
        node: "",
        storage: "",
        pool: "",
        rto_target: "",
      }).checks,
    ).toEqual(["cmd:hostname", "cmd:systemctl is-active ssh"])
  })
})

describe("TriggerDrill", () => {
  beforeEach(() => vi.stubGlobal("fetch", vi.fn()))
  afterEach(() => vi.unstubAllGlobals())

  it("posts the workload and nothing else when no option is touched", async () => {
    vi.mocked(fetch).mockResolvedValue(json(201, { id: "new-run" }))
    const onStarted = vi.fn()
    render(
      wrap(
        <TriggerDrill
          workload={workload}
          backups={backups}
          canOperate
          onStarted={onStarted}
        />,
      ),
    )

    await userEvent.click(screen.getByRole("button", { name: /run a drill/i }))
    await userEvent.click(screen.getByRole("button", { name: /^start$/i }))

    await waitFor(() => expect(onStarted).toHaveBeenCalledWith("new-run"))
    const [url, init] = vi.mocked(fetch).mock.calls[0] ?? []
    expect(url).toBe("/api/v1/recovery-runs")
    expect(JSON.parse(String((init as RequestInit).body))).toEqual({
      workload_id: workload.id,
    })
  })

  // The panel is the confirmation. It has to say what it will do before it
  // does it, or the button is a trap.
  it("names the backup it will restore", async () => {
    render(
      wrap(
        <TriggerDrill
          workload={workload}
          backups={backups}
          canOperate
          onStarted={() => {}}
        />,
      ),
    )
    await userEvent.click(screen.getByRole("button", { name: /run a drill/i }))
    expect(
      screen.getByText(new RegExp(first(backups, "backups").id, "i")),
    ).toBeInTheDocument()
  })

  // A listing cannot afford one backups request per row, so it passes none.
  // The panel must still say what it is about to do.
  it("says it restores the most recent backup when it was given none", async () => {
    render(
      wrap(
        <TriggerDrill
          workload={workload}
          backups={[]}
          canOperate
          onStarted={() => {}}
        />,
      ),
    )
    await userEvent.click(screen.getByRole("button", { name: /run a drill/i }))
    expect(screen.getByText(/most recent backup/i)).toBeInTheDocument()
  })

  // Without the scope, no button at all - not a disabled one. A dead control
  // nobody explains sends people looking for the fault in the wrong place.
  it("renders nothing without the operate scope", () => {
    const { container } = render(
      wrap(
        <TriggerDrill
          workload={workload}
          backups={backups}
          canOperate={false}
          onStarted={() => {}}
        />,
      ),
    )
    expect(container).toBeEmptyDOMElement()
  })

  // The backend refuses a second drill with a 409. The UI does not wait for
  // that refusal: it points at the drill that is already running.
  it("points at the running drill instead of offering to start another", () => {
    render(
      wrap(
        <TriggerDrill
          workload={workload}
          backups={backups}
          canOperate
          activeRunID="run-in-flight"
          onStarted={() => {}}
        />,
      ),
    )
    expect(
      screen.queryByRole("button", { name: /run a drill/i }),
    ).not.toBeInTheDocument()
    expect(screen.getByText(/already running/i)).toBeInTheDocument()
  })

  // And when the race happens anyway - two tabs - the 409 renders as a way
  // forward, not as a raw error.
  it("turns a 409 into something readable, without closing the panel", async () => {
    vi.mocked(fetch).mockResolvedValue(
      new Response(
        JSON.stringify({
          type: "already-running",
          title: "This workload already has a drill in flight",
          status: 409,
          detail: "run other-run is queued or running for workload 110",
        }),
        { status: 409, headers: { "content-type": "application/problem+json" } },
      ),
    )
    render(
      wrap(
        <TriggerDrill
          workload={workload}
          backups={backups}
          canOperate
          onStarted={() => {}}
        />,
      ),
    )

    await userEvent.click(screen.getByRole("button", { name: /run a drill/i }))
    await userEvent.click(screen.getByRole("button", { name: /^start$/i }))

    expect(
      await screen.findByText(/already has a drill in flight/i),
    ).toBeInTheDocument()
    expect(screen.getByRole("button", { name: /^start$/i })).toBeInTheDocument()
  })
})

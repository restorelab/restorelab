import { act, renderHook } from "@testing-library/react"
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"
import { useRunStream } from "./useRunStream"

class FakeEventSource {
  static last: FakeEventSource | null = null
  readonly listeners = new Map<string, (e: MessageEvent) => void>()
  closed = false
  constructor(readonly url: string) {
    FakeEventSource.last = this
  }
  addEventListener(type: string, fn: (e: MessageEvent) => void) {
    this.listeners.set(type, fn)
  }
  close() {
    this.closed = true
  }
  emit(type: string, data: unknown) {
    this.listeners.get(type)?.({ data: JSON.stringify(data) } as MessageEvent)
  }
}

describe("useRunStream", () => {
  beforeEach(() => {
    FakeEventSource.last = null
    vi.stubGlobal("EventSource", FakeEventSource)
  })
  afterEach(() => vi.unstubAllGlobals())

  it("opens no stream when disabled, so a finished run costs nothing", () => {
    renderHook(() => useRunStream("r1", false))
    expect(FakeEventSource.last).toBeNull()
  })

  it("points the stream at the run's own events route", () => {
    renderHook(() => useRunStream("r1", true))
    expect(FakeEventSource.last?.url).toBe("/api/v1/recovery-runs/r1/events")
  })

  it("folds progress frames into the state", () => {
    const { result } = renderHook(() => useRunStream("r1", true))
    act(() => {
      FakeEventSource.last?.emit("progress", {
        seq: 1,
        at: "2026-09-02T12:00:00Z",
        state: "RESTORING",
        step: "restore",
        status: "running",
      })
    })
    expect(result.current.state).toBe("RESTORING")
    expect(result.current.steps.get("restore")?.status).toBe("running")
  })

  it("closes the stream on done", () => {
    const { result } = renderHook(() => useRunStream("r1", true))
    act(() => FakeEventSource.last?.emit("done", { state: "SUCCESS" }))
    expect(result.current.finished).toBe(true)
    expect(FakeEventSource.last?.closed).toBe(true)
  })

  it("does NOT close the stream on disconnected, and does not call the run finished", () => {
    const { result } = renderHook(() => useRunStream("r1", true))
    act(() =>
      FakeEventSource.last?.emit("disconnected", {
        state: "RESTORING",
        reason: "the server is shutting down",
      }),
    )
    expect(result.current.disconnected).toBe(true)
    expect(result.current.finished).toBe(false)
  })

  it("closes the stream when the component goes away", () => {
    const { unmount } = renderHook(() => useRunStream("r1", true))
    unmount()
    expect(FakeEventSource.last?.closed).toBe(true)
  })
})

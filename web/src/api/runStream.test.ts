import { describe, expect, it } from "vitest"
import { type StreamEvent, initialStreamState, reduce } from "./runStream"
import type { Check, ProgressFrame } from "./types"

function progress(over: Partial<ProgressFrame>): StreamEvent {
  return {
    kind: "progress",
    frame: {
      seq: 1,
      at: "2026-09-02T12:00:00Z",
      state: "RESTORING",
      ...over,
    },
  }
}

function play(...events: StreamEvent[]) {
  return events.reduce(reduce, initialStreamState)
}

describe("reduce", () => {
  it("starts empty", () => {
    expect(initialStreamState.state).toBeNull()
    expect(initialStreamState.finished).toBe(false)
    expect(initialStreamState.disconnected).toBe(false)
    expect(initialStreamState.lastSeq).toBe(0)
  })

  it("tracks the current state and the last sequence number", () => {
    const s = play(progress({ seq: 3, state: "RESTORING" }))
    expect(s.state).toBe("RESTORING")
    expect(s.lastSeq).toBe(3)
  })

  it("records a step and its status", () => {
    const s = play(progress({ seq: 1, step: "restore", status: "running" }))
    expect(s.steps.get("restore")).toMatchObject({
      name: "restore",
      status: "running",
    })
  })

  it("lets a later frame move a step from running to done", () => {
    const s = play(
      progress({ seq: 1, step: "restore", status: "running" }),
      progress({ seq: 2, step: "restore", status: "done" }),
    )
    expect(s.steps.get("restore")?.status).toBe("done")
    expect(s.steps.size).toBe(1)
  })

  it("collects checks in arrival order", () => {
    const check = (name: string): Check => ({
      name,
      type: "http",
      status: "pass",
      pass: true,
      started_at: "2026-09-02T12:00:00Z",
      completed_at: "2026-09-02T12:00:01Z",
      duration_seconds: 1,
      duration: "1s",
      attempts: 1,
    })
    const s = play(
      progress({ seq: 1, state: "RUNNING_CHECKS", check: check("ssh") }),
      progress({ seq: 2, state: "RUNNING_CHECKS", check: check("http") }),
    )
    expect(s.checks.map((c) => c.name)).toEqual(["ssh", "http"])
  })

  it("ignores a frame it has already seen, so a replayed stream is idempotent", () => {
    const s = play(
      progress({ seq: 2, step: "restore", status: "done" }),
      progress({ seq: 1, step: "restore", status: "running" }),
    )
    expect(s.lastSeq).toBe(2)
    expect(s.steps.get("restore")?.status).toBe("done")
  })

  it("keeps an error message off a frame", () => {
    const s = play(progress({ seq: 1, state: "FAILED", error: "guest never booted" }))
    expect(s.error).toBe("guest never booted")
  })

  // --------------------------------------------------------------------------
  // The whole reason this module is a pure function.
  // --------------------------------------------------------------------------

  it("marks the run finished on done", () => {
    const s = play(progress({ seq: 1 }), { kind: "done", frame: { state: "SUCCESS" } })
    expect(s.finished).toBe(true)
    expect(s.disconnected).toBe(false)
    expect(s.state).toBe("SUCCESS")
  })

  it("does NOT mark the run finished on disconnected", () => {
    const s = play(progress({ seq: 1, state: "RESTORING" }), {
      kind: "disconnected",
      frame: { state: "RESTORING", reason: "the server is shutting down" },
    })
    expect(s.disconnected).toBe(true)
    expect(s.finished).toBe(false)
    expect(s.state).toBe("RESTORING")
  })

  it("treats disconnected on a terminal state as finished all the same", () => {
    const s = play({
      kind: "disconnected",
      frame: { state: "SUCCESS", reason: "the server is shutting down" },
    })
    expect(s.disconnected).toBe(true)
    expect(s.finished).toBe(true)
  })

  it("clears the disconnected flag when the stream comes back", () => {
    const s = play(
      { kind: "disconnected", frame: { state: "RESTORING", reason: "gone" } },
      progress({ seq: 2, state: "STARTING" }),
    )
    expect(s.disconnected).toBe(false)
    expect(s.state).toBe("STARTING")
  })

  it("never mutates the state it was given", () => {
    const before = initialStreamState
    const after = reduce(
      before,
      progress({ seq: 1, step: "restore", status: "running" }),
    )
    expect(before.steps.size).toBe(0)
    expect(after.steps.size).toBe(1)
    expect(after).not.toBe(before)
  })
})

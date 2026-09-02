import {
  type Check,
  type DisconnectedFrame,
  type DoneFrame,
  type ProgressFrame,
  type RunState,
  type StepStatus,
  isTerminal,
} from "./types"

/**
 * The live view of a drill, folded from its event stream.
 *
 * This module is a pure function on purpose, and the purpose is one line
 * below: `event: disconnected` says the connection is ending, `event: done`
 * says the drill is. Confusing them marks a running drill as finished, and it
 * is the most expensive regression this slice could introduce. A pure reducer
 * makes that distinction a table test with no browser, no component and no
 * EventSource in sight.
 */

export interface StreamStep {
  name: string
  status: StepStatus
  at: string
  message?: string
  error?: string
}

export interface StreamState {
  /** The drill's own state, as of the last frame. */
  state: RunState | null
  /** The highest sequence number seen. Replayed frames below it are dropped. */
  lastSeq: number
  steps: Map<string, StreamStep>
  checks: Check[]
  messages: ProgressFrame[]
  /** The drill has ended. */
  finished: boolean
  /** The connection has ended. Says nothing about the drill. */
  disconnected: boolean
  error?: string
}

export const initialStreamState: StreamState = {
  state: null,
  lastSeq: 0,
  steps: new Map(),
  checks: [],
  messages: [],
  finished: false,
  disconnected: false,
}

export type StreamEvent =
  | { kind: "progress"; frame: ProgressFrame }
  | { kind: "done"; frame: DoneFrame }
  | { kind: "disconnected"; frame: DisconnectedFrame }

export function reduce(state: StreamState, event: StreamEvent): StreamState {
  switch (event.kind) {
    case "progress": {
      const f = event.frame
      // The API replays from Last-Event-ID on reconnect, so frames already
      // folded in arrive again. Sequence numbers make that free to ignore.
      if (f.seq <= state.lastSeq) return state

      const steps = new Map(state.steps)
      if (f.step && f.status) {
        steps.set(f.step, {
          name: f.step,
          status: f.status,
          at: f.at,
          message: f.message,
          error: f.error,
        })
      }

      return {
        ...state,
        state: f.state,
        lastSeq: f.seq,
        steps,
        checks: f.check ? [...state.checks, f.check] : state.checks,
        messages: [...state.messages, f],
        // A frame arriving is proof the connection is alive again.
        disconnected: false,
        finished: isTerminal(f.state),
        error: f.error ?? state.error,
      }
    }

    case "done":
      // The drill ended. This is the only event that finishes a run outright.
      return {
        ...state,
        state: event.frame.state,
        finished: true,
        disconnected: false,
      }

    case "disconnected":
      // The connection ended. The drill is still going unless its own state
      // says otherwise - which it can, if the server stopped just after the
      // run finished.
      return {
        ...state,
        state: event.frame.state,
        disconnected: true,
        finished: isTerminal(event.frame.state),
      }
  }
}

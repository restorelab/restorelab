import { eventsUrl } from "@/api/client"
import { type StreamState, initialStreamState, reduce } from "@/api/runStream"
import type { DisconnectedFrame, DoneFrame, ProgressFrame } from "@/api/types"
import { useEffect, useReducer } from "react"

/**
 * Follows one drill's event stream.
 *
 * All the judgement lives in runStream.reduce, which is pure and tested on its
 * own; this hook only owns the EventSource - opening it, translating three
 * named events into reducer actions, and closing it. EventSource is used
 * rather than fetch because the session cookie goes with it for free, and
 * because reconnection, backoff and Last-Event-ID replay come with it too.
 */
export function useRunStream(runId: string, enabled: boolean): StreamState {
  const [state, dispatch] = useReducer(reduce, initialStreamState)

  useEffect(() => {
    if (!enabled) return
    const source = new EventSource(eventsUrl(runId))

    const onProgress = (e: MessageEvent) => {
      dispatch({ kind: "progress", frame: JSON.parse(e.data) as ProgressFrame })
    }
    // done means the drill ended, so there is nothing left to listen for.
    const onDone = (e: MessageEvent) => {
      dispatch({ kind: "done", frame: JSON.parse(e.data) as DoneFrame })
      source.close()
    }
    // disconnected means the connection ended and the drill has not. The
    // source stays open so EventSource's own retry can bring it back, and the
    // reducer keeps `finished` false.
    const onDisconnected = (e: MessageEvent) => {
      const frame = JSON.parse(e.data) as DisconnectedFrame
      dispatch({ kind: "disconnected", frame })
    }

    source.addEventListener("progress", onProgress)
    source.addEventListener("done", onDone)
    source.addEventListener("disconnected", onDisconnected)

    return () => source.close()
  }, [runId, enabled])

  return state
}

import { apiSend, apiSendWithStatus } from "@/api/client"
import type { CleanupResult, RunSummary } from "@/api/types"
import { useMutation, useQueryClient } from "@tanstack/react-query"

/** What a drill is asked for, ad-hoc: a workload, and whatever else differs. */
export interface TriggerBody {
  workload_id: string
  backup?: string
  checks?: string[]
  network?: string
  node?: string
  storage?: string
  pool?: string
  rto_target?: string
  skip_startup?: boolean
}

/**
 * Queues a drill.
 *
 * The queue and the runs listing are both invalidated; the run itself is not,
 * because the caller navigates to it and its own query fetches it fresh.
 */
export function useTriggerDrill() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (body: TriggerBody) =>
      apiSend<RunSummary>("POST", "/recovery-runs", body),
    onSuccess: async () => {
      await qc.invalidateQueries({ queryKey: ["queue"] })
      await qc.invalidateQueries({ queryKey: ["runs"] })
    },
  })
}

/**
 * Asks a drill to stop.
 *
 * The status is kept rather than thrown away: 200 and 202 are different states
 * of the world - over, versus still being torn down - and the dialog says
 * which. See apiSendWithStatus.
 */
export function useCancelRun(runID: string) {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: () =>
      apiSendWithStatus<RunSummary>(
        "POST",
        `/recovery-runs/${encodeURIComponent(runID)}/cancel`,
      ),
    onSuccess: async () => {
      await qc.invalidateQueries({ queryKey: ["run", runID] })
      await qc.invalidateQueries({ queryKey: ["queue"] })
      await qc.invalidateQueries({ queryKey: ["runs"] })
    },
  })
}

/**
 * Destroys a temporary workload a drill left behind.
 *
 * Invalidating ["workloads"] reaches the orphan listing too: its key starts
 * with the same segment, so the prefix match catches it without this having to
 * name it.
 */
export function useCleanupWorkload() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (workloadID: string) =>
      apiSend<CleanupResult>("POST", `/cleanup/${encodeURIComponent(workloadID)}`),
    onSuccess: async () => {
      await qc.invalidateQueries({ queryKey: ["workloads"] })
      await qc.invalidateQueries({ queryKey: ["queue"] })
    },
  })
}

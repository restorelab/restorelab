import { apiSend, apiSendText, apiSendWithStatus } from "@/api/client"
import type {
  CleanupResult,
  NotificationChannel,
  NotificationKind,
  NotificationTest,
  Plan,
  RunSummary,
  Validated,
} from "@/api/types"
import { useMutation, useQueryClient } from "@tanstack/react-query"

/** A drill described in place: a workload, and whatever else differs. */
export interface AdhocTrigger {
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

/** A drill named by a stored plan, which carries its own workload. */
export interface PlanTrigger {
  plan: string
}

/**
 * How a drill is asked for.
 *
 * A union rather than one struct with everything optional, because that is
 * what the server enforces: "a request either names a plan or describes a
 * drill, not both", and it answers 400 listing the fields that were mixed in.
 * A type that allowed both would let the client build that 400.
 */
export type TriggerBody = AdhocTrigger | PlanTrigger

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

// ----------------------------------------------------------------- catalogue

/**
 * Asks what a document means, storing nothing.
 *
 * Not a useMutation: the editor calls it on a debounce and renders whatever
 * comes back, answer or refusal, so it wants the promise rather than a
 * mutation's state machine.
 */
export function validatePlan(document: string): Promise<Validated> {
  return apiSendText<Validated>("POST", "/plans/validate", document)
}

/** Stores a new plan. */
export function useCreatePlan() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (document: string) => apiSendText<Plan>("POST", "/plans", document),
    onSuccess: async () => {
      await qc.invalidateQueries({ queryKey: ["plans"] })
    },
  })
}

/**
 * Replaces a plan's document.
 *
 * The version guard is sent by default. A dashboard has read the plan and
 * knows which version it is editing; omitting the guard is what a CI pipeline
 * does, because it genuinely does not know, and it means the last write wins
 * in silence. Passing `undefined` is the deliberate overwrite the conflict
 * dialog offers, and it is the only way to get there.
 */
export function useUpdatePlan(ref: string) {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: ({ document, version }: { document: string; version?: number }) =>
      apiSendText<Plan>(
        "PUT",
        `/plans/${encodeURIComponent(ref)}${version === undefined ? "" : `?version=${version}`}`,
        document,
      ),
    onSuccess: async () => {
      await qc.invalidateQueries({ queryKey: ["plans"] })
      await qc.invalidateQueries({ queryKey: ["plan", ref] })
    },
  })
}

/**
 * Removes a plan.
 *
 * Its past drills keep their name and the exact document they ran: a drill
 * executes the snapshot taken when it was queued, never the catalogue row.
 */
export function useDeletePlan() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (ref: string) =>
      apiSend<void>("DELETE", `/plans/${encodeURIComponent(ref)}`),
    onSuccess: async () => {
      await qc.invalidateQueries({ queryKey: ["plans"] })
    },
  })
}

// ------------------------------------------------------------- notifications

/** A channel being created. Everything about it is stated, because nothing exists yet. */
export interface NewNotification {
  id: string
  kind: NotificationKind
  url: string
  enabled: boolean
}

/**
 * The half of a channel an edit changes.
 *
 * Every field is optional and an absent one keeps what is stored, which is a
 * PATCH's semantics under a PUT. That is deliberate on both sides of the wire:
 * the resource has a field the API refuses to hand back, so a client can never
 * send the whole thing.
 *
 * `url` being optional is the load-bearing part of this type. The screen
 * cannot prefill a webhook URL it is never given, so an edit of a name or a
 * toggle arrives with the field blank; sending that blank as an empty string
 * would ask the server to distinguish "unchanged" from "cleared" on a value
 * it cannot see. Omitting the key says "unchanged" and nothing else can.
 */
export interface NotificationEdit {
  kind?: NotificationKind
  url?: string
  enabled?: boolean
}

/** Stores a new channel, sealing its URL on the way in. */
export function useCreateNotification() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (body: NewNotification) =>
      apiSend<NotificationChannel>("POST", "/notifications", body),
    onSuccess: async () => {
      await qc.invalidateQueries({ queryKey: ["notifications"] })
    },
  })
}

/** Changes a channel, keeping every field the caller left out. */
export function useUpdateNotification(id: string) {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (body: NotificationEdit) =>
      apiSend<NotificationChannel>(
        "PUT",
        `/notifications/${encodeURIComponent(id)}`,
        body,
      ),
    onSuccess: async () => {
      await qc.invalidateQueries({ queryKey: ["notifications"] })
    },
  })
}

/**
 * Removes a channel.
 *
 * Deliveries already written keep their channel id, so this stops future
 * messages without rewriting what was said about past runs.
 */
export function useDeleteNotification() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (id: string) =>
      apiSend<void>("DELETE", `/notifications/${encodeURIComponent(id)}`),
    onSuccess: async () => {
      await qc.invalidateQueries({ queryKey: ["notifications"] })
    },
  })
}

/**
 * Sends one sample message, on purpose, now.
 *
 * It invalidates nothing, and that is not an oversight: the server records no
 * delivery for a test, because the delivery table is keyed by (run, channel)
 * and there is no run here. So the listing's last_* fields still describe the
 * last real message, which is the honest answer - a green test badge must not
 * overwrite the record of a channel that failed last night.
 */
export function useTestNotification(id: string) {
  return useMutation({
    mutationFn: () =>
      apiSend<NotificationTest>(
        "POST",
        `/notifications/${encodeURIComponent(id)}/test`,
      ),
  })
}

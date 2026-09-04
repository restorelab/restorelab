import { queryOptions } from "@tanstack/react-query"
import { apiGet } from "./client"
import {
  type Backup,
  type Confidence,
  type Doctor,
  type NotificationChannel,
  type Page,
  type Plan,
  type Provider,
  type QueueEntry,
  type RunDocument,
  type RunSummary,
  type ScheduledPlan,
  type Session,
  type Slot,
  type Workload,
  isTerminal,
} from "./types"

/**
 * How often a screen asks again.
 *
 * There is no cluster-wide event stream - the only SSE is per run - so every
 * listing is polled. The cadence comes from the data already in the cache
 * rather than from any shared state: a page whose runs have all finished slows
 * itself down on the next cycle, and a dashboard left open on a wall display
 * stops asking altogether when the tab is hidden, because
 * refetchIntervalInBackground defaults to false and nothing here changes it.
 */
export const FAST_MS = 5_000
export const SLOW_MS = 30_000

/** True while any run on this page is still going. */
export function anyRunActive(page: Page<RunSummary> | undefined): boolean {
  return page?.items.some((r) => !isTerminal(r.state)) ?? false
}

function runsInterval(q: { state: { data?: Page<RunSummary> } }): number {
  return anyRunActive(q.state.data) ? FAST_MS : SLOW_MS
}

function query(params: Record<string, string | number | undefined>): string {
  const s = new URLSearchParams()
  for (const [k, v] of Object.entries(params)) {
    if (v !== undefined && v !== "") s.set(k, String(v))
  }
  const out = s.toString()
  return out ? `?${out}` : ""
}

// ------------------------------------------------------------------- session

export const sessionQuery = () =>
  queryOptions({
    queryKey: ["session"] as const,
    queryFn: () => apiGet<Session>("/session"),
    // A revoked token must not linger behind a cached 200.
    staleTime: 0,
    retry: false,
  })

// ---------------------------------------------------------------------- runs

export interface RunsFilter {
  state?: string
  workload?: string
  cursor?: string
  limit?: number
}

export const runsQuery = (f: RunsFilter) =>
  queryOptions({
    queryKey: ["runs", f] as const,
    queryFn: () =>
      apiGet<Page<RunSummary>>(
        `/recovery-runs${query({
          state: f.state,
          workload: f.workload,
          cursor: f.cursor,
          limit: f.limit,
        })}`,
      ),
    refetchInterval: runsInterval,
  })

export const runQuery = (id: string) =>
  queryOptions({
    queryKey: ["run", id] as const,
    queryFn: () => apiGet<RunDocument>(`/recovery-runs/${encodeURIComponent(id)}`),
    // A finished run never changes again; a running one is followed by
    // EventSource, and the polling here is only the fallback for when that
    // stream has dropped.
    refetchInterval: (q) =>
      q.state.data && isTerminal(q.state.data.state) ? false : FAST_MS,
  })

export const queueQuery = () =>
  queryOptions({
    queryKey: ["queue"] as const,
    queryFn: () => apiGet<Page<QueueEntry>>("/queue"),
    refetchInterval: (q) => (q.state.data?.items.length ? FAST_MS : SLOW_MS),
  })

// ----------------------------------------------------------------- workloads

export const workloadsQuery = () =>
  queryOptions({
    queryKey: ["workloads"] as const,
    queryFn: () => apiGet<Page<Workload>>("/workloads"),
    // The inventory comes from the hypervisor and changes on a human's
    // timescale, not a drill's.
    refetchInterval: SLOW_MS,
  })

/**
 * The temporary workloads a drill left behind.
 *
 * A query of its own rather than a filter over workloadsQuery: that listing
 * hides managed workloads by default, and the overview needs exactly the ones
 * it hides. Same cadence as the inventory - an orphan is not urgent by the
 * second, only by the day.
 *
 * The key starts with "workloads" deliberately: the cleanup mutation
 * invalidates that prefix and reaches this without having to name it.
 */
export const orphansQuery = () =>
  queryOptions({
    queryKey: ["workloads", "temporary"] as const,
    queryFn: async () => {
      const page = await apiGet<Page<Workload>>("/workloads?temporary=true")
      return { items: page.items.filter((w) => w.managed) }
    },
    refetchInterval: SLOW_MS,
  })

export const workloadQuery = (id: string) =>
  queryOptions({
    queryKey: ["workload", id] as const,
    queryFn: () => apiGet<Workload>(`/workloads/${encodeURIComponent(id)}`),
  })

export const backupsQuery = (id: string) =>
  queryOptions({
    queryKey: ["backups", id] as const,
    queryFn: () => apiGet<Page<Backup>>(`/workloads/${encodeURIComponent(id)}/backups`),
  })

export const confidenceQuery = (id: string) =>
  queryOptions({
    queryKey: ["confidence", id] as const,
    queryFn: () =>
      apiGet<Confidence>(`/workloads/${encodeURIComponent(id)}/confidence`),
  })

// ---------------------------------------------------------------------- meta

export const doctorQuery = () =>
  queryOptions({
    queryKey: ["doctor"] as const,
    queryFn: () => apiGet<Doctor>("/doctor"),
    refetchInterval: SLOW_MS,
  })

// ----------------------------------------------------------------- catalogue

/**
 * The stored plans.
 *
 * A catalogue changes when a human changes it, so it polls at the inventory's
 * cadence rather than a drill's.
 */
export const plansQuery = () =>
  queryOptions({
    queryKey: ["plans"] as const,
    queryFn: () => apiGet<Page<Plan>>("/plans"),
    refetchInterval: SLOW_MS,
  })

/**
 * One plan, with its document.
 *
 * staleTime is 0 because the version this returns is the one the editor sends
 * back as its ?version= guard: a cached version number would turn a stale
 * read into a conflict the viewer cannot explain.
 */
export const planQuery = (ref: string) =>
  queryOptions({
    queryKey: ["plan", ref] as const,
    queryFn: () => apiGet<Plan>(`/plans/${encodeURIComponent(ref)}`),
    staleTime: 0,
  })

export const providersQuery = () =>
  queryOptions({
    queryKey: ["providers"] as const,
    queryFn: () => apiGet<Page<Provider>>("/providers"),
  })

/**
 * The plans that drill themselves, and when each one drills next.
 *
 * A schedule changes when a human edits a plan, so this polls at the
 * catalogue's cadence rather than a drill's.
 */
export const scheduleQuery = () =>
  queryOptions({
    queryKey: ["schedule"] as const,
    queryFn: () => apiGet<Page<ScheduledPlan>>("/schedule"),
    refetchInterval: SLOW_MS,
  })

/**
 * The slots the scheduler has decided, skipped ones included.
 *
 * Skipped slots are the point: a workload that was never drilled because its
 * slot kept being missed looks identical to one nobody scheduled, unless
 * something says otherwise.
 */
export const slotsQuery = (filter: { plan?: string; workload?: string } = {}) =>
  queryOptions({
    queryKey: ["slots", filter.plan ?? null, filter.workload ?? null] as const,
    queryFn: () => {
      const q = new URLSearchParams()
      if (filter.plan) q.set("plan", filter.plan)
      if (filter.workload) q.set("workload", filter.workload)
      const qs = q.toString()
      return apiGet<Page<Slot>>(qs ? `/schedule/slots?${qs}` : "/schedule/slots")
    },
    refetchInterval: SLOW_MS,
  })

// ------------------------------------------------------------- notifications

/**
 * The configured channels, with the health of each one's last delivery.
 *
 * It polls at the catalogue's cadence rather than a drill's: a channel changes
 * when a human changes it. The refresh matters anyway, because the last_*
 * fields do not - a delivery failing at three in the morning is exactly what
 * this listing is read for, and a screen left open on a wall display should
 * come to say so on its own.
 */
export const notificationsQuery = () =>
  queryOptions({
    queryKey: ["notifications"] as const,
    queryFn: () => apiGet<Page<NotificationChannel>>("/notifications"),
    refetchInterval: SLOW_MS,
  })

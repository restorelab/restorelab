/**
 * The wire types, mirrored by hand from the Go DTOs.
 *
 * There is no OpenAPI document, so nothing enforces that this file and
 * internal/api agree. That is a known debt, written down in the C2 spec: a
 * renamed key on the Go side passes every test here, because the tests answer
 * with fixtures built from these same types. The parade is an end-to-end
 * browser test against the real binary, and it is C3's problem.
 *
 * Sources: internal/api/{runs,workloads,meta,trigger,session,problem}.go and
 * internal/report/json.go.
 */

// ----------------------------------------------------------------- envelopes

/** Every listing answers in this shape. next_cursor is absent on the last page. */
export interface Page<T> {
  items: T[]
  next_cursor?: string
}

/** application/problem+json. */
export interface Problem {
  type: string
  title: string
  status: number
  detail?: string
  instance?: string
}

// -------------------------------------------------------------------- states

export const RUN_STATES = [
  "QUEUED",
  "DISCOVERING_BACKUP",
  "PREPARING_ENVIRONMENT",
  "RESTORING",
  "STARTING",
  "WAITING_FOR_GUEST",
  "RUNNING_CHECKS",
  "GENERATING_REPORT",
  "CLEANING_UP",
  "SUCCESS",
  "FAILED",
  "CANCELLED",
  "CLEANUP_FAILED",
] as const
export type RunState = (typeof RUN_STATES)[number]

const TERMINAL: ReadonlySet<string> = new Set([
  "SUCCESS",
  "FAILED",
  "CANCELLED",
  "CLEANUP_FAILED",
])

/**
 * Mirrors core.RunState.Terminal().
 *
 * A state this build has never heard of counts as still running: a new value
 * is far more likely to be a new stage than a new ending, and calling it
 * finished would stop the live view on a drill that is still going.
 */
export function isTerminal(state: string): boolean {
  return TERMINAL.has(state)
}

export type RunResult = "SUCCESS" | "DEGRADED" | "FAILED"
export type StepStatus = "pending" | "running" | "done" | "failed" | "skipped"
export type CheckStatus = "pass" | "fail" | "error" | "skipped"

// ---------------------------------------------------------------------- runs

/** One row of GET /recovery-runs. */
export interface RunSummary {
  id: string
  plan_name: string
  plan_id?: string
  source_workload_id: string
  source_name?: string
  state: RunState
  result?: RunResult
  started_at: string
  completed_at: string | null
  rto_seconds: number
  rto: string
  rto_target_seconds?: number
  rto_exceeded: boolean
  cleanup_done: boolean
}

/** One row of GET /queue: a run plus the lease over it. */
export interface QueueEntry extends RunSummary {
  worker?: string
  /** A lease in the past means the worker stopped renewing it. */
  lease_expires_at?: string
}

/** A backup, as report.BackupDTO renders it. */
export interface Backup {
  id: string
  workload_id: string
  provider_id?: string
  datastore?: string
  node?: string
  created_at: string
  age_seconds: number
  age: string
  size_bytes: number
  size: string
  protected: boolean
  encrypted: boolean
  verified: string
  format?: string
  notes?: string
}

/** One stage of a drill, as report.StepDTO renders it. */
export interface Step {
  name: string
  state: RunState
  status: StepStatus
  started_at: string
  completed_at: string
  duration_seconds: number
  duration: string
  message?: string
  error?: string
}

/** One in-guest check, as report.CheckDTO renders it. */
export interface Check {
  name: string
  type: string
  status: CheckStatus
  pass: boolean
  started_at: string
  completed_at: string
  duration_seconds: number
  duration: string
  attempts: number
  message?: string
}

/**
 * GET /recovery-runs/{id} - the full report document, not a summary.
 *
 * It already carries steps and checks, so the detail screen needs exactly one
 * request, not two.
 */
export interface RunDocument {
  schema: string
  run_id: string
  plan_name: string
  plan_id?: string
  plan_version?: number
  provider_id?: string
  backup_provider_id?: string
  source_workload_id: string
  source_name: string
  temp_workload_id?: string
  temp_name?: string
  node?: string
  backup: Backup | null
  state: RunState
  result: RunResult
  started_at: string
  completed_at: string
  steps: Step[]
  checks: Check[]
  rto_seconds: number
  rto: string
  rto_target_seconds?: number
  rto_target?: string
  rto_exceeded: boolean
  cleanup_done: boolean
  error?: string
}

// ----------------------------------------------------------------- workloads

export interface WorkloadStatus {
  power_state: string
  uptime_seconds: number
  agent_ready: boolean
  ips?: string[]
  cpu_usage: number
  memory_bytes: number
}

export interface Workload {
  id: string
  name: string
  kind: string
  node?: string
  cluster?: string
  tags?: string[]
  cpu_cores: number
  memory_bytes: number
  disk_bytes: number
  power_state: string
  template: boolean
  managed: boolean
  recovery_run_id?: string
  status?: WorkloadStatus
}

/**
 * GET /workloads/{id}/confidence.
 *
 * score is null when the workload has never been tested. A UI renders that as
 * "--", never as 0%: "we have no idea" and "we know it is bad" are different
 * answers, and the Go DTO carries the distinction deliberately.
 */
export interface Confidence {
  workload_id: string
  score: number | null
  tested: boolean
  reasons: string[]
  last_run_id?: string
  runs_considered: number
}

// ---------------------------------------------------------------------- meta

export interface Provider {
  id: string
  kind: string
  roles?: string[]
  endpoint: string
  node?: string
  datastore?: string
  insecure: boolean
  default?: boolean
}

export interface Finding {
  level: string
  area: string
  title: string
  detail?: string
}

export interface Doctor {
  provider_id: string
  endpoint?: string
  ok: boolean
  problems: number
  findings: Finding[]
}

// ------------------------------------------------------------------- session

export interface Session {
  token_name: string
  scopes: string[]
  expires_at: string
}

// ----------------------------------------------------------------- SSE frames

/** The payload of `event: progress`. */
export interface ProgressFrame {
  seq: number
  at: string
  state: RunState
  step?: string
  status?: StepStatus
  message?: string
  check?: Check
  error?: string
}

/** The payload of `event: done` - the drill ended. */
export interface DoneFrame {
  state: RunState
}

/** The payload of `event: disconnected` - the connection ended, not the run. */
export interface DisconnectedFrame {
  state: RunState
  reason: string
}

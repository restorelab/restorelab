/**
 * The wire types, mirrored by hand from the Go DTOs.
 *
 * What keeps this file and internal/api in step is the captured fixtures next
 * door: internal/api/fixtures_test.go writes the real body of every route
 * into __fixtures__, and fixtures.ts reads them back under these types, which
 * tsc checks. A renamed or dropped key fails there, on the day it changes.
 *
 * The check is one-directional, and it is worth knowing which direction.
 * It proves that every key this file *requires* is present in the capture,
 * with the right type. It says nothing about keys the capture has and this
 * file does not: TypeScript only flags excess properties on object literals,
 * and a fixture is an imported module.
 *
 * So a key the server renames or drops fails here. A key the server *adds*
 * does not, and neither does a field deleted from this file. Both of those
 * are on you: when you add a field to a DTO, add it here too - nothing will
 * remind you.
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
  "INCONCLUSIVE",
] as const
export type RunState = (typeof RUN_STATES)[number]

const TERMINAL: ReadonlySet<string> = new Set([
  "SUCCESS",
  "FAILED",
  "CANCELLED",
  "CLEANUP_FAILED",
  "INCONCLUSIVE",
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

/**
 * What a drill established, in increasing order - core.ProofLevel.
 *
 * It answers a different question from RunResult: the result says how the
 * drill went, this says what it proved. A workload drilled with the default
 * `cmd:hostname` succeeds and establishes BOOT, nothing more, and printing the
 * first without the second is how a backup tool ends up reassuring people
 * about a recovery nobody verified.
 */
export const PROOF_LEVELS = ["NONE", "BOOT", "SERVICE", "DATA"] as const
export type ProofLevel = (typeof PROOF_LEVELS)[number]

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

  /**
   * What this drill established. Absent on a run that predates the field,
   * which means "not recorded" - never "nothing was proven". The two are
   * different statements and the interface must not merge them.
   */
  proof_level?: ProofLevel
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

  /** As on RunSummary: absent means not recorded, not "nothing was proven". */
  proof_level?: ProofLevel
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

  /**
   * The workload's most recent drill, all four absent when it has never had
   * one. last_run_at is when that drill *started*: a drill still running has
   * no completion time, and "last tested" has to have an answer while it is
   * in flight.
   *
   * The state is here as well as the result because a run still going and a
   * run that was cancelled both carry an empty result, and they are not the
   * same news.
   */
  last_run_id?: string
  last_run_at?: string
  last_run_state?: RunState
  last_run_result?: RunResult

  /**
   * What that last drill established. Absent when it was never drilled and
   * also when the run predates the field: neither case may be shown as a
   * level, which is why the badge renders nothing rather than a floor.
   */
  last_run_proof?: ProofLevel

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

  /**
   * What the newest drill that reached a verdict established, and the ceiling
   * it puts on the score. They are here so a client can explain the number
   * instead of printing it: 60 beside "only the boot was verified" is a
   * sentence, a bare 60 is a mystery. Both absent when nothing was recorded,
   * and the score is then capped by nothing.
   */
  proof_level?: ProofLevel
  proof_cap?: number
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

/**
 * One diagnostic finding.
 *
 * internal/diag emits exactly three levels - a cluster in perfect health
 * comes back as a list of "ok" findings, not as an empty list. The union is
 * written out rather than left as `string` because C2 spent a screen on a
 * fourth level, "error", that no Go code has ever produced.
 */
export interface Finding {
  level: "ok" | "warn" | "fail"
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

// ---------------------------------------------------------------- catalogue

/**
 * A stored plan.
 *
 * `yaml` is absent from a listing and present on a detail: a catalogue of
 * fifty plans must not ship fifty documents to draw a table. The document is
 * stored and returned verbatim, comments included, so an editor may send back
 * exactly what it received without losing anything a human wrote.
 */
export interface Plan {
  id: string
  name: string
  description?: string
  workload_id: string
  provider_id?: string
  version: number
  created_at: string
  updated_at: string
  yaml?: string
}

/**
 * What POST /plans/validate answers about a document, having stored nothing.
 *
 * `normalized_yaml` is the document with its defaults applied, and it is the
 * useful half: it shows the difference between a field left out and a field
 * left out *meaning something*. A plan with no `restore` block still lands its
 * clone on the isolated bridge, and this is where that becomes visible.
 */
export interface Validated {
  valid: boolean
  name: string
  description?: string
  workload_id: string
  provider_id?: string
  normalized_yaml: string

  /**
   * What this plan would establish if every one of its checks passed, and the
   * same thing as a sentence in the conditional. A promise about the document,
   * not a fact about a drill - which is why the editor is the one screen that
   * can answer "is this worth running" before five minutes are spent finding
   * out.
   */
  proof_level?: ProofLevel
  proof_summary?: string
}

// --------------------------------------------------------------------- setup

/** What the wizard collected, and the only request it ever gets to send. */
export interface SetupRequest {
  endpoint: string
  admin_user: string
  admin_password: string
  storages: string[]
  insecure?: boolean
  fingerprint?: string
  create_bridge?: boolean
  apply_bridge?: boolean
}

/** One provisioning action, in the order it was performed. */
export interface SetupStep {
  description: string
  status: string
  detail?: string
}

/**
 * What POST /setup produced.
 *
 * `token` is the RestoreLab API token the wizard mints at the end, returned
 * exactly once. The browser holds it in memory, waits for the server to come
 * back configured, and exchanges it for a session cookie - it is never
 * written to storage, and it never needs to survive a reload because the
 * wizard does not reload.
 */
export interface SetupOutcome {
  steps: SetupStep[]
  provider_id: string
  node?: string
  bridge?: string
  bridge_applied?: boolean
  token: string
  token_name: string
}

/**
 * A setup refusal, with how far provisioning got.
 *
 * The steps are the useful half: every one of them is idempotent, so knowing
 * where it stopped turns "fix it and run it again" into a real instruction
 * rather than a hope.
 */
export interface SetupFailure extends Problem {
  steps?: SetupStep[]
}

// ------------------------------------------------------------------- writes

/** POST /cleanup/{vmid} - what was destroyed. */
export interface CleanupResult {
  workload_id: string
  removed: boolean
}

/**
 * One cron slot the scheduler has decided about.
 *
 * A skipped slot carries a reason and no run. It is the answer to "why was
 * this machine not tested", which the run history cannot give: a slot that
 * was skipped produced no run to look at.
 */
export interface Slot {
  plan_id: string
  plan_name?: string
  slot_at: string
  decided_at: string
  outcome: "queued" | "skipped"
  reason?: string
  run_id?: string
}

/**
 * A plan that drills itself, and what is about to happen to it.
 *
 * next_slot_at is null when the schedule could not be read, and `error` then
 * says why. The two are separate fields because "nothing is coming" and "we
 * could not work it out" are different answers, and a plan whose cron broke
 * has silently stopped being tested.
 */
export interface ScheduledPlan {
  plan_id: string
  name: string
  workload_id: string
  schedule: string
  timezone?: string
  next_slot_at: string | null
  error?: string
  last_slot?: Slot
}

// ------------------------------------------------------------- notifications

/**
 * The three channel kinds this product can render for.
 *
 * A list rather than a bare union so the settings form can build its options
 * from the same source the type comes from: a fourth kind added to the Go
 * registry is then one line here, not two.
 */
export const NOTIFICATION_KINDS = ["discord", "slack", "webhook"] as const
export type NotificationKind = (typeof NOTIFICATION_KINDS)[number]

/**
 * A configured notification channel.
 *
 * There is no `url`, and there is not going to be one. The API refuses to
 * hand a webhook URL back in any response, truncated or starred out, because
 * the URL is the credential: whoever holds it can post into that channel with
 * no second factor. `host` is what an operator gets instead, and it is enough
 * to tell two channels apart.
 *
 * `kind` is a plain string rather than NotificationKind on purpose. The API
 * validates the kind on the way in, but a configuration file edited by hand
 * does not go through the API, so a listing can carry a kind this build has
 * never heard of. Narrowing it here would make the type lie about that case
 * instead of letting the screen render it.
 *
 * The four last_* fields are the channel's health, read off its most recent
 * delivery. last_state travels beside last_error because a pending delivery
 * that will be retried in thirty seconds carries an error too, and painting
 * that as a dead channel would be an overstatement in the other direction.
 */
export interface NotificationChannel {
  id: string
  kind: string
  host: string
  enabled: boolean
  last_state?: string
  last_sent?: string
  last_status?: number
  last_error?: string
}

/**
 * What one deliberate test message produced.
 *
 * `status` is the far end's own answer, not RestoreLab's: Discord replies 204
 * and Slack replies 200, and somebody who has never seen this path fire needs
 * to see which of them spoke rather than a 200 this server invented.
 */
export interface NotificationTest {
  id: string
  kind: string
  status: number
  sent_at: string
}

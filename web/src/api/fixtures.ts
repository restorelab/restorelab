/**
 * The captured API responses, read back under the types this app believes in.
 *
 * This file is one half of a contract; the other half is
 * internal/api/fixtures_test.go, which writes the JSON next door from the real
 * handlers. There is no OpenAPI document, so this is what stops types.ts from
 * drifting away from the Go DTOs: a renamed key fails the golden test there,
 * and then fails `tsc` here.
 *
 * The checking happens at compile time and costs nothing to run. It is not a
 * Vitest file on purpose - there is no behaviour to exercise, only a shape to
 * assert.
 *
 * The tests import from here rather than building payloads of their own. That
 * is the other half of the point: a test that invents its own response proves
 * only that the code agrees with the test.
 */

import backups from "./__fixtures__/backups.json"
import cancel200 from "./__fixtures__/cancel-200.json"
import cancel202 from "./__fixtures__/cancel-202.json"
import cleanup from "./__fixtures__/cleanup.json"
import confidence from "./__fixtures__/confidence.json"
import doctor from "./__fixtures__/doctor.json"
import plan from "./__fixtures__/plan.json"
import plansPage from "./__fixtures__/plans-page.json"
import problem401 from "./__fixtures__/problem-401.json"
import problem404 from "./__fixtures__/problem-404.json"
import problem409Version from "./__fixtures__/problem-409-version.json"
import problem409 from "./__fixtures__/problem-409.json"
import providers from "./__fixtures__/providers.json"
import queue from "./__fixtures__/queue.json"
import runEvents from "./__fixtures__/run-events.json"
import runFinished from "./__fixtures__/run-finished.json"
import runRunning from "./__fixtures__/run-running.json"
import runsPage from "./__fixtures__/runs-page.json"
import scheduleSlots from "./__fixtures__/schedule-slots.json"
import schedule from "./__fixtures__/schedule.json"
import session from "./__fixtures__/session.json"
import setupFailed from "./__fixtures__/setup-failed.json"
import setupResult from "./__fixtures__/setup-result.json"
import trigger201 from "./__fixtures__/trigger-201.json"
import validateInvalid from "./__fixtures__/validate-invalid.json"
import validateOk from "./__fixtures__/validate-ok.json"
import workload from "./__fixtures__/workload.json"
import workloadsPage from "./__fixtures__/workloads-page.json"
import type {
  Backup,
  CleanupResult,
  Confidence,
  Doctor,
  Page,
  Plan,
  Problem,
  ProgressFrame,
  Provider,
  QueueEntry,
  RunDocument,
  RunSummary,
  ScheduledPlan,
  Session,
  SetupFailure,
  SetupOutcome,
  Slot,
  Validated,
  Workload,
} from "./types"

/**
 * A type with its string unions widened back to plain strings.
 *
 * A JSON import gives `string` where types.ts says `"SUCCESS" | "FAILED"`, so
 * a straight annotation would fail on every enum for a reason that has nothing
 * to do with drift. Widening the unions - and nothing else - leaves the checks
 * that matter intact: a key the server renamed or dropped, and a number that
 * became a string, both still fail.
 *
 * What no annotation can catch here is a key the capture has and the type does
 * not. Excess properties are only flagged on object literals, and this is an
 * imported module - see the note at the top of types.ts.
 */
type Widen<T> = T extends string
  ? string
  : T extends readonly (infer E)[]
    ? Widen<E>[]
    : T extends object
      ? { [K in keyof T]: Widen<T[K]> }
      : T

/**
 * Reads one capture as T.
 *
 * The parameter type is the assertion; the return type is what the tests
 * consume. The cast is safe precisely because the parameter checked the shape
 * - the only thing it gives up is the union narrowing Widen let go of.
 */
function fixture<T>(json: Widen<T>): T {
  return json as T
}

/**
 * The first item of a captured page, or a loud failure.
 *
 * Indexing an array yields `T | undefined` under noUncheckedIndexedAccess,
 * and a test that spread that would fail with a type error far from its
 * cause. A fixture page that has become empty is a fixture worth fixing on
 * the Go side, so this says so rather than papering over it.
 */
export function first<T>(items: T[], what: string): T {
  const [item] = items
  if (!item) {
    throw new Error(
      `the captured ${what} fixture is empty: fix the case in internal/api/fixtures_test.go`,
    )
  }
  return item
}

export const sessionFixture = fixture<Session>(session)
export const runsPageFixture = fixture<Page<RunSummary>>(runsPage)
export const runFinishedFixture = fixture<RunDocument>(runFinished)
export const runRunningFixture = fixture<RunDocument>(runRunning)
export const runEventsFixture = fixture<Page<ProgressFrame>>(runEvents)
export const queueFixture = fixture<Page<QueueEntry>>(queue)
export const workloadsPageFixture = fixture<Page<Workload>>(workloadsPage)
export const workloadFixture = fixture<Workload>(workload)
export const backupsFixture = fixture<Page<Backup>>(backups)
export const confidenceFixture = fixture<Confidence>(confidence)
export const doctorFixture = fixture<Doctor>(doctor)
export const providersFixture = fixture<Page<Provider>>(providers)
export const trigger201Fixture = fixture<RunSummary>(trigger201)
export const cancel200Fixture = fixture<RunSummary>(cancel200)
export const cancel202Fixture = fixture<RunSummary>(cancel202)
export const cleanupFixture = fixture<CleanupResult>(cleanup)
export const problem401Fixture = fixture<Problem>(problem401)
export const problem404Fixture = fixture<Problem>(problem404)
export const problem409Fixture = fixture<Problem>(problem409)

export const plansPageFixture = fixture<Page<Plan>>(plansPage)
export const planFixture = fixture<Plan>(plan)
export const scheduleFixture = fixture<Page<ScheduledPlan>>(schedule)
export const slotsFixture = fixture<Page<Slot>>(scheduleSlots)
export const validateOkFixture = fixture<Validated>(validateOk)
export const validateInvalidFixture = fixture<Problem>(validateInvalid)
export const problem409VersionFixture = fixture<Problem>(problem409Version)

export const setupResultFixture = fixture<SetupOutcome>(setupResult)
export const setupFailedFixture = fixture<SetupFailure>(setupFailed)

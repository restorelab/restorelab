import type { StreamState } from "@/api/runStream"
import type { Check, Step, StepStatus } from "@/api/types"
import { EmptyState } from "@/components/empty-state"
import { CheckStatusIcon, StepStatusIcon, toneClass } from "@/components/run-status"
import { addNamespace } from "@/i18n"
import run from "@/i18n/locales/en/run.json"
import { useTranslation } from "react-i18next"

// The module that uses a namespace is the one that registers it. Without this
// line t() renders the raw key and every label here reads "noSteps.title".
addNamespace("run", run)

/** The step the in-guest checks belong under, by name, as the engine emits it. */
const CHECKS_STEP = "checks"

/**
 * One row of the timeline, after the document and the live stream have been
 * folded together.
 *
 * The document is the resting truth and it decides the order; the stream only
 * carries a status that is newer, so it overrides by step name and may add a
 * step the document has not written down yet. A duration only ever comes from
 * the document: a step still running has not got one.
 */
interface Row {
  name: string
  status: StepStatus
  duration?: string
  message?: string
  error?: string
}

function merge(steps: Step[], live: StreamState | null): Row[] {
  const rows: Row[] = steps.map((s) => {
    const fresh = live?.steps.get(s.name)
    return {
      name: s.name,
      status: fresh ? fresh.status : s.status,
      duration: s.duration,
      message: fresh?.message ?? s.message,
      error: fresh?.error ?? s.error,
    }
  })

  if (live) {
    const known = new Set(rows.map((r) => r.name))
    for (const fresh of live.steps.values()) {
      if (known.has(fresh.name)) continue
      rows.push({
        name: fresh.name,
        status: fresh.status,
        message: fresh.message,
        error: fresh.error,
      })
    }
  }

  return rows
}

function CheckList({ checks }: { checks: Check[] }) {
  const { t } = useTranslation("run")
  return (
    <ul className="mt-2 space-y-1.5 border-border border-l pl-4">
      {checks.map((check) => (
        <li key={`${check.name}-${check.started_at}`} className="text-sm">
          <div className="flex items-center gap-2">
            <CheckStatusIcon status={check.status} />
            <span>{check.name}</span>
            {check.attempts > 1 ? (
              <span className="tabular text-muted-foreground text-xs">
                {t("attempts", { count: check.attempts })}
              </span>
            ) : null}
            <span className="tabular ml-auto text-muted-foreground">
              {check.duration}
            </span>
          </div>
          {check.message && !check.pass ? (
            <p className={`mt-1 pl-6 text-xs ${toneClass("failed")}`}>
              {check.message}
            </p>
          ) : null}
        </li>
      ))}
    </ul>
  )
}

/**
 * The phases of a drill, in order, with the in-guest checks nested under the
 * phase that ran them.
 */
export function PhaseTimeline({
  steps,
  checks,
  live,
}: {
  steps: Step[]
  checks: Check[]
  live: StreamState | null
}) {
  const { t } = useTranslation("run")
  const rows = merge(steps, live)

  if (rows.length === 0 && checks.length === 0) {
    return <EmptyState title={t("noSteps.title")} />
  }

  const hasChecksStep = rows.some((r) => r.name === CHECKS_STEP)

  return (
    <ol className="space-y-4">
      {rows.map((row) => (
        <li key={row.name}>
          <div className="flex items-center gap-3">
            <StepStatusIcon status={row.status} />
            <span className="font-medium text-sm">{row.name}</span>
            {row.duration ? (
              <span className="tabular ml-auto text-muted-foreground text-sm">
                {row.duration}
              </span>
            ) : null}
          </div>
          {row.message ? (
            <p className="mt-1 pl-7 text-muted-foreground text-sm">{row.message}</p>
          ) : null}
          {row.error ? (
            <p className={`mt-1 pl-7 text-sm ${toneClass("failed")}`}>{row.error}</p>
          ) : null}
          {row.name === CHECKS_STEP && checks.length > 0 ? (
            <div className="pl-7">
              <CheckList checks={checks} />
            </div>
          ) : null}
        </li>
      ))}

      {/* A drill can record checks without a phase named for them - an older
          run, or an engine that reported them alongside. They are still the
          answer to "what was verified", so they are not dropped. */}
      {!hasChecksStep && checks.length > 0 ? (
        <li>
          <h3 className="font-medium text-sm">{t("checks")}</h3>
          <CheckList checks={checks} />
        </li>
      ) : null}
    </ol>
  )
}

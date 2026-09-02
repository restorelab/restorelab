import type { CheckStatus, RunResult, RunState, StepStatus } from "@/api/types"
import { cn } from "@/lib/utils"
import {
  AlertTriangle,
  Ban,
  CheckCircle2,
  CircleDashed,
  Loader2,
  XCircle,
} from "lucide-react"
import { useTranslation } from "react-i18next"

/**
 * The state vocabulary, defined once.
 *
 * Colour says one thing on this dashboard and one thing only: state. Nothing
 * here is decorative, and no component gets to decide its own colour for a
 * status - if it did, two screens would disagree about what amber means.
 */
export type Tone = "success" | "failed" | "warning" | "running" | "idle"

const TONE_TEXT: Record<Tone, string> = {
  success: "text-state-success",
  failed: "text-state-failed",
  warning: "text-state-warning",
  running: "text-state-running",
  idle: "text-state-idle",
}

export function toneClass(tone: Tone): string {
  return TONE_TEXT[tone]
}

/**
 * A run's colour.
 *
 * DEGRADED is the interesting case: the drill recovered, so it is not a
 * failure, but a non-critical check failed or the RTO was missed, so it is not
 * a clean pass either. Amber is the honest answer.
 *
 * CANCELLED is deliberately not red. Someone chose to stop it; that is not the
 * same news as a backup that would not restore.
 */
export function runTone(state: RunState, result?: RunResult): Tone {
  if (state === "SUCCESS") return result === "DEGRADED" ? "warning" : "success"
  if (state === "FAILED" || state === "CLEANUP_FAILED") return "failed"
  if (state === "CANCELLED" || state === "QUEUED") return "idle"
  return "running"
}

export function stepTone(status: StepStatus): Tone {
  switch (status) {
    case "done":
      return "success"
    case "failed":
      return "failed"
    case "running":
      return "running"
    default:
      return "idle"
  }
}

/** A check that could not run is not a check that failed. */
export function checkTone(status: CheckStatus): Tone {
  switch (status) {
    case "pass":
      return "success"
    case "fail":
      return "failed"
    case "error":
      return "warning"
    default:
      return "idle"
  }
}

export function runLabelKey(state: RunState): string {
  return `runState.${state}`
}

const TONE_ICON: Record<Tone, typeof CheckCircle2> = {
  success: CheckCircle2,
  failed: XCircle,
  warning: AlertTriangle,
  running: Loader2,
  idle: CircleDashed,
}

function ToneIcon({ tone, className }: { tone: Tone; className?: string }) {
  const Icon = TONE_ICON[tone]
  return (
    <Icon
      aria-hidden="true"
      className={cn("size-4 shrink-0", tone === "running" && "animate-spin", className)}
    />
  )
}

export function RunStatusBadge({
  state,
  result,
  className,
}: {
  state: RunState
  result?: RunResult
  className?: string
}) {
  const { t } = useTranslation()
  const tone = runTone(state, result)
  return (
    <span
      className={cn(
        "inline-flex items-center gap-1.5 text-sm",
        toneClass(tone),
        className,
      )}
    >
      <ToneIcon tone={tone} />
      {t(runLabelKey(state))}
    </span>
  )
}

export function StepStatusIcon({ status }: { status: StepStatus }) {
  const { t } = useTranslation()
  const tone = stepTone(status)
  return (
    <span className={toneClass(tone)} title={t(`stepStatus.${status}`)}>
      <ToneIcon tone={tone} />
    </span>
  )
}

export function CheckStatusIcon({ status }: { status: CheckStatus }) {
  const { t } = useTranslation()
  const tone = checkTone(status)
  const Icon = status === "skipped" ? Ban : TONE_ICON[tone]
  return (
    <span className={toneClass(tone)} title={t(`checkStatus.${status}`)}>
      <Icon aria-hidden="true" className="size-4 shrink-0" />
    </span>
  )
}

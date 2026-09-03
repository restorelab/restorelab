import {
  type CheckStatus,
  type ProofLevel,
  type RunResult,
  type RunState,
  type StepStatus,
  isTerminal,
} from "@/api/types"
import { cn } from "@/lib/utils"
import {
  AlertTriangle,
  Ban,
  CheckCircle2,
  CircleDashed,
  Loader2,
  Shield,
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
 *
 * INCONCLUSIVE is amber for the same reason, and it is the one worth
 * understanding: the backup restored and the workload booted, but a critical
 * check could not be evaluated - most often a tcp: or http: check dialled
 * from a machine with no route into the isolated recovery network. Red would
 * tell somebody their backup is broken when it demonstrably is not. Amber says
 * what is true: nobody knows yet, and something needs fixing before anybody
 * will.
 */
export function runTone(state: RunState, result?: RunResult): Tone {
  if (state === "SUCCESS") return result === "DEGRADED" ? "warning" : "success"
  if (state === "FAILED" || state === "CLEANUP_FAILED") return "failed"
  if (state === "INCONCLUSIVE") return "warning"
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

/**
 * The key for a run's label.
 *
 * A degraded run is stored with state SUCCESS and result DEGRADED - the
 * workload did come back - so labelling it from the state alone printed
 * "Succeeded" next to the amber icon runTone already gives it. The word
 * contradicted the icon, and of the two the word is what people quote.
 *
 * The result only ever overrides the label in that one case. Everywhere else
 * the state is the more precise of the two: it distinguishes CANCELLED,
 * INCONCLUSIVE and CLEANUP_FAILED, which the result cannot.
 */
export function runLabelKey(state: RunState, result?: RunResult): string {
  if (state === "SUCCESS" && result === "DEGRADED") return "runResult.DEGRADED"
  return `runState.${state}`
}

/**
 * A proof level's colour.
 *
 * A low level is a fact, not a fault, so none of them is ever red: red says a
 * drill went badly, and a restore-only drill that ran nothing inside the guest
 * went exactly as it was asked to. NONE is neutral for that reason - no claim
 * was made either way - and BOOT is the one worth an amber: it is the level
 * that looks like a pass and is not. A machine drilled with the default
 * `cmd:hostname` succeeds, scores well, and has proven only that the kernel
 * came up; amber is where that stops being invisible.
 *
 * An unrecorded level - a run from before the field existed - is neutral too.
 * Nothing may be concluded from it in either direction.
 */
export function proofTone(level?: ProofLevel): Tone {
  switch (level) {
    case "DATA":
    case "SERVICE":
      return "success"
    case "BOOT":
      return "warning"
    default:
      return "idle"
  }
}

/**
 * The keys for a level's short label and its sentence.
 *
 * An absent level keys "unrecorded", never "NONE": "we did not write it down"
 * and "nothing was proven" are different statements, and the whole scale is
 * worthless the moment the interface says one when it means the other.
 */
export function proofLabelKey(level?: ProofLevel): string {
  return `proofLevel.${level || "unrecorded"}`
}

export function proofPhraseKey(level?: ProofLevel): string {
  return `proofPhrase.${level || "unrecorded"}`
}

/**
 * Whether a run's level is settled enough to be shown.
 *
 * A drill still queued or still in flight carries NONE, and the API is right
 * to say so: it has established nothing *yet*. Rendered as-is it reads as a
 * verdict on a run that has not reached one, and "nothing was verified" about
 * a restore still copying disks is a lie told in an honest field. So nothing
 * is shown until the run is terminal.
 *
 * A caller with no state to offer - the confidence score, which only ever
 * counts drills that reached a verdict - passes none and gets true.
 */
export function proofIsSettled(state?: RunState): boolean {
  return state === undefined || isTerminal(state)
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
      {t(runLabelKey(state, result))}
    </span>
  )
}

/**
 * What a drill established, in two words and a colour.
 *
 * It renders nothing at all when no level was recorded. That is deliberate:
 * beside a state badge, a "Not recorded" chip on every historic row would be
 * noise, and any level shown there would be a claim nobody made. Screens with
 * room for a sentence use ProofPhrase instead, which does say it.
 *
 * The shield is the same for every level - only the colour moves - so that a
 * proof level never reads as a second verdict on the run. The verdict is the
 * state badge next to it.
 *
 * Passing the run's state suppresses the badge while that run is still going,
 * for the reason proofIsSettled explains.
 */
export function ProofBadge({
  level,
  state,
  className,
}: {
  level?: ProofLevel
  /** The run the level belongs to, when there is one. */
  state?: RunState
  className?: string
}) {
  const { t } = useTranslation()
  if (!level || !proofIsSettled(state)) return null
  const tone = proofTone(level)
  return (
    <span
      className={cn(
        "inline-flex items-center gap-1.5 text-sm",
        toneClass(tone),
        className,
      )}
      title={t(proofPhraseKey(level))}
    >
      <Shield aria-hidden="true" className="size-4 shrink-0" />
      {t(proofLabelKey(level))}
    </span>
  )
}

/**
 * The same thing as the sentence somebody can act on.
 *
 * "Succeeded" invites you to stop reading; "Succeeded / only the boot was
 * verified" is the line that makes somebody go and write a real check. Unlike
 * the badge this renders for an unrecorded level too, because here there is
 * room to say which of the two silences it is - but not for a drill still in
 * flight, which has an answer to nothing yet.
 */
export function ProofPhrase({
  level,
  state,
  className,
}: {
  level?: ProofLevel
  state?: RunState
  className?: string
}) {
  const { t } = useTranslation()
  if (!proofIsSettled(state)) return null
  return (
    <p className={cn("text-muted-foreground text-sm", className)}>
      {t(proofPhraseKey(level))}
    </p>
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

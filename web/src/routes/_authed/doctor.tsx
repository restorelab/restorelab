import { doctorQuery } from "@/api/queries"
import type { Doctor, Finding } from "@/api/types"
import { EmptyState } from "@/components/empty-state"
import { ErrorState } from "@/components/error-state"
import { type Tone, toneClass } from "@/components/run-status"
import { Skeleton } from "@/components/ui/skeleton"
import { addNamespace } from "@/i18n"
import doctorLocale from "@/i18n/locales/en/doctor.json"
import { cn } from "@/lib/utils"
import { useQuery } from "@tanstack/react-query"
import { createFileRoute } from "@tanstack/react-router"
import {
  AlertTriangle,
  CheckCircle2,
  CircleDashed,
  Stethoscope,
  XCircle,
} from "lucide-react"
import { useTranslation } from "react-i18next"

// The module that uses a namespace registers it, at import time. Without this
// line t() renders the raw key and this screen shows "ok.title".
addNamespace("doctor", doctorLocale)

export const Route = createFileRoute("/_authed/doctor")({
  component: DoctorPage,
})

/**
 * "Is the cluster configured correctly?"
 *
 * GET /doctor always answers 200, findings and all - a misconfigured cluster
 * is exactly what the endpoint is for. So a finding is never a request error,
 * and only a transport or auth failure gets the error state.
 */
function DoctorPage() {
  const q = useQuery(doctorQuery())

  if (q.isPending) return <DoctorSkeleton />
  if (q.isError) {
    return <ErrorState error={q.error} onRetry={() => void q.refetch()} />
  }
  return <DoctorContent doctor={q.data} />
}

/**
 * The diagnostic's colour, from the level the Go side writes.
 *
 * internal/diag emits three: ok, warn, fail. The API copies the string
 * through, so this maps the synonyms a future level might use rather than
 * matching one spelling exactly, and anything it has never heard of stays
 * grey - an unknown level is not a licence to paint the screen red.
 */
export function findingTone(level: string): Tone {
  switch (level.toLowerCase()) {
    case "fail":
    case "failed":
    case "error":
      return "failed"
    case "warn":
    case "warning":
      return "warning"
    case "ok":
    case "pass":
      return "success"
    default:
      return "idle"
  }
}

const TONE_ICON: Record<Tone, typeof CheckCircle2> = {
  success: CheckCircle2,
  failed: XCircle,
  warning: AlertTriangle,
  running: CircleDashed,
  idle: CircleDashed,
}

/**
 * The screen itself, kept apart from the route so it renders without a router
 * or a query client.
 */
export function DoctorContent({ doctor }: { doctor: Doctor }) {
  const { t } = useTranslation("doctor")

  return (
    <div className="flex flex-col gap-6">
      <header className="flex flex-col gap-3">
        <div className="flex flex-wrap items-center gap-3">
          <h1 className="font-semibold text-xl">{t("title")}</h1>
          <Verdict ok={doctor.ok} problems={doctor.problems} />
        </div>
        <dl className="flex flex-wrap gap-x-8 gap-y-1 text-sm">
          <div className="flex items-baseline gap-2">
            <dt className="text-muted-foreground">{t("provider")}</dt>
            <dd className="font-mono">{doctor.provider_id}</dd>
          </div>
          {doctor.endpoint ? (
            <div className="flex items-baseline gap-2">
              <dt className="text-muted-foreground">{t("endpoint")}</dt>
              <dd className="font-mono break-all">{doctor.endpoint}</dd>
            </div>
          ) : null}
        </dl>
      </header>

      {doctor.findings.length === 0 ? (
        <EmptyState
          title={t("ok.title")}
          description={t("ok.description")}
          icon={<Stethoscope className="size-6" aria-hidden="true" />}
        />
      ) : (
        <ul className="divide-y rounded-lg border">
          {doctor.findings.map((f) => (
            <FindingRow key={`${f.level}:${f.area}:${f.title}`} finding={f} />
          ))}
        </ul>
      )}
    </div>
  )
}

/** The one-line answer, next to the title. */
function Verdict({ ok, problems }: { ok: boolean; problems: number }) {
  const { t } = useTranslation("doctor")
  const tone: Tone = ok ? "success" : "failed"
  const Icon = TONE_ICON[tone]
  return (
    <span className={cn("inline-flex items-center gap-1.5 text-sm", toneClass(tone))}>
      <Icon className="size-4 shrink-0" aria-hidden="true" />
      {ok ? t("healthy") : t("problems", { count: problems })}
    </span>
  )
}

/**
 * One check: its verdict as a colour and an icon, then what it is about, what
 * it found, and - when the diagnostic has one - what to do next.
 */
function FindingRow({ finding }: { finding: Finding }) {
  const { t } = useTranslation("doctor")
  const tone = findingTone(finding.level)
  const Icon = TONE_ICON[tone]

  return (
    <li data-tone={tone} className="flex items-start gap-3 px-4 py-3">
      <Icon
        className={cn("mt-0.5 size-4 shrink-0", toneClass(tone))}
        aria-hidden="true"
      />
      <span className="sr-only">{t(`level.${tone}`)}</span>
      <div className="flex min-w-0 flex-col gap-1">
        <div className="flex flex-wrap items-baseline gap-2">
          <span className="font-medium text-sm">{finding.title}</span>
          <span className="rounded bg-muted px-1.5 py-0.5 font-mono text-muted-foreground text-xs">
            {finding.area}
          </span>
        </div>
        {finding.detail ? (
          <p className="text-muted-foreground text-sm">{finding.detail}</p>
        ) : null}
      </div>
    </li>
  )
}

function DoctorSkeleton() {
  const { t } = useTranslation("doctor")
  return (
    <div className="flex flex-col gap-6" aria-busy="true" aria-label={t("loading")}>
      <Skeleton className="h-7 w-48" />
      <Skeleton className="h-4 w-72" />
      <div className="flex flex-col gap-2 rounded-lg border p-4">
        <Skeleton className="h-5 w-full" />
        <Skeleton className="h-5 w-4/5" />
        <Skeleton className="h-5 w-3/5" />
      </div>
    </div>
  )
}

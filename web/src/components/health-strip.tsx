import type { Doctor, Page, QueueEntry, Workload } from "@/api/types"
import { type Tone, toneClass } from "@/components/run-status"
import { addNamespace } from "@/i18n"
import overview from "@/i18n/locales/en/overview.json"
import { cn } from "@/lib/utils"
import { CheckCircle2, XCircle } from "lucide-react"
import { useTranslation } from "react-i18next"

addNamespace("overview", overview)

/**
 * The one line that answers "is anything wrong?" before anything is read.
 *
 * It stays on screen even when there is nothing else to show: on day one the
 * cluster is reachable and the inventory is known, and that is worth saying.
 *
 * Colour comes from toneClass, never from a class written here - the state
 * vocabulary is defined once, in run-status.tsx.
 */
export function HealthStrip({
  doctor,
  queue,
  workloads,
}: {
  doctor: Doctor
  queue: Page<QueueEntry>
  workloads: Page<Workload>
}) {
  const { t } = useTranslation("overview")
  const tone: Tone = doctor.ok ? "success" : "failed"
  const Icon = doctor.ok ? CheckCircle2 : XCircle

  return (
    <div className="flex flex-wrap items-center gap-x-6 gap-y-2 rounded-lg border bg-card px-4 py-3 text-sm">
      <span
        className={cn("inline-flex items-center gap-2 font-medium", toneClass(tone))}
      >
        <Icon aria-hidden="true" className="size-4 shrink-0" />
        {doctor.ok ? t("health.ok") : t("health.problems", { count: doctor.problems })}
      </span>
      <span className="text-muted-foreground">
        {t("health.workloads", { count: workloads.items.length })}
      </span>
      <span className="text-muted-foreground">
        {t("health.running", { count: queue.items.length })}
      </span>
    </div>
  )
}

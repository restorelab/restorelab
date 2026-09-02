import { type Tone, toneClass } from "@/components/run-status"
import { cn } from "@/lib/utils"
import { useTranslation } from "react-i18next"

/**
 * The Recovery Confidence score.
 *
 * null is not zero. A workload that has never been drilled has no score, and
 * rendering that as 0% would report a measurement nobody took - it reads as
 * "we tested it and it is terrible" when the truth is "we have never tried".
 * The Go DTO makes the distinction with a nullable field; this honours it.
 */
export function ConfidenceScore({
  value,
  tested,
}: {
  value: number | null
  tested: boolean
}) {
  const { t } = useTranslation()

  if (!tested || value === null) {
    return (
      <span className="tabular text-muted-foreground" title={t("common.never")}>
        {t("common.none")}
      </span>
    )
  }

  const tone: Tone = value >= 80 ? "success" : value >= 50 ? "warning" : "failed"
  return (
    <span className="inline-flex items-center gap-2">
      <span
        role="meter"
        data-tone={tone}
        aria-valuenow={value}
        aria-valuemin={0}
        aria-valuemax={100}
        className="h-1.5 w-16 overflow-hidden rounded-full bg-muted"
      >
        <span
          className={cn("block h-full bg-current", toneClass(tone))}
          style={{ width: `${value}%` }}
        />
      </span>
      <span className={cn("tabular text-sm", toneClass(tone))}>{value}</span>
    </span>
  )
}

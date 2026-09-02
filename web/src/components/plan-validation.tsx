import { ApiError } from "@/api/client"
import type { Validated } from "@/api/types"
import { addNamespace } from "@/i18n"
import plans from "@/i18n/locales/en/plans.json"
import { useTranslation } from "react-i18next"

addNamespace("plans", plans)

/** One labelled fact about what a document means. */
function Fact({ label, value }: { label: string; value?: string }) {
  if (!value) return null
  return (
    <div className="flex gap-2 text-sm">
      <span className="text-muted-foreground">{label}</span>
      <span>{value}</span>
    </div>
  )
}

/**
 * What the server says a document means, or why it refuses it.
 *
 * Every word here comes from POST /plans/validate. No rule about what makes a
 * plan valid is written in TypeScript: internal/plan is the only definition,
 * and a second one here would drift from it at the first field added on the
 * other side - which is the same argument that put catalog between the API
 * and the store.
 */
export function PlanValidation({
  result,
  error,
  pending,
}: {
  result?: Validated
  error?: unknown
  pending?: boolean
}) {
  const { t } = useTranslation("plans")

  if (pending) {
    return <p className="text-muted-foreground text-sm">{t("validation.checking")}</p>
  }

  if (error) {
    return (
      <div className="space-y-1">
        <p className="font-medium text-sm text-state-failed">
          {t("validation.invalid")}
        </p>
        {/* The detail is the half that names the field. Replacing it with a
            generic "invalid" would throw away the only useful part. */}
        <p className="whitespace-pre-wrap text-sm">
          {error instanceof ApiError ? (error.detail ?? error.title) : String(error)}
        </p>
      </div>
    )
  }

  if (!result) return null

  return (
    <div className="space-y-3">
      <p className="font-medium text-sm text-state-success">{t("validation.valid")}</p>

      <div className="space-y-1">
        <p className="font-medium text-muted-foreground text-xs uppercase tracking-wide">
          {t("validation.means")}
        </p>
        <Fact label={t("validation.name")} value={result.name} />
        <Fact label={t("validation.workload")} value={result.workload_id} />
        <Fact label={t("validation.provider")} value={result.provider_id} />
      </div>

      <div className="space-y-1">
        <p className="font-medium text-muted-foreground text-xs uppercase tracking-wide">
          {t("validation.normalized")}
        </p>
        <p className="text-muted-foreground text-xs">
          {t("validation.normalizedHint")}
        </p>
        <pre className="max-h-64 overflow-auto rounded-md border bg-muted p-3 font-mono text-xs">
          {result.normalized_yaml}
        </pre>
      </div>
    </div>
  )
}

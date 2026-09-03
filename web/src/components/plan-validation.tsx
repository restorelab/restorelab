import { ApiError } from "@/api/client"
import type { Validated } from "@/api/types"
import { ProofBadge } from "@/components/run-status"
import { addNamespace } from "@/i18n"
import plans from "@/i18n/locales/en/plans.json"
import { useTranslation } from "react-i18next"

addNamespace("plans", plans)

/**
 * The proof summary with its level prefix removed.
 *
 * The server words it as "SERVICE, the service would be verified, the data
 * would not" - one line for a CLI, where there is no badge to carry the level.
 * Here the badge already says SERVICE, so repeating it would read as a stutter.
 * Anything that does not start with the level it declares is left exactly as
 * the server wrote it: the wording is the API's business, not this file's.
 */
export function proofSentence(result: Validated): string | undefined {
  const summary = result.proof_summary
  if (!summary) return undefined
  const prefix = result.proof_level ? `${result.proof_level}, ` : ""
  return prefix && summary.startsWith(prefix) ? summary.slice(prefix.length) : summary
}

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

  const proves = proofSentence(result)

  return (
    <div className="space-y-3">
      <p className="font-medium text-sm text-state-success">{t("validation.valid")}</p>

      {/* "Valid" and "worth running" are different questions, and until this
          block existed only the first one had an answer here. This is also the
          only place `proves:` is ever discovered: a plan that would prove
          nothing beyond the boot says so while it is still being written,
          rather than after five minutes of drilling. */}
      {proves ? (
        <div className="space-y-1">
          <p className="font-medium text-muted-foreground text-xs uppercase tracking-wide">
            {t("validation.proof")}
          </p>
          <div className="flex flex-wrap items-center gap-2">
            <ProofBadge level={result.proof_level} />
            <span className="text-sm">{proves}</span>
          </div>
          <p className="text-muted-foreground text-xs">{t("validation.proofHint")}</p>
        </div>
      ) : null}

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

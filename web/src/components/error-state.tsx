import { ApiError } from "@/api/client"
import { Button } from "@/components/ui/button"
import { AlertTriangle } from "lucide-react"
import { useTranslation } from "react-i18next"

/**
 * A failure, in the server's own words.
 *
 * The API answers problem+json with a title written for a person; inventing a
 * friendlier one here would replace a specific sentence with a vague one.
 */
export function ErrorState({
  error,
  onRetry,
}: {
  error: unknown
  onRetry?: () => void
}) {
  const { t } = useTranslation()
  const title = error instanceof ApiError ? error.title : t("error.title")
  const detail =
    error instanceof ApiError
      ? error.detail
      : error instanceof Error
        ? error.message
        : undefined

  return (
    <div className="flex flex-col items-center gap-3 px-6 py-16 text-center">
      <AlertTriangle className="size-6 text-state-warning" aria-hidden="true" />
      <h2 className="font-medium text-base">{title}</h2>
      {detail ? (
        <p className="max-w-prose text-muted-foreground text-sm">{detail}</p>
      ) : null}
      {onRetry ? (
        <Button variant="outline" size="sm" onClick={onRetry}>
          {t("error.retry")}
        </Button>
      ) : null}
    </div>
  )
}

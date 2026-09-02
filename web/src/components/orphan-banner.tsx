import type { Workload } from "@/api/types"
import { CleanupWorkload } from "@/components/cleanup-workload"
import { Alert, AlertTitle } from "@/components/ui/alert"
import { addNamespace } from "@/i18n"
import actions from "@/i18n/locales/en/actions.json"
import { AlertTriangle } from "lucide-react"
import { useTranslation } from "react-i18next"

addNamespace("actions", actions)

/**
 * What a drill left behind on the cluster, and the way to remove it.
 *
 * It shows nothing when there is nothing, because a banner that is always
 * there is furniture and furniture is not read. It shows even to a session
 * that cannot act: an orphan is a machine burning storage on a production
 * cluster, and saying nothing would be worse than saying "not from here".
 */
export function OrphanBanner({
  orphans,
  canOperate,
}: {
  orphans: Workload[]
  canOperate: boolean
}) {
  const { t } = useTranslation("actions")

  if (orphans.length === 0) return null

  return (
    <Alert>
      <AlertTriangle className="size-4" aria-hidden="true" />
      <AlertTitle>{t("cleanup.banner", { count: orphans.length })}</AlertTitle>
      <ul className="mt-3 flex flex-col gap-2">
        {orphans.map((w) => (
          <li key={w.id} className="flex flex-wrap items-center gap-3 text-sm">
            <span className="tabular font-medium">{w.id}</span>
            <span className="text-muted-foreground">{w.name}</span>
            {w.node ? <span className="text-muted-foreground">{w.node}</span> : null}
            <span className="ml-auto">
              <CleanupWorkload workload={w} canOperate={canOperate} />
            </span>
          </li>
        ))}
      </ul>
    </Alert>
  )
}

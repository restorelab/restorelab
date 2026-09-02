import { ApiError } from "@/api/client"
import { useCleanupWorkload } from "@/api/mutations"
import type { Workload } from "@/api/types"
import { ConfirmDialog } from "@/components/confirm-dialog"
import { Button } from "@/components/ui/button"
import { addNamespace } from "@/i18n"
import actions from "@/i18n/locales/en/actions.json"
import { useState } from "react"
import { useTranslation } from "react-i18next"

addNamespace("actions", actions)

/**
 * Destroys a temporary workload a drill left behind.
 *
 * This is the one irreversible button in the dashboard, and the dialog names
 * what it will destroy: the id, the machine's name, its node, and the drill
 * that left it. There is no id to retype - the backend has already refused
 * anything outside its reserved range and anything it does not own, so
 * whoever reaches this button has been filtered twice, and a string typed by
 * reflex would be theatre rather than a safeguard.
 */
export function CleanupWorkload({
  workload,
  canOperate,
}: {
  workload: Workload
  canOperate: boolean
}) {
  const { t } = useTranslation("actions")
  const [open, setOpen] = useState(false)
  const cleanup = useCleanupWorkload()

  if (!canOperate) return null

  const provenance = workload.recovery_run_id
    ? t("cleanup.fromRun", { run: workload.recovery_run_id })
    : t("cleanup.fromNoRun")

  return (
    <>
      <Button variant="destructive" size="sm" onClick={() => setOpen(true)}>
        {t("cleanup.button")}
      </Button>

      <ConfirmDialog
        open={open}
        onOpenChange={(next) => {
          setOpen(next)
          if (!next) cleanup.reset()
        }}
        title={t("cleanup.title", { id: workload.id })}
        description={`${t("cleanup.willDestroy", {
          id: workload.id,
          name: workload.name,
          node: workload.node ?? "?",
        })} ${provenance}`}
        confirmLabel={t("cleanup.submit")}
        tone="destructive"
        pending={cleanup.isPending}
        error={
          cleanup.error instanceof ApiError
            ? (cleanup.error.detail ?? cleanup.error.title)
            : undefined
        }
        onConfirm={() =>
          cleanup.mutate(workload.id, { onSuccess: () => setOpen(false) })
        }
      />
    </>
  )
}

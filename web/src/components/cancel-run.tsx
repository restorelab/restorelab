import { ApiError } from "@/api/client"
import { useCancelRun as useCancelRunMutation } from "@/api/mutations"
import { type RunDocument, isTerminal } from "@/api/types"
import { ConfirmDialog } from "@/components/confirm-dialog"
import { Button } from "@/components/ui/button"
import { addNamespace } from "@/i18n"
import actions from "@/i18n/locales/en/actions.json"
import { useState } from "react"
import { useTranslation } from "react-i18next"

addNamespace("actions", actions)

/**
 * Stops a drill in flight.
 *
 * It lives on this screen and nowhere else, on purpose. The dialog has to name
 * the machine it is about to destroy, and only the run document carries it:
 * GET /recovery-runs/{id} answers with temp_workload_id, temp_name and node,
 * while the listing - built from store.RunSummary - has none of them. The
 * queue on the overview links here rather than offering a button that could
 * not say what it would destroy.
 */
export function CancelRun({
  run,
  canOperate,
}: {
  run: RunDocument
  canOperate: boolean
}) {
  const { t } = useTranslation("actions")
  const [open, setOpen] = useState(false)
  const cancel = useCancelRunMutation(run.run_id)

  if (!canOperate || isTerminal(run.state)) return null

  // 200 and 202 are different states of the world, and the wording follows the
  // status rather than the mere fact that a request succeeded: 200 means the
  // drill was still queued and is now over, 202 means a worker was told and is
  // still tearing the temporary workload down. Reporting "done" on a 202 would
  // announce a machine gone while it still exists.
  const outcome = cancel.data
    ? cancel.data.status === 200
      ? t("cancel.stopped")
      : t("cancel.stopping")
    : undefined

  const willDo = run.temp_workload_id
    ? t("cancel.willDestroy", {
        id: run.temp_workload_id,
        node: run.node ?? "?",
      })
    : t("cancel.nothingYet")

  return (
    <>
      <Button variant="outline" size="sm" onClick={() => setOpen(true)}>
        {t("cancel.button")}
      </Button>

      <ConfirmDialog
        open={open}
        onOpenChange={(next) => {
          setOpen(next)
          // A dialog reopened on last time's answer would report a drill
          // stopped that is running again.
          if (!next) cancel.reset()
        }}
        title={t("cancel.title")}
        // The dialog stays open after the answer and its description becomes
        // the outcome: one that closed on a 202 would leave the viewer
        // believing the machine is already gone.
        description={outcome ?? willDo}
        confirmLabel={t("cancel.submit")}
        tone="destructive"
        pending={cancel.isPending}
        error={cancel.error instanceof ApiError ? cancel.error.title : undefined}
        onConfirm={() => cancel.mutate()}
      />
    </>
  )
}

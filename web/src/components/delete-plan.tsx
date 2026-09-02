import { ApiError } from "@/api/client"
import { useDeletePlan } from "@/api/mutations"
import type { Plan } from "@/api/types"
import { ConfirmDialog } from "@/components/confirm-dialog"
import { Button } from "@/components/ui/button"
import { addNamespace } from "@/i18n"
import plans from "@/i18n/locales/en/plans.json"
import { useState } from "react"
import { useTranslation } from "react-i18next"

addNamespace("plans", plans)

/**
 * Removes a plan from the catalogue.
 *
 * The dialog's useful sentence is the reassuring one, because it is the
 * counter-intuitive one: past drills keep their name and the exact document
 * they ran. B3 proved it - after a delete, a run's report differs by exactly
 * one field, plan_id, and nothing else moves. A drill in flight executes the
 * snapshot taken when it was queued, never the catalogue row.
 *
 * Saying so matters: letting somebody believe they are about to lose history
 * would make them keep a plan they wanted gone.
 */
export function DeletePlan({
  plan,
  canManage,
  onDeleted,
}: {
  plan: Plan
  canManage: boolean
  onDeleted: () => void
}) {
  const { t } = useTranslation("plans")
  const [open, setOpen] = useState(false)
  const remove = useDeletePlan()

  if (!canManage) return null

  return (
    <>
      <Button variant="destructive" size="sm" onClick={() => setOpen(true)}>
        {t("delete.button")}
      </Button>

      <ConfirmDialog
        open={open}
        onOpenChange={(next) => {
          setOpen(next)
          if (!next) remove.reset()
        }}
        title={t("delete.title", { name: plan.name })}
        description={`${t("delete.description")} ${t("delete.keeps")}`}
        confirmLabel={t("delete.submit")}
        tone="destructive"
        pending={remove.isPending}
        error={
          remove.error instanceof ApiError
            ? (remove.error.detail ?? remove.error.title)
            : undefined
        }
        onConfirm={() => remove.mutate(plan.name, { onSuccess: onDeleted })}
      />
    </>
  )
}

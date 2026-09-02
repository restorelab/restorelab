import { Button } from "@/components/ui/button"
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog"
import { addNamespace } from "@/i18n"
import actions from "@/i18n/locales/en/actions.json"
import { useTranslation } from "react-i18next"

addNamespace("actions", actions)

/**
 * The shell both destructive confirmations share.
 *
 * It names what is about to happen in its description and never asks "are you
 * sure": a box that states the consequence informs, a box that asks for a
 * confirmation people give by reflex does nothing at all. The caller writes
 * that sentence, because only the caller knows the machine's name.
 *
 * It also stays open on a refusal. A dialog that closes on an error the
 * viewer never read is a dialog that lies about having worked.
 */
export function ConfirmDialog({
  open,
  onOpenChange,
  title,
  description,
  confirmLabel,
  tone = "default",
  pending = false,
  error,
  onConfirm,
}: {
  open: boolean
  onOpenChange: (open: boolean) => void
  title: string
  description: string
  confirmLabel: string
  tone?: "default" | "destructive"
  pending?: boolean
  error?: string
  onConfirm: () => void
}) {
  const { t } = useTranslation("actions")

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>{title}</DialogTitle>
          <DialogDescription>{description}</DialogDescription>
        </DialogHeader>
        {error ? <p className="text-sm text-state-failed">{error}</p> : null}
        <DialogFooter>
          <Button
            variant="outline"
            onClick={() => onOpenChange(false)}
            disabled={pending}
          >
            {t("dismiss")}
          </Button>
          <Button
            variant={tone === "destructive" ? "destructive" : "default"}
            onClick={onConfirm}
            disabled={pending}
          >
            {pending ? t("working") : confirmLabel}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

import { ApiError } from "@/api/client"
import { type TriggerBody, useTriggerDrill } from "@/api/mutations"
import type { Backup, Workload } from "@/api/types"
import { AppLink } from "@/components/app-link"
import { Button } from "@/components/ui/button"
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { addNamespace } from "@/i18n"
import actions from "@/i18n/locales/en/actions.json"
import { type ChangeEvent, useState } from "react"
import { useTranslation } from "react-i18next"

addNamespace("actions", actions)

/** The ad-hoc fields, as the form holds them: strings, empty when untouched. */
export interface TriggerOptions {
  backup: string
  checks: string
  network: string
  node: string
  storage: string
  pool: string
  rto_target: string
}

const emptyOptions: TriggerOptions = {
  backup: "",
  checks: "",
  network: "",
  node: "",
  storage: "",
  pool: "",
  rto_target: "",
}

/** The plain fields, in the order the panel shows them. */
const PLAIN_FIELDS = [
  "backup",
  "network",
  "node",
  "storage",
  "pool",
  "rto_target",
] as const

/**
 * Builds the request body from the form.
 *
 * A field left alone is left out entirely rather than sent empty: the server
 * resolves its own defaults - the latest backup, the isolated network, the
 * configured node, storage and pool - and it resolves them better than this
 * form can guess them.
 */
export function triggerBody(workloadID: string, o: TriggerOptions): TriggerBody {
  const body: TriggerBody = { workload_id: workloadID }
  if (o.backup) body.backup = o.backup
  if (o.network) body.network = o.network
  if (o.node) body.node = o.node
  if (o.storage) body.storage = o.storage
  if (o.pool) body.pool = o.pool
  if (o.rto_target) body.rto_target = o.rto_target

  const checks = o.checks
    .split("\n")
    .map((c) => c.trim())
    .filter(Boolean)
  if (checks.length > 0) body.checks = checks

  return body
}

/**
 * The button that starts a drill, and the panel that says what it will do.
 *
 * The panel is the confirmation. It names the backup, the isolated network and
 * what happens to the clone afterwards, all before anything is posted; there
 * is no second box asking whether the viewer is sure, because a confirmation
 * given by reflex confirms nothing.
 *
 * Nothing at all is rendered without the operate scope - not a disabled
 * button. A dead control nobody explains sends people looking for the fault in
 * the wrong place.
 *
 * `backups` may be empty: a listing cannot afford one backups request per row,
 * so it passes none and the panel says "the most recent backup" rather than
 * naming it. A detail screen, which already loads them, names it.
 */
export function TriggerDrill({
  workload,
  backups,
  canOperate,
  activeRunID,
  onStarted,
}: {
  workload: Workload
  backups: Backup[]
  canOperate: boolean
  activeRunID?: string
  onStarted: (runID: string) => void
}) {
  const { t } = useTranslation("actions")
  const [open, setOpen] = useState(false)
  const [options, setOptions] = useState<TriggerOptions>(emptyOptions)
  const trigger = useTriggerDrill()

  if (!canOperate) return null

  // The backend answers 409 for a second drill on the same workload. This does
  // not wait for that refusal: it points at the drill already running.
  if (activeRunID) {
    return (
      <span className="text-muted-foreground text-sm">
        {t("trigger.alreadyRunning")}{" "}
        <AppLink
          to="/runs/$id"
          params={{ id: activeRunID }}
          className="underline-offset-4 hover:underline"
        >
          {t("trigger.viewRunning")}
        </AppLink>
      </span>
    )
  }

  const [latest] = backups
  const err = trigger.error

  function set(key: keyof TriggerOptions) {
    return (e: ChangeEvent<HTMLInputElement | HTMLTextAreaElement>) =>
      setOptions((o) => ({ ...o, [key]: e.target.value }))
  }

  return (
    <>
      <Button size="sm" onClick={() => setOpen(true)}>
        {t("trigger.button")}
      </Button>

      <Dialog
        open={open}
        onOpenChange={(next) => {
          setOpen(next)
          if (!next) trigger.reset()
        }}
      >
        <DialogContent>
          <DialogHeader>
            <DialogTitle>{t("trigger.title", { name: workload.name })}</DialogTitle>
            <DialogDescription>
              {latest
                ? t("trigger.willRestore", { backup: latest.id })
                : t("trigger.willRestoreLatest")}
            </DialogDescription>
          </DialogHeader>

          <details className="rounded-md border px-3 py-2">
            <summary className="cursor-pointer text-sm">{t("trigger.options")}</summary>
            <div className="mt-3 flex flex-col gap-3">
              {PLAIN_FIELDS.map((key) => (
                <div key={key} className="flex flex-col gap-1">
                  <Label htmlFor={`trigger-${key}`}>{key}</Label>
                  <Input
                    id={`trigger-${key}`}
                    value={options[key]}
                    onChange={set(key)}
                  />
                </div>
              ))}
              <div className="flex flex-col gap-1">
                <Label htmlFor="trigger-checks">checks</Label>
                {/* A textarea rather than an Input: one check per line is the
                    only shape that stays readable past the second one. */}
                <textarea
                  id="trigger-checks"
                  rows={3}
                  className="rounded-md border bg-transparent px-3 py-2 text-sm shadow-xs outline-none focus-visible:ring-[3px]"
                  value={options.checks}
                  onChange={set("checks")}
                />
              </div>
            </div>
          </details>

          {err ? (
            <p className="text-sm text-state-failed">
              {err instanceof ApiError
                ? `${err.title}${err.detail ? ` — ${err.detail}` : ""}`
                : String(err)}
            </p>
          ) : null}

          <DialogFooter>
            <Button variant="outline" onClick={() => setOpen(false)}>
              {t("dismiss")}
            </Button>
            <Button
              disabled={trigger.isPending}
              onClick={() =>
                trigger.mutate(triggerBody(workload.id, options), {
                  onSuccess: (run) => {
                    setOpen(false)
                    onStarted(run.id)
                  },
                })
              }
            >
              {trigger.isPending ? t("working") : t("trigger.submit")}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </>
  )
}

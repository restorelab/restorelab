import { ApiError } from "@/api/client"
import { useCreatePlan, useUpdatePlan, validatePlan } from "@/api/mutations"
import type { Plan, Validated } from "@/api/types"
import { PlanValidation } from "@/components/plan-validation"
import { Button } from "@/components/ui/button"
import { addNamespace } from "@/i18n"
import plans from "@/i18n/locales/en/plans.json"
import { useEffect, useState } from "react"
import { useTranslation } from "react-i18next"

addNamespace("plans", plans)

/** How long the editor waits after a keystroke before asking the server. */
const VALIDATE_DEBOUNCE_MS = 400

/**
 * Which refusal a 409 is.
 *
 * PUT /plans/{ref} answers 409 for two unrelated reasons, and they need two
 * different screens: somebody else saved, or the document renames the plan.
 * The problem type is what distinguishes them - matching on the title would
 * break the day somebody improves the wording.
 */
export function conflictKind(error: unknown): "version" | "rename" | null {
  if (!(error instanceof ApiError) || error.status !== 409) return null
  if (error.type.endsWith("version-conflict")) return "version"
  if (error.type.endsWith("rename-not-supported")) return "rename"
  return null
}

/**
 * The plan editor: a textarea, and what the server says about it.
 *
 * The document is sent verbatim - never recomposed from parsed fields - which
 * is what makes a human's comments survive a round trip through this screen.
 *
 * `plan` absent means creating. Present, it carries the version this editor
 * loaded, which goes back as the ?version= guard: a dashboard knows which
 * version it is editing, and omitting the guard is what a CI pipeline does
 * because it genuinely does not.
 */
export function PlanEditor({
  initialDocument,
  plan,
  onSaved,
  onTakeTheirs,
}: {
  initialDocument: string
  plan?: Plan
  onSaved: (saved: Plan) => void
  /** Called when the viewer chooses the server's version over their own. */
  onTakeTheirs?: () => void
}) {
  const { t } = useTranslation("plans")
  const [document, setDocument] = useState(initialDocument)
  const [checked, setChecked] = useState<Validated | undefined>(undefined)
  const [checkError, setCheckError] = useState<unknown>(undefined)
  const [checking, setChecking] = useState(false)

  const create = useCreatePlan()
  const update = useUpdatePlan(plan?.name ?? "")
  const saving = create.isPending || update.isPending
  const saveError = plan ? update.error : create.error
  const conflict = conflictKind(saveError)

  // One request for a burst of typing. This endpoint parses a document on
  // every call, and a request per keystroke would ask the server to parse a
  // half-written plan thirty times to answer about the thirty-first.
  useEffect(() => {
    if (document.trim() === "") {
      setChecked(undefined)
      setCheckError(undefined)
      return
    }
    let live = true
    setChecking(true)
    const timer = window.setTimeout(() => {
      validatePlan(document)
        .then((result) => {
          if (!live) return
          setChecked(result)
          setCheckError(undefined)
        })
        .catch((err: unknown) => {
          if (!live) return
          setChecked(undefined)
          setCheckError(err)
        })
        .finally(() => {
          if (live) setChecking(false)
        })
    }, VALIDATE_DEBOUNCE_MS)

    return () => {
      live = false
      window.clearTimeout(timer)
    }
  }, [document])

  function save(version?: number) {
    if (plan) {
      update.mutate({ document, version }, { onSuccess: onSaved })
      return
    }
    create.mutate(document, { onSuccess: onSaved })
  }

  return (
    <div className="grid gap-4 lg:grid-cols-2">
      <div className="flex flex-col gap-3">
        <textarea
          aria-label={t("edit")}
          className="min-h-96 rounded-md border bg-transparent p-3 font-mono text-sm outline-none focus-visible:ring-[3px]"
          spellCheck={false}
          value={document}
          onChange={(e) => setDocument(e.target.value)}
        />
        <div>
          <Button disabled={saving} onClick={() => save(plan?.version)}>
            {saving ? t("saving") : t("save")}
          </Button>
        </div>
      </div>

      <div className="flex flex-col gap-4">
        <PlanValidation result={checked} error={checkError} pending={checking} />

        {conflict === "version" ? (
          <div className="space-y-3 rounded-md border p-3">
            <p className="font-medium text-sm">{t("conflict.title")}</p>
            <p className="text-muted-foreground text-sm">{t("conflict.description")}</p>
            <div className="flex flex-wrap gap-2">
              {/* Nothing replaces what somebody has been typing unless they
                  ask for it: reloading is the caller's business, and both
                  ways out are offered rather than one being taken quietly. */}
              <Button variant="outline" onClick={() => onTakeTheirs?.()}>
                {t("conflict.takeTheirs")}
              </Button>
              <Button variant="destructive" onClick={() => save(undefined)}>
                {t("conflict.overwrite")}
              </Button>
            </div>
          </div>
        ) : null}

        {conflict === "rename" ? (
          <div className="space-y-2 rounded-md border p-3">
            <p className="font-medium text-sm">{t("rename.title")}</p>
            <p className="text-muted-foreground text-sm">{t("rename.description")}</p>
            {saveError instanceof ApiError && saveError.detail ? (
              <p className="text-sm">{saveError.detail}</p>
            ) : null}
          </div>
        ) : null}

        {saveError && conflict === null ? (
          <p className="text-sm text-state-failed">
            {saveError instanceof ApiError ? saveError.title : String(saveError)}
          </p>
        ) : null}
      </div>
    </div>
  )
}

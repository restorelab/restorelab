import type { SetupStep } from "@/api/types"
import { addNamespace } from "@/i18n"
import setup from "@/i18n/locales/en/setup.json"
import { useTranslation } from "react-i18next"

addNamespace("setup", setup)

/**
 * What provisioning did, in the order it did it.
 *
 * It renders after a failure as well as after a success, and that is the
 * point: the server returns the steps it got through either way, every one of
 * them is idempotent, and knowing where it stopped is what turns "fix it and
 * run it again" into an instruction somebody can follow.
 */
export function SetupSteps({ steps }: { steps: SetupStep[] }) {
  const { t } = useTranslation("setup")

  if (steps.length === 0) return null

  return (
    <div className="space-y-2">
      <p className="font-medium text-muted-foreground text-xs uppercase tracking-wide">
        {t("steps.title")}
      </p>
      <ul className="space-y-1">
        {steps.map((step) => (
          <li key={step.description} className="flex flex-wrap gap-2 text-sm">
            <span>{step.description}</span>
            <span className="text-muted-foreground">{step.status}</span>
            {step.detail ? (
              <span className="w-full text-muted-foreground text-xs">
                {step.detail}
              </span>
            ) : null}
          </li>
        ))}
      </ul>
    </div>
  )
}

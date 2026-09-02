import { Button } from "@/components/ui/button"
import { Check, Copy } from "lucide-react"
import { type ReactNode, useEffect, useRef, useState } from "react"
import { useTranslation } from "react-i18next"

/**
 * What a screen shows when it has nothing to show.
 *
 * The optional command is the point: an empty dashboard on day one should hand
 * someone the next step, not report that a table is empty. C4 will replace
 * these commands with buttons; the explanations around them stay.
 */
export function EmptyState({
  title,
  description,
  command,
  action,
  icon,
}: {
  title: string
  description?: string
  command?: string
  /**
   * The button that does the thing, when there is one.
   *
   * It sits above the command rather than replacing it: someone who drives
   * this from a terminal must not lose the line they were going to copy.
   */
  action?: ReactNode
  icon?: ReactNode
}) {
  const { t } = useTranslation()
  const [copied, setCopied] = useState(false)
  const timer = useRef<number | undefined>(undefined)

  useEffect(() => () => window.clearTimeout(timer.current), [])

  async function copy() {
    if (!command) return
    await navigator.clipboard.writeText(command)
    setCopied(true)
    timer.current = window.setTimeout(() => setCopied(false), 2000)
  }

  return (
    <div className="flex flex-col items-center gap-3 px-6 py-16 text-center">
      {icon ? <div className="text-muted-foreground">{icon}</div> : null}
      <h2 className="font-medium text-base">{title}</h2>
      {description ? (
        <p className="max-w-prose text-muted-foreground text-sm">{description}</p>
      ) : null}
      {action ? <div className="mt-2">{action}</div> : null}
      {command ? (
        <div className="mt-2 flex items-center gap-2 rounded-md border bg-muted px-3 py-2">
          <code className="font-mono text-sm">{command}</code>
          <Button
            variant="ghost"
            size="sm"
            onClick={copy}
            aria-label={copied ? t("common.copied") : t("common.copy")}
          >
            {copied ? <Check className="size-4" /> : <Copy className="size-4" />}
          </Button>
        </div>
      ) : null}
    </div>
  )
}

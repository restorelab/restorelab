import { ApiError, apiGet, apiSend, apiSendWithToken } from "@/api/client"
import type { Session, SetupOutcome, SetupRequest, SetupStep } from "@/api/types"
import { EmptyState } from "@/components/empty-state"
import { SetupSteps } from "@/components/setup-steps"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { addNamespace } from "@/i18n"
import setup from "@/i18n/locales/en/setup.json"
import { useMutation } from "@tanstack/react-query"
import { createFileRoute, useNavigate } from "@tanstack/react-router"
import { KeyRound } from "lucide-react"
import { useEffect, useState } from "react"
import { useTranslation } from "react-i18next"

addNamespace("setup", setup)

export const Route = createFileRoute("/setup")({
  // The key is absent rather than undefined when the address carried none, so
  // the screen can tell "no token" from "empty token" without a placeholder.
  validateSearch: (s: Record<string, unknown>): { token?: string } =>
    typeof s.token === "string" ? { token: s.token } : {},
  component: SetupPage,
})

/** What the bridge screen offers, and what each choice sends. */
type BridgeChoice = "create" | "defer" | "skip"

/** The form's fields, as strings, the way a form holds them. */
export interface SetupFields {
  endpoint: string
  adminUser: string
  adminPassword: string
  storages: string
  insecure: boolean
  bridge: BridgeChoice
}

const emptyFields: SetupFields = {
  endpoint: "",
  adminUser: "root@pam",
  adminPassword: "",
  storages: "",
  insecure: true,
  bridge: "create",
}

/**
 * Builds the one request the wizard gets to send.
 *
 * One, because the setup token on the console is spent by it: everything the
 * installation needs has to be collected before it leaves. That is why the
 * bridge is a field here rather than a second screen with a second call.
 */
export function setupRequest(f: SetupFields): SetupRequest {
  return {
    endpoint: f.endpoint.trim(),
    admin_user: f.adminUser.trim(),
    admin_password: f.adminPassword,
    storages: f.storages
      .split("\n")
      .map((s) => s.trim())
      .filter(Boolean),
    insecure: f.insecure,
    create_bridge: f.bridge !== "skip",
    apply_bridge: f.bridge === "create",
  }
}

/**
 * The steps a refusal carried, if it carried any.
 *
 * They ride on the problem document rather than on ApiError's own fields,
 * because only this endpoint has them - see ApiError.body.
 */
export function stepsOf(error: unknown): SetupStep[] {
  if (!(error instanceof ApiError)) return []
  const body = error.body as { steps?: SetupStep[] } | undefined
  return body?.steps ?? []
}

/**
 * The form, and the only request first-run setup ever sends.
 *
 * It refuses to submit without a storage: a RestoreLab installed without one
 * cannot run the drill it was installed for, and an installation that ends
 * that way has failed while looking like it succeeded.
 */
export function SetupForm({
  token,
  onDone,
}: {
  token: string
  onDone: (outcome: SetupOutcome) => void
}) {
  const { t } = useTranslation("setup")
  const [fields, setFields] = useState<SetupFields>(emptyFields)

  const connect = useMutation({
    mutationFn: (body: SetupRequest) =>
      apiSendWithToken<SetupOutcome>("/setup", token, body),
    onSuccess: onDone,
  })

  const body = setupRequest(fields)
  const ready = body.endpoint !== "" && body.storages.length > 0
  const failure = connect.error

  function set<K extends keyof SetupFields>(key: K, value: SetupFields[K]) {
    setFields((f) => ({ ...f, [key]: value }))
  }

  return (
    <form
      className="mx-auto flex w-full max-w-xl flex-col gap-5"
      onSubmit={(e) => {
        e.preventDefault()
        if (!ready) return
        connect.mutate(body)
      }}
    >
      <header className="space-y-2">
        <h1 className="font-semibold text-lg">{t("title")}</h1>
        <p className="text-muted-foreground text-sm">{t("intro")}</p>
      </header>

      <div className="flex flex-col gap-2">
        <Label htmlFor="endpoint">{t("form.endpoint")}</Label>
        <Input
          id="endpoint"
          value={fields.endpoint}
          placeholder={t("form.endpointHint")}
          onChange={(e) => set("endpoint", e.target.value)}
        />
      </div>

      <div className="flex flex-col gap-2">
        <Label htmlFor="admin-user">{t("form.adminUser")}</Label>
        <Input
          id="admin-user"
          value={fields.adminUser}
          onChange={(e) => set("adminUser", e.target.value)}
        />
      </div>

      <div className="flex flex-col gap-2">
        <Label htmlFor="admin-password">{t("form.adminPassword")}</Label>
        <Input
          id="admin-password"
          type="password"
          autoComplete="off"
          value={fields.adminPassword}
          onChange={(e) => set("adminPassword", e.target.value)}
        />
        <p className="text-muted-foreground text-xs">{t("form.passwordHint")}</p>
      </div>

      <div className="flex flex-col gap-2">
        <Label htmlFor="storages">{t("form.storages")}</Label>
        <textarea
          id="storages"
          rows={2}
          className="rounded-md border bg-transparent px-3 py-2 font-mono text-sm outline-none focus-visible:ring-[3px]"
          value={fields.storages}
          onChange={(e) => set("storages", e.target.value)}
        />
        <p className="text-muted-foreground text-xs">{t("form.storagesHint")}</p>
      </div>

      <label className="flex items-center gap-2 text-sm">
        <input
          type="checkbox"
          checked={fields.insecure}
          onChange={(e) => set("insecure", e.target.checked)}
        />
        {t("form.insecure")}
      </label>

      <fieldset className="space-y-2 rounded-md border p-3">
        <legend className="px-1 font-medium text-sm">{t("bridge.legend")}</legend>
        <p className="text-muted-foreground text-sm">{t("bridge.description")}</p>
        {/* Named before it happens, like every other consequence in this
            dashboard: no existing interface is touched, and the node's
            network configuration is reloaded. */}
        <p className="text-muted-foreground text-xs">{t("bridge.consequence")}</p>
        {(["create", "defer", "skip"] as const).map((choice) => (
          <label key={choice} className="flex items-center gap-2 text-sm">
            <input
              type="radio"
              name="bridge"
              value={choice}
              checked={fields.bridge === choice}
              onChange={() => set("bridge", choice)}
            />
            {t(`bridge.${choice}`)}
          </label>
        ))}
      </fieldset>

      <p className="text-muted-foreground text-xs">{t("form.oneShot")}</p>

      {failure ? (
        <div className="space-y-3 rounded-md border p-3">
          <p className="font-medium text-sm text-state-failed">{t("failed.title")}</p>
          <p className="text-sm">
            {failure instanceof ApiError
              ? (failure.detail ?? failure.title)
              : String(failure)}
          </p>
          <SetupSteps steps={stepsOf(failure)} />
          <p className="text-muted-foreground text-sm">{t("failed.again")}</p>
          <p className="text-muted-foreground text-xs">{t("failed.spent")}</p>
        </div>
      ) : null}

      <Button type="submit" disabled={!ready || connect.isPending}>
        {connect.isPending ? t("form.working") : t("form.submit")}
      </Button>
    </form>
  )
}

/**
 * The last screen: wait for the server to come back, then open the session.
 *
 * The page is never reloaded, which is why the token can live in memory: the
 * setup server is being torn down and the real one opened on the same port,
 * and this polls until that has happened.
 */
export function Finishing({
  outcome,
  onReady,
}: {
  outcome: SetupOutcome
  onReady: () => void
}) {
  const { t } = useTranslation("setup")

  useEffect(() => {
    let live = true

    async function waitAndSignIn() {
      while (live) {
        try {
          await apiGet<unknown>("/health")
          await apiSend<Session>("POST", "/session", { token: outcome.token })
          if (live) onReady()
          return
        } catch {
          // The server is between two lives: its setup half has let the port
          // go and its configured half has not taken it yet. Asking again is
          // the whole of the strategy.
          await new Promise((r) => setTimeout(r, 500))
        }
      }
    }

    void waitAndSignIn()
    return () => {
      live = false
    }
  }, [outcome.token, onReady])

  return (
    <div className="mx-auto flex w-full max-w-xl flex-col gap-4">
      <h1 className="font-semibold text-lg">{t("finishing.title")}</h1>
      <p className="text-muted-foreground text-sm">{t("finishing.description")}</p>
      <SetupSteps steps={outcome.steps} />
    </div>
  )
}

function SetupPage() {
  const { t } = useTranslation("setup")
  const { token } = Route.useSearch()
  const navigate = useNavigate()
  const [outcome, setOutcome] = useState<SetupOutcome | undefined>(undefined)

  if (!token) {
    return (
      <div className="flex min-h-dvh items-center justify-center px-4">
        <EmptyState
          icon={<KeyRound className="size-6" aria-hidden="true" />}
          title={t("noToken.title")}
          description={t("noToken.description")}
        />
      </div>
    )
  }

  return (
    <div className="flex min-h-dvh items-center justify-center px-4 py-10">
      {outcome ? (
        <Finishing outcome={outcome} onReady={() => void navigate({ to: "/" })} />
      ) : (
        <SetupForm token={token} onDone={setOutcome} />
      )}
    </div>
  )
}

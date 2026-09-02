import { ApiError, apiSend } from "@/api/client"
import type { Session } from "@/api/types"
import { EmptyState } from "@/components/empty-state"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { addNamespace } from "@/i18n"
import auth from "@/i18n/locales/en/auth.json"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import { createFileRoute, useNavigate } from "@tanstack/react-router"
import { useState } from "react"
import { useTranslation } from "react-i18next"

addNamespace("auth", auth)

export const Route = createFileRoute("/login")({
  // The key is absent rather than undefined when there is nowhere to go back
  // to. That is what makes `redirect` optional at every navigate() call site -
  // a schema that always names the key makes it required, and every
  // `navigate({ to: "/login" })` in the app would have to carry a placeholder.
  validateSearch: (s: Record<string, unknown>): { redirect?: string } =>
    typeof s.redirect === "string" ? { redirect: s.redirect } : {},
  component: LoginPage,
})

/**
 * The sign-in form, kept apart from the route so it can be tested without a
 * router. It owns the request and nothing else.
 */
export function LoginForm({ onSignedIn }: { onSignedIn: () => void }) {
  const { t } = useTranslation("auth")
  const [token, setToken] = useState("")

  const signIn = useMutation({
    mutationFn: (secret: string) =>
      apiSend<Session>("POST", "/session", { token: secret }),
    onSuccess: onSignedIn,
  })

  const err = signIn.error
  // No history database means no tokens, which means there is nothing to sign
  // in with. That is not a wrong password, and saying so would send someone
  // hunting for a token that cannot exist.
  if (err instanceof ApiError && err.status === 503) {
    return (
      <EmptyState
        title={t("noHistory.title")}
        description={t("noHistory.description")}
        command={t("noHistory.command")}
      />
    )
  }

  return (
    <form
      className="mx-auto flex w-full max-w-sm flex-col gap-4"
      onSubmit={(e) => {
        e.preventDefault()
        signIn.mutate(token)
      }}
    >
      <h1 className="font-semibold text-lg">{t("title")}</h1>
      <div className="flex flex-col gap-2">
        <Label htmlFor="token">{t("tokenLabel")}</Label>
        <Input
          id="token"
          type="password"
          autoComplete="off"
          value={token}
          onChange={(e) => setToken(e.target.value)}
        />
        <p className="text-muted-foreground text-xs">{t("tokenHint")}</p>
      </div>
      {err ? (
        <p className="text-sm text-state-failed">
          {err instanceof ApiError ? err.title : String(err)}
        </p>
      ) : null}
      <Button type="submit" disabled={!token || signIn.isPending}>
        {signIn.isPending ? t("signingIn") : t("submit")}
      </Button>
    </form>
  )
}

function LoginPage() {
  const qc = useQueryClient()
  const navigate = useNavigate()
  const { redirect } = Route.useSearch()

  return (
    <div className="flex min-h-dvh items-center justify-center px-4">
      <LoginForm
        onSignedIn={async () => {
          await qc.invalidateQueries({ queryKey: ["session"] })
          await navigate({ to: redirect ?? "/" })
        }}
      />
    </div>
  )
}

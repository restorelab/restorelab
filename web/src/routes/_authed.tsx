import { UnauthorizedError, apiSend } from "@/api/client"
import { sessionQuery } from "@/api/queries"
import { AppShell } from "@/components/app-shell"
import { ErrorState } from "@/components/error-state"
import { useQueryClient } from "@tanstack/react-query"
import { Outlet, createFileRoute, redirect, useNavigate } from "@tanstack/react-router"

export const Route = createFileRoute("/_authed")({
  beforeLoad: async ({ context, location }) => {
    try {
      const session = await context.queryClient.ensureQueryData(sessionQuery())
      // Scopes go into the context now even though nothing in C2 reads them:
      // C3 adds buttons, and it must not have to retrofit a notion of
      // permission into every screen already written.
      return {
        scopes: session.scopes,
        can: (scope: string) => session.scopes.includes(scope),
      }
    } catch (err) {
      if (err instanceof UnauthorizedError) {
        throw redirect({ to: "/login", search: { redirect: location.pathname } })
      }
      throw err
    }
  },
  component: AuthedLayout,
  errorComponent: ({ error }) => <ErrorState error={error} />,
})

function AuthedLayout() {
  const qc = useQueryClient()
  const navigate = useNavigate()

  async function signOut() {
    try {
      await apiSend("DELETE", "/session")
    } finally {
      // Data read under a dead session must not stay on screen behind a login
      // form, so the cache is cleared rather than invalidated.
      qc.clear()
      await navigate({ to: "/login" })
    }
  }

  return (
    <AppShell onSignOut={signOut}>
      <Outlet />
    </AppShell>
  )
}

import { ErrorState } from "@/components/error-state"
import type { QueryClient } from "@tanstack/react-query"
import { Link, Outlet, createRootRouteWithContext } from "@tanstack/react-router"
import { useTranslation } from "react-i18next"

export interface RouterContext {
  queryClient: QueryClient
}

export const Route = createRootRouteWithContext<RouterContext>()({
  component: () => <Outlet />,
  // An exception in one screen must not take the application down with it.
  errorComponent: ({ error }) => <ErrorState error={error} />,
  notFoundComponent: NotFound,
})

function NotFound() {
  const { t } = useTranslation()
  return (
    <div className="flex flex-col items-center gap-3 px-6 py-16 text-center">
      <h2 className="font-medium text-base">{t("error.notFound")}</h2>
      <Link to="/" className="text-sm underline">
        {t("error.backHome")}
      </Link>
    </div>
  )
}

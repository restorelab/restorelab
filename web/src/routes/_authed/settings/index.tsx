import { ApiError } from "@/api/client"
import { notificationsQuery } from "@/api/queries"
import { EmptyState } from "@/components/empty-state"
import { ErrorState } from "@/components/error-state"
import { NotificationChannels } from "@/components/notification-channels"
import { addNamespace } from "@/i18n"
import notificationsLocale from "@/i18n/locales/en/notifications.json"
import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, useRouteContext } from "@tanstack/react-router"
import { BellOff } from "lucide-react"
import { useTranslation } from "react-i18next"

addNamespace("notifications", notificationsLocale)

export const Route = createFileRoute("/_authed/settings/")({
  loader: ({ context: { queryClient } }) =>
    queryClient.ensureQueryData(notificationsQuery()),
  component: SettingsPage,
  errorComponent: ({ error }) => {
    // A server started without a configuration file it can write back to
    // answers 503 rather than an empty list, because "no channel is
    // configured" and "this server cannot tell you" are different statements
    // and only the second one is true. That is a deployment to explain, not
    // an error to report - the same shape the catalogue's 503 takes.
    if (error instanceof ApiError && error.status === 503) {
      return <NoConfiguration />
    }
    return <ErrorState error={error} />
  },
})

function NoConfiguration() {
  const { t } = useTranslation("notifications")
  return (
    <EmptyState
      icon={<BellOff className="size-6" aria-hidden="true" />}
      title={t("unavailable.title")}
      description={t("unavailable.description")}
      command={t("unavailable.command")}
    />
  )
}

/**
 * The settings screen, which is the notification channels and nothing else yet.
 *
 * It is a route group rather than a single file because the next thing that
 * belongs here - tokens, the master key, the scheduler's switch - will be a
 * sibling, and moving a URL afterwards costs somebody's bookmark.
 */
function SettingsPage() {
  const channels = useSuspenseQuery(notificationsQuery()).data
  const { can } = useRouteContext({ from: "/_authed" })

  return (
    <NotificationChannels
      channels={channels}
      canManage={can("manage")}
      canOperate={can("operate")}
    />
  )
}

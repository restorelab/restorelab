import { providersQuery, workloadsQuery } from "@/api/queries"
import type { Workload } from "@/api/types"
import { AppLink } from "@/components/app-link"
import { EmptyState } from "@/components/empty-state"
import { ErrorState } from "@/components/error-state"
import { PlanEditor } from "@/components/plan-editor"
import { Button } from "@/components/ui/button"
import { Table, TableBody, TableCell, TableRow } from "@/components/ui/table"
import { addNamespace } from "@/i18n"
import plansLocale from "@/i18n/locales/en/plans.json"
import { defaultProviderID, planSkeleton } from "@/lib/plan-skeleton"
import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, useNavigate } from "@tanstack/react-router"
import { ArrowLeft, Server } from "lucide-react"
import { useState } from "react"
import { useTranslation } from "react-i18next"

addNamespace("plans", plansLocale)

export const Route = createFileRoute("/_authed/plans/new")({
  loader: ({ context: { queryClient } }) =>
    Promise.all([
      queryClient.ensureQueryData(workloadsQuery()),
      // A plan must name its provider - unlike an ad-hoc drill, where the
      // server falls back to its configured default. The skeleton needs a
      // real one, so it is loaded before the editor opens.
      queryClient.ensureQueryData(providersQuery()),
    ]),
  component: NewPlanPage,
  errorComponent: ({ error }) => <ErrorState error={error} />,
})

function NewPlanPage() {
  const { t } = useTranslation("plans")
  const workloads = useSuspenseQuery(workloadsQuery()).data
  const providers = useSuspenseQuery(providersQuery()).data
  const navigate = useNavigate()
  const [picked, setPicked] = useState<Workload | undefined>(undefined)

  const providerID = defaultProviderID(providers.items)

  return (
    <div className="space-y-6">
      <AppLink
        to="/plans"
        className="inline-flex items-center gap-1 text-muted-foreground text-sm hover:text-foreground"
      >
        <ArrowLeft className="size-4" aria-hidden="true" />
        {t("back")}
      </AppLink>

      {providerID === undefined ? (
        <EmptyState
          icon={<Server className="size-6" aria-hidden="true" />}
          title={t("noProvider")}
          command="restorelab connect"
        />
      ) : picked === undefined ? (
        <section className="space-y-4">
          <h1 className="font-semibold text-lg">{t("newTitle")}</h1>
          <div className="rounded-lg border">
            <Table>
              <TableBody>
                {workloads.items.map((w) => (
                  <TableRow key={w.id}>
                    <TableCell className="font-medium">{w.name}</TableCell>
                    <TableCell className="tabular text-muted-foreground">
                      {w.id}
                    </TableCell>
                    <TableCell className="text-muted-foreground">{w.node}</TableCell>
                    <TableCell className="text-right">
                      <Button size="sm" onClick={() => setPicked(w)}>
                        {t("new")}
                      </Button>
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </div>
        </section>
      ) : (
        <PlanEditor
          initialDocument={planSkeleton(picked, providerID)}
          onSaved={(saved) =>
            void navigate({ to: "/plans/$ref", params: { ref: saved.name } })
          }
        />
      )}
    </div>
  )
}

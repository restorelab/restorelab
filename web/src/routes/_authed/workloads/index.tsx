import { confidenceQuery, workloadsQuery } from "@/api/queries"
import type { Confidence, Page, Workload } from "@/api/types"
import { AppLink } from "@/components/app-link"
import { ConfidenceScore } from "@/components/confidence"
import { EmptyState } from "@/components/empty-state"
import { ErrorState } from "@/components/error-state"
import { Skeleton } from "@/components/ui/skeleton"
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table"
import { addNamespace } from "@/i18n"
import workloadsLocale from "@/i18n/locales/en/workloads.json"
import { useQueries, useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute } from "@tanstack/react-router"
import { Server } from "lucide-react"
import { useTranslation } from "react-i18next"

addNamespace("workloads", workloadsLocale)

export const Route = createFileRoute("/_authed/workloads/")({
  loader: ({ context: { queryClient } }) =>
    queryClient.ensureQueryData(workloadsQuery()),
  component: WorkloadsPage,
  errorComponent: ({ error }) => <ErrorState error={error} />,
})

/** The last drill this workload was scored on, when there is one to point at. */
function LastDrill({ confidence }: { confidence: Confidence | undefined }) {
  const { t } = useTranslation("workloads")
  // No confidence yet means no answer yet - not "never drilled", which is an
  // answer, and one this row has not been given.
  if (!confidence) return null
  if (!confidence.tested) return <span>{t("neverDrilled")}</span>
  if (!confidence.last_run_id) return null
  return (
    <AppLink
      to="/runs/$id"
      params={{ id: confidence.last_run_id }}
      className="hover:underline"
    >
      {t("viewLastDrill")}
    </AppLink>
  )
}

/**
 * The inventory, taking its data as props so it can be rendered alone.
 *
 * A workload missing from `confidences` has not been answered yet. That is
 * neither a zero nor a "never tested" - both are answers, and none has arrived
 * - so the cell waits rather than inventing one.
 */
export function WorkloadsContent({
  workloads,
  confidences,
}: {
  workloads: Page<Workload>
  confidences: Map<string, Confidence>
}) {
  const { t } = useTranslation("workloads")

  return (
    <section className="space-y-4">
      <header className="space-y-1">
        <h1 className="font-semibold text-lg">{t("title")}</h1>
        <p className="text-muted-foreground text-sm">{t("question")}</p>
      </header>

      {workloads.items.length === 0 ? (
        <EmptyState
          icon={<Server className="size-6" aria-hidden="true" />}
          title={t("empty.title")}
          description={t("empty.description")}
          command={t("empty.command")}
        />
      ) : (
        <div className="rounded-lg border">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>{t("columns.workload")}</TableHead>
                <TableHead>{t("columns.id")}</TableHead>
                <TableHead>{t("columns.node")}</TableHead>
                <TableHead>{t("columns.confidence")}</TableHead>
                <TableHead>{t("columns.lastDrill")}</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {workloads.items.map((w) => {
                const confidence = confidences.get(w.id)
                return (
                  <TableRow key={w.id}>
                    <TableCell>
                      <AppLink
                        to="/workloads/$id"
                        params={{ id: w.id }}
                        className="hover:underline"
                      >
                        {w.name}
                      </AppLink>
                    </TableCell>
                    <TableCell className="tabular text-muted-foreground">
                      {w.id}
                    </TableCell>
                    <TableCell className="text-muted-foreground">{w.node}</TableCell>
                    <TableCell>
                      {confidence ? (
                        <ConfidenceScore
                          value={confidence.score}
                          tested={confidence.tested}
                        />
                      ) : (
                        <Skeleton
                          className="h-4 w-20"
                          aria-label={t("loadingConfidence")}
                        />
                      )}
                    </TableCell>
                    <TableCell className="text-muted-foreground text-sm">
                      <LastDrill confidence={confidence} />
                    </TableCell>
                  </TableRow>
                )
              })}
            </TableBody>
          </Table>
        </div>
      )}
    </section>
  )
}

function WorkloadsPage() {
  const page = useSuspenseQuery(workloadsQuery()).data

  // One request per workload, declared together. useQueries keeps them
  // parallel and individually cached; the same thing written as a useQuery
  // inside the row loop would break the rules of hooks the first time the
  // inventory changed length.
  const results = useQueries({
    queries: page.items.map((w) => confidenceQuery(w.id)),
  })

  const confidences = new Map<string, Confidence>()
  page.items.forEach((w, i) => {
    const data = results[i]?.data
    if (data) confidences.set(w.id, data)
  })

  return <WorkloadsContent workloads={page} confidences={confidences} />
}

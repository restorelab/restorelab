import { doctorQuery, queueQuery, runsQuery, workloadsQuery } from "@/api/queries"
import type { Doctor, Page, QueueEntry, RunSummary, Workload } from "@/api/types"
import { AppLink } from "@/components/app-link"
import { EmptyState } from "@/components/empty-state"
import { ErrorState } from "@/components/error-state"
import { HealthStrip } from "@/components/health-strip"
import { RunStatusBadge, toneClass } from "@/components/run-status"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
import { Table, TableBody, TableCell, TableRow } from "@/components/ui/table"
import { addNamespace } from "@/i18n"
import overview from "@/i18n/locales/en/overview.json"
import { elapsedSeconds, formatDuration, formatRelative } from "@/lib/time"
import { cn } from "@/lib/utils"
import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute } from "@tanstack/react-router"
import { FlaskConical } from "lucide-react"
import { useTranslation } from "react-i18next"

addNamespace("overview", overview)

export const Route = createFileRoute("/_authed/")({
  loader: ({ context: { queryClient } }) =>
    Promise.all([
      queryClient.ensureQueryData(doctorQuery()),
      queryClient.ensureQueryData(queueQuery()),
      queryClient.ensureQueryData(runsQuery({ limit: 10 })),
      queryClient.ensureQueryData(workloadsQuery()),
    ]),
  component: OverviewPage,
  errorComponent: ({ error }) => <ErrorState error={error} />,
})

/** The name a drill is known by: the workload's, or its id when it has none. */
function workloadLabel(run: RunSummary): string {
  return run.source_name ?? run.source_workload_id
}

/** One drill still going: what it is, where it is, and for how long. */
function RunningRow({ entry }: { entry: QueueEntry }) {
  const { t } = useTranslation("overview")
  return (
    <TableRow>
      <TableCell className="font-medium">{workloadLabel(entry)}</TableCell>
      <TableCell>
        <RunStatusBadge state={entry.state} />
      </TableCell>
      <TableCell className="tabular text-muted-foreground">
        {formatDuration(elapsedSeconds(entry.started_at))}
      </TableCell>
      <TableCell className="text-right">
        <AppLink
          to="/runs/$id"
          params={{ id: entry.id }}
          className="text-sm underline-offset-4 hover:underline"
        >
          {t("running.view")}
        </AppLink>
      </TableCell>
    </TableRow>
  )
}

/** One finished drill: how it ended, when, and how long recovery took. */
function RecentRow({ run }: { run: RunSummary }) {
  const { t } = useTranslation("overview")
  return (
    <TableRow>
      <TableCell>
        <RunStatusBadge state={run.state} result={run.result} />
      </TableCell>
      <TableCell className="font-medium">
        <AppLink
          to="/runs/$id"
          params={{ id: run.id }}
          className="underline-offset-4 hover:underline"
        >
          {workloadLabel(run)}
        </AppLink>
      </TableCell>
      <TableCell className="text-muted-foreground">
        {formatRelative(run.started_at)}
      </TableCell>
      <TableCell className="text-right text-muted-foreground">
        <span className="mr-2 text-xs uppercase">{t("recent.rto")}</span>
        <span className={cn("tabular", run.rto_exceeded && toneClass("warning"))}>
          {run.rto}
        </span>
      </TableCell>
    </TableRow>
  )
}

/** The page's markup, taking its data as props so it can be tested alone. */
export function OverviewContent({
  doctor,
  queue,
  runs,
  workloads,
}: {
  doctor: Doctor
  queue: Page<QueueEntry>
  runs: Page<RunSummary>
  workloads: Page<Workload>
}) {
  const { t } = useTranslation("overview")
  // Day one: nothing has ever run, and nothing is running. The health strip
  // still has something to say, so only the two lists below are replaced.
  const dayOne = queue.items.length === 0 && runs.items.length === 0

  return (
    <div className="flex flex-col gap-6">
      <HealthStrip doctor={doctor} queue={queue} workloads={workloads} />

      {dayOne ? (
        <EmptyState
          icon={<FlaskConical className="size-6" />}
          title={t("empty.title")}
          description={t("empty.description")}
          command={t("empty.command", {
            workload: workloads.items[0]?.id ?? "<workload>",
          })}
        />
      ) : null}

      {queue.items.length > 0 ? (
        <Card>
          <CardHeader>
            <CardTitle>{t("running.title")}</CardTitle>
          </CardHeader>
          <CardContent>
            <Table>
              <TableBody>
                {queue.items.map((entry) => (
                  <RunningRow key={entry.id} entry={entry} />
                ))}
              </TableBody>
            </Table>
          </CardContent>
        </Card>
      ) : null}

      {runs.items.length > 0 ? (
        <Card>
          <CardHeader className="flex-row items-center justify-between">
            <CardTitle>{t("recent.title")}</CardTitle>
            <AppLink
              to="/runs"
              className="text-muted-foreground text-sm hover:text-foreground"
            >
              {t("recent.all")}
            </AppLink>
          </CardHeader>
          <CardContent>
            <Table>
              <TableBody>
                {runs.items.map((run) => (
                  <RecentRow key={run.id} run={run} />
                ))}
              </TableBody>
            </Table>
          </CardContent>
        </Card>
      ) : null}
    </div>
  )
}

function OverviewPage() {
  const doctor = useSuspenseQuery(doctorQuery()).data
  const queue = useSuspenseQuery(queueQuery()).data
  const runs = useSuspenseQuery(runsQuery({ limit: 10 })).data
  const workloads = useSuspenseQuery(workloadsQuery()).data
  return (
    <OverviewContent doctor={doctor} queue={queue} runs={runs} workloads={workloads} />
  )
}

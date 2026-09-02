import { type RunsFilter, runsQuery } from "@/api/queries"
import { type Page, RUN_STATES, type RunSummary } from "@/api/types"
import { AppLink } from "@/components/app-link"
import { EmptyState } from "@/components/empty-state"
import { ErrorState } from "@/components/error-state"
import { RunStatusBadge, toneClass } from "@/components/run-status"
import { Button } from "@/components/ui/button"
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"
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
import runs from "@/i18n/locales/en/runs.json"
import { formatAbsolute, formatRelative } from "@/lib/time"
import { cn } from "@/lib/utils"
import { useQuery } from "@tanstack/react-query"
import { createFileRoute } from "@tanstack/react-router"
import { ChevronDown, History } from "lucide-react"
import { useTranslation } from "react-i18next"

addNamespace("runs", runs)

/** How many rows one page of history holds. The API paginates by cursor. */
const PAGE_SIZE = 50

export const Route = createFileRoute("/_authed/runs/")({
  // A key is absent rather than undefined when the filter is not set. That is
  // what keeps every `navigate({ to: "/runs" })` in the app free of
  // placeholders: a schema that always names its keys makes them required.
  validateSearch: (s: Record<string, unknown>): RunsFilter => {
    const filter: RunsFilter = {}
    if (typeof s.state === "string") filter.state = s.state
    if (typeof s.workload === "string") filter.workload = s.workload
    if (typeof s.cursor === "string") filter.cursor = s.cursor
    return filter
  },
  loaderDeps: ({ search }) => search,
  loader: ({ context: { queryClient }, deps }) =>
    queryClient.ensureQueryData(runsQuery({ ...deps, limit: PAGE_SIZE })),
  component: RunsPage,
  errorComponent: ({ error }) => <ErrorState error={error} />,
})

/**
 * The drill history: what has run, and how it went.
 *
 * It owns no state of its own. The filters and the cursor live in the URL, so
 * a filtered view is a link someone can send and the browser's back button
 * means what it looks like it means.
 */
export function RunsContent({
  page,
  filter,
  onFilter,
  onPage,
}: {
  page: Page<RunSummary>
  filter: RunsFilter
  onFilter: (next: RunsFilter) => void
  onPage: (cursor: string) => void
}) {
  const { t } = useTranslation("runs")
  const { t: tc } = useTranslation()

  const filtering = Boolean(filter.state || filter.workload)
  // A cursor read into a const so the narrowing survives into the handler.
  const nextCursor = page.next_cursor

  // Day one and "your filter matched nothing" are different pieces of news.
  // Offering to launch a drill to someone who has just filtered on FAILED
  // would answer a question they did not ask.
  if (page.items.length === 0 && !filtering) {
    return (
      <section className="flex flex-col gap-4">
        <h1 className="font-semibold text-lg">{t("title")}</h1>
        <EmptyState
          icon={<History className="size-6" />}
          title={t("empty.title")}
          description={t("empty.description")}
          command={t("empty.command")}
        />
      </section>
    )
  }

  return (
    <section className="flex flex-col gap-4">
      <div className="flex flex-wrap items-center gap-2">
        <h1 className="mr-auto font-semibold text-lg">{t("title")}</h1>

        <DropdownMenu>
          <DropdownMenuTrigger asChild>
            <Button variant="outline" size="sm">
              {filter.state ? tc(`runState.${filter.state}`) : t("filter.state")}
              <ChevronDown className="size-4" aria-hidden="true" />
            </Button>
          </DropdownMenuTrigger>
          <DropdownMenuContent align="end" className="max-h-80 overflow-y-auto">
            <DropdownMenuItem
              onSelect={() =>
                onFilter({ ...filter, state: undefined, cursor: undefined })
              }
            >
              {t("filter.state")}
            </DropdownMenuItem>
            {RUN_STATES.map((state) => (
              <DropdownMenuItem
                key={state}
                onSelect={() => onFilter({ ...filter, state, cursor: undefined })}
              >
                {tc(`runState.${state}`)}
              </DropdownMenuItem>
            ))}
          </DropdownMenuContent>
        </DropdownMenu>

        {filter.workload ? (
          <span className="rounded-md border px-2 py-1 text-muted-foreground text-sm">
            <span className="tabular">{filter.workload}</span>
          </span>
        ) : null}

        {filtering ? (
          <Button variant="ghost" size="sm" onClick={() => onFilter({})}>
            {t("filter.clear")}
          </Button>
        ) : null}
      </div>

      {page.items.length === 0 ? (
        <EmptyState title={t("noMatch.title")} />
      ) : (
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>{t("columns.state")}</TableHead>
              <TableHead>{t("columns.workload")}</TableHead>
              <TableHead>{t("columns.plan")}</TableHead>
              <TableHead>{t("columns.started")}</TableHead>
              <TableHead className="text-right">{t("columns.rto")}</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {page.items.map((run) => (
              <TableRow key={run.id}>
                <TableCell>
                  <AppLink
                    to="/runs/$id"
                    params={{ id: run.id }}
                    className="hover:underline"
                  >
                    <RunStatusBadge state={run.state} result={run.result} />
                  </AppLink>
                </TableCell>
                <TableCell>
                  <AppLink
                    to="/workloads/$id"
                    params={{ id: run.source_workload_id }}
                    className="hover:underline"
                  >
                    {run.source_name ?? run.source_workload_id}
                  </AppLink>
                </TableCell>
                <TableCell className="text-muted-foreground">{run.plan_name}</TableCell>
                <TableCell
                  className="text-muted-foreground"
                  title={formatAbsolute(run.started_at)}
                >
                  {formatRelative(run.started_at)}
                </TableCell>
                <TableCell className="text-right">
                  {/* Colour says one thing here: a missed RTO is a failure. */}
                  <span
                    className={cn("tabular", run.rto_exceeded && toneClass("failed"))}
                  >
                    {run.rto}
                  </span>
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}

      {/* The API's own cursor, and nothing else. An opaque cursor cannot be
          walked backwards, so there are no page numbers to offer. */}
      {nextCursor ? (
        <div className="flex justify-end">
          <Button variant="outline" size="sm" onClick={() => onPage(nextCursor)}>
            {t("next")}
          </Button>
        </div>
      ) : null}
    </section>
  )
}

function RunsPage() {
  const search = Route.useSearch()
  const navigate = Route.useNavigate()
  const { data, error, refetch } = useQuery(runsQuery({ ...search, limit: PAGE_SIZE }))

  if (error) return <ErrorState error={error} onRetry={() => void refetch()} />
  if (!data) return <Skeleton className="h-64 w-full" />

  return (
    <RunsContent
      page={data}
      filter={search}
      // Never useState: the URL would then say one thing and the table
      // another, and a filtered view would stop being shareable.
      onFilter={(next) => void navigate({ to: "/runs", search: next })}
      onPage={(cursor) => void navigate({ to: "/runs", search: { ...search, cursor } })}
    />
  )
}

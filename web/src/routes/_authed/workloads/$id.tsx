import {
  backupsQuery,
  confidenceQuery,
  runsQuery,
  slotsQuery,
  workloadQuery,
} from "@/api/queries"
import {
  type Backup,
  type Confidence,
  type Page,
  type RunSummary,
  type Workload,
  isTerminal,
} from "@/api/types"
import { ConfidenceScore } from "@/components/confidence"
import { EmptyState } from "@/components/empty-state"
import { ErrorState } from "@/components/error-state"
import { RunStatusBadge } from "@/components/run-status"
import { TriggerDrill } from "@/components/trigger-drill"
import { Badge } from "@/components/ui/badge"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
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
import { formatAbsolute, formatRelative } from "@/lib/time"
import { cn } from "@/lib/utils"
import { useQuery, useSuspenseQuery } from "@tanstack/react-query"
import {
  Link,
  createFileRoute,
  useNavigate,
  useRouteContext,
} from "@tanstack/react-router"
import { Archive, ArrowLeft } from "lucide-react"
import { useTranslation } from "react-i18next"

addNamespace("workloads", workloadsLocale)

export const Route = createFileRoute("/_authed/workloads/$id")({
  loader: ({ context: { queryClient }, params }) =>
    Promise.all([
      queryClient.ensureQueryData(workloadQuery(params.id)),
      queryClient.ensureQueryData(confidenceQuery(params.id)),
      queryClient.ensureQueryData(runsQuery({ workload: params.id, limit: 20 })),
      // Backups are prefetched, not ensured: a deployment whose backup
      // provider is unreachable answers this route with a problem, and that is
      // one missing card, not a blank page.
      queryClient.prefetchQuery(backupsQuery(params.id)),
    ]),
  component: WorkloadDetailPage,
  errorComponent: ({ error }) => <ErrorState error={error} />,
})

/** Bytes, in the units a hypervisor console shows. */
function formatBytes(bytes: number): string {
  if (!Number.isFinite(bytes) || bytes <= 0) return "0 B"
  const units = ["B", "KiB", "MiB", "GiB", "TiB"]
  let value = bytes
  let unit = 0
  while (value >= 1024 && unit < units.length - 1) {
    value /= 1024
    unit++
  }
  return `${value >= 10 || unit === 0 ? Math.round(value) : value.toFixed(1)} ${units[unit]}`
}

function Header({
  workload,
  backups,
  canOperate,
  activeRunID,
  onStarted,
}: {
  workload: Workload
  backups: Backup[]
  canOperate: boolean
  activeRunID?: string
  onStarted: (runID: string) => void
}) {
  const { t } = useTranslation("workloads")
  return (
    <header className="space-y-2">
      <Link
        to="/workloads"
        className="inline-flex items-center gap-1 text-muted-foreground text-sm hover:text-foreground"
      >
        <ArrowLeft className="size-4" aria-hidden="true" />
        {t("detail.back")}
      </Link>
      <div className="flex flex-wrap items-center gap-3">
        <h1 className="font-semibold text-lg">{workload.name}</h1>
        {/* This screen already loads the backups, so unlike the listing it
            can name the one a drill would restore. */}
        <span className="ml-auto">
          <TriggerDrill
            workload={workload}
            backups={backups}
            canOperate={canOperate}
            activeRunID={activeRunID}
            onStarted={onStarted}
          />
        </span>
        <span className="tabular text-muted-foreground text-sm">{workload.id}</span>
        <Badge variant="outline">{workload.kind}</Badge>
        <Badge variant="outline">{workload.power_state}</Badge>
        {workload.node ? <Badge variant="outline">{workload.node}</Badge> : null}
        {workload.template ? (
          <Badge variant="outline">{t("detail.template")}</Badge>
        ) : null}
        {workload.managed ? (
          <Badge variant="outline">{t("detail.managed")}</Badge>
        ) : null}
      </div>
      <p className="tabular text-muted-foreground text-sm">
        {t("detail.spec", {
          cores: workload.cpu_cores,
          memory: formatBytes(workload.memory_bytes),
          disk: formatBytes(workload.disk_bytes),
        })}
      </p>
    </header>
  )
}

/**
 * The score, and the reasons that make it what it is.
 *
 * The Go comment on this indicator is explicit: the reasons are the value, and
 * the bare integer should never be shown without them. So they are not folded
 * away behind a disclosure here.
 */
function ConfidenceCard({ confidence }: { confidence: Confidence }) {
  const { t } = useTranslation("workloads")
  return (
    <Card>
      <CardHeader>
        <CardTitle>{t("detail.confidence.title")}</CardTitle>
      </CardHeader>
      <CardContent className="space-y-3">
        <div className="flex items-center gap-3">
          <ConfidenceScore value={confidence.score} tested={confidence.tested} />
          {confidence.tested ? (
            <span className="text-muted-foreground text-sm">
              {t("detail.confidence.runsConsidered", {
                count: confidence.runs_considered,
              })}
            </span>
          ) : null}
        </div>
        {confidence.tested ? (
          <div className="space-y-1">
            <p className="font-medium text-sm">{t("detail.confidence.reasons")}</p>
            {confidence.reasons.length === 0 ? (
              <p className="text-muted-foreground text-sm">
                {t("detail.confidence.perfect")}
              </p>
            ) : (
              <ul className="space-y-1 text-muted-foreground text-sm">
                {confidence.reasons.map((reason) => (
                  <li key={reason}>{reason}</li>
                ))}
              </ul>
            )}
          </div>
        ) : (
          <p className="max-w-prose text-muted-foreground text-sm">
            {t("detail.confidence.untested")}
          </p>
        )}
      </CardContent>
    </Card>
  )
}

function BackupsCard({ id }: { id: string }) {
  const { t } = useTranslation("workloads")
  const backups = useQuery(backupsQuery(id))

  return (
    <Card>
      <CardHeader>
        <CardTitle>{t("detail.backups.title")}</CardTitle>
      </CardHeader>
      <CardContent>
        {backups.isPending ? (
          <Skeleton className="h-16 w-full" aria-label={t("detail.backups.title")} />
        ) : backups.error ? (
          <ErrorState error={backups.error} onRetry={() => void backups.refetch()} />
        ) : backups.data.items.length === 0 ? (
          <EmptyState
            icon={<Archive className="size-6" aria-hidden="true" />}
            title={t("detail.backups.empty.title")}
            description={t("detail.backups.empty.description")}
          />
        ) : (
          <BackupTable backups={backups.data} />
        )}
      </CardContent>
    </Card>
  )
}

/**
 * The slots the scheduler decided against, for this machine.
 *
 * Rendered only when there are some. A machine whose drills all happened has
 * nothing to say here, and an empty card would suggest otherwise; a machine
 * whose slots keep being skipped looks, without this, exactly like one nobody
 * ever scheduled - which is the confusion the slot table exists to end.
 */
function SkippedSlotsCard({ id }: { id: string }) {
  const { t } = useTranslation("workloads")
  const slots = useQuery(slotsQuery({ workload: id }))

  const skipped = (slots.data?.items ?? []).filter((s) => s.outcome === "skipped")
  if (skipped.length === 0) return null

  return (
    <Card>
      <CardHeader>
        <CardTitle>{t("detail.skippedSlots.title")}</CardTitle>
        <p className="text-muted-foreground text-sm">
          {t("detail.skippedSlots.description")}
        </p>
      </CardHeader>
      <CardContent>
        <ul className="space-y-3">
          {skipped.map((slot) => (
            <li key={`${slot.plan_id}-${slot.slot_at}`} className="text-sm">
              <div className="flex flex-wrap items-baseline gap-x-2">
                <span className="tabular">{formatAbsolute(slot.slot_at)}</span>
                {slot.plan_name ? (
                  <span className="text-muted-foreground text-xs">
                    {slot.plan_name}
                  </span>
                ) : null}
              </div>
              {slot.reason ? (
                <p className="text-muted-foreground text-xs">{slot.reason}</p>
              ) : null}
            </li>
          ))}
        </ul>
      </CardContent>
    </Card>
  )
}

/** Proxmox Backup Server snapshots are the only backups that carry a state. */
function isPbsFormat(format: string | undefined): boolean {
  return format === "pbs" || (format?.startsWith("pbs-") ?? false)
}

/**
 * The verification state, as a phrase or not at all.
 *
 * "none" means the provider reported no verification, which covers two
 * different situations: a PBS snapshot nobody ever verified - worth saying -
 * and a vzdump backup, a format with no verification concept at all, where a
 * badge reading "never verified" would invent a shortcoming. The format tells
 * them apart, exactly as internal/report/html.go does.
 */
function Verification({ backup }: { backup: Backup }) {
  const { t } = useTranslation("workloads")
  const prefix = "detail.backups.verification."
  switch (backup.verified) {
    case "ok":
      return <Badge variant="outline">{t(`${prefix}ok`)}</Badge>
    case "failed":
      return <Badge variant="outline">{t(`${prefix}failed`)}</Badge>
    case "none":
      return isPbsFormat(backup.format) ? (
        <Badge variant="outline">{t(`${prefix}none`)}</Badge>
      ) : null
    case "":
      // The provider never set the field. Nothing was measured, so nothing is
      // reported - an empty badge would be a claim.
      return null
    default:
      return <Badge variant="outline">{t(`${prefix}unknown`)}</Badge>
  }
}

function BackupTable({ backups }: { backups: Page<Backup> }) {
  const { t } = useTranslation("workloads")
  return (
    <Table>
      <TableHeader>
        <TableRow>
          <TableHead>{t("detail.backups.columns.created")}</TableHead>
          <TableHead>{t("detail.backups.columns.age")}</TableHead>
          <TableHead>{t("detail.backups.columns.size")}</TableHead>
          <TableHead>{t("detail.backups.columns.datastore")}</TableHead>
          <TableHead>{t("detail.backups.columns.flags")}</TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {backups.items.map((b) => (
          <TableRow key={b.id}>
            <TableCell className="tabular">{formatAbsolute(b.created_at)}</TableCell>
            <TableCell className="tabular">{b.age}</TableCell>
            <TableCell className="tabular">{b.size}</TableCell>
            <TableCell className="text-muted-foreground">
              {b.datastore ?? b.node}
            </TableCell>
            <TableCell className="space-x-1">
              {b.protected ? (
                <Badge variant="outline">{t("detail.backups.protected")}</Badge>
              ) : null}
              {b.encrypted ? (
                <Badge variant="outline">{t("detail.backups.encrypted")}</Badge>
              ) : null}
              <Verification backup={b} />
            </TableCell>
          </TableRow>
        ))}
      </TableBody>
    </Table>
  )
}

function RunsCard({ id, runs }: { id: string; runs: Page<RunSummary> }) {
  const { t } = useTranslation("workloads")
  return (
    <Card>
      <CardHeader>
        <CardTitle>{t("detail.runs.title")}</CardTitle>
      </CardHeader>
      <CardContent>
        {runs.items.length === 0 ? (
          <EmptyState
            title={t("detail.runs.empty.title")}
            description={t("detail.runs.empty.description")}
            command={t("detail.runs.empty.command", { id })}
          />
        ) : (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>{t("detail.runs.columns.state")}</TableHead>
                <TableHead>{t("detail.runs.columns.plan")}</TableHead>
                <TableHead>{t("detail.runs.columns.started")}</TableHead>
                <TableHead>{t("detail.runs.columns.rto")}</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {runs.items.map((run) => (
                <TableRow key={run.id}>
                  <TableCell>
                    <Link
                      to="/runs/$id"
                      params={{ id: run.id }}
                      className="hover:underline"
                    >
                      <RunStatusBadge state={run.state} result={run.result} />
                    </Link>
                  </TableCell>
                  <TableCell>{run.plan_name}</TableCell>
                  <TableCell
                    className="tabular text-muted-foreground"
                    title={formatAbsolute(run.started_at)}
                  >
                    {formatRelative(run.started_at)}
                  </TableCell>
                  <TableCell
                    className={cn("tabular", run.rto_exceeded && "text-state-failed")}
                  >
                    {run.rto}
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}
      </CardContent>
    </Card>
  )
}

function WorkloadDetailPage() {
  const { id } = Route.useParams()
  const workload = useSuspenseQuery(workloadQuery(id)).data
  const confidence = useSuspenseQuery(confidenceQuery(id)).data
  const runs = useSuspenseQuery(runsQuery({ workload: id, limit: 20 })).data
  const backups = useQuery(backupsQuery(id)).data
  const { can } = useRouteContext({ from: "/_authed" })
  const navigate = useNavigate()

  // A drill in flight on this machine is already in the listing above; there
  // is no reason to ask the queue for it a second time.
  const active = runs.items.find((r) => !isTerminal(r.state))

  return (
    <div className="space-y-6">
      <Header
        workload={workload}
        backups={backups?.items ?? []}
        canOperate={can("operate")}
        activeRunID={active?.id}
        onStarted={(runID) => void navigate({ to: "/runs/$id", params: { id: runID } })}
      />
      <ConfidenceCard confidence={confidence} />
      <BackupsCard id={id} />
      <SkippedSlotsCard id={id} />
      <RunsCard id={id} runs={runs} />
    </div>
  )
}

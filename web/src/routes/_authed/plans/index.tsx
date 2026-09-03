import { ApiError } from "@/api/client"
import { useTriggerDrill } from "@/api/mutations"
import { plansQuery, scheduleQuery } from "@/api/queries"
import type { Page, Plan, ScheduledPlan } from "@/api/types"
import { AppLink } from "@/components/app-link"
import { EmptyState } from "@/components/empty-state"
import { ErrorState } from "@/components/error-state"
import { Button } from "@/components/ui/button"
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table"
import { addNamespace } from "@/i18n"
import plansLocale from "@/i18n/locales/en/plans.json"
import { formatRelative, formatUntil } from "@/lib/time"
import { useQuery, useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, useNavigate, useRouteContext } from "@tanstack/react-router"
import { FileText } from "lucide-react"
import { useTranslation } from "react-i18next"

addNamespace("plans", plansLocale)

export const Route = createFileRoute("/_authed/plans/")({
  loader: ({ context: { queryClient } }) => queryClient.ensureQueryData(plansQuery()),
  component: PlansPage,
  errorComponent: ({ error }) => {
    // No history database means no catalogue table, which the API answers as a
    // 503 rather than as an empty list. That is not an error to report, it is
    // a deployment to explain - and the command it names exists.
    if (error instanceof ApiError && error.status === 503) {
      return <NoCatalogue />
    }
    return <ErrorState error={error} />
  },
})

function NoCatalogue() {
  const { t } = useTranslation("plans")
  return (
    <EmptyState
      icon={<FileText className="size-6" aria-hidden="true" />}
      title={t("noHistory.title")}
      description={t("noHistory.description")}
      command={t("noHistory.command")}
    />
  )
}

/**
 * When a plan drills next, or why it does not.
 *
 * Three states, and they must not be allowed to look alike: a plan with no
 * schedule (a dash - most plans, and not a problem), a plan whose schedule
 * cannot be read (said out loud, because it has silently stopped drilling),
 * and a plan with a slot coming.
 */
function NextDrill({ plan }: { plan?: ScheduledPlan }) {
  const { t } = useTranslation("plans")

  if (!plan) {
    return <span className="text-muted-foreground">{t("notScheduled")}</span>
  }
  if (plan.error || !plan.next_slot_at) {
    return (
      <span className="text-destructive text-xs">
        {t("scheduleBroken", { reason: plan.error ?? "" })}
      </span>
    )
  }
  return (
    <span className="text-muted-foreground">
      {formatUntil(plan.next_slot_at)}
      <span className="ml-1.5 tabular text-xs opacity-70">{plan.schedule}</span>
    </span>
  )
}

/**
 * The catalogue, taking its data as props so it can be rendered alone.
 *
 * Three powers meet on this screen and none implies another: read lists it,
 * manage writes it, operate runs it. What a session cannot do is not rendered
 * at all - never a disabled control, which sends people looking for the fault
 * in the wrong place.
 */
export function PlansContent({
  plans,
  schedule,
  canManage,
  canOperate,
  onRun,
}: {
  plans: Page<Plan>
  /**
   * What each plan's cron is about to do, keyed by plan id below.
   *
   * Optional because the catalogue must still render when the schedule
   * cannot be read - a 503 on one endpoint should not blank the screen the
   * other one fills.
   */
  schedule?: Page<ScheduledPlan>
  canManage: boolean
  canOperate: boolean
  onRun: (name: string) => void
}) {
  const { t } = useTranslation("plans")
  const scheduled = new Map((schedule?.items ?? []).map((s) => [s.plan_id, s]))

  return (
    <section className="space-y-4">
      <header className="flex flex-wrap items-start justify-between gap-3">
        <div className="space-y-1">
          <h1 className="font-semibold text-lg">{t("title")}</h1>
          <p className="text-muted-foreground text-sm">{t("question")}</p>
        </div>
        {canManage ? (
          <AppLink
            to="/plans/new"
            className="inline-flex items-center rounded-md bg-primary px-3 py-1.5 text-primary-foreground text-sm"
          >
            {t("new")}
          </AppLink>
        ) : (
          <p className="text-muted-foreground text-sm">{t("readOnly")}</p>
        )}
      </header>

      {plans.items.length === 0 ? (
        <EmptyState
          icon={<FileText className="size-6" aria-hidden="true" />}
          title={t("empty.title")}
          description={t("empty.description")}
          command={t("empty.command")}
        />
      ) : (
        <div className="rounded-lg border">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>{t("columns.name")}</TableHead>
                <TableHead>{t("columns.workload")}</TableHead>
                <TableHead>{t("columns.version")}</TableHead>
                <TableHead>{t("columns.nextDrill")}</TableHead>
                <TableHead>{t("columns.updated")}</TableHead>
                <TableHead className="text-right">{t("columns.action")}</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {plans.items.map((p) => (
                <TableRow key={p.id}>
                  <TableCell>
                    <AppLink
                      to="/plans/$ref"
                      params={{ ref: p.name }}
                      className="hover:underline"
                    >
                      {p.name}
                    </AppLink>
                    {p.description ? (
                      <p className="text-muted-foreground text-xs">{p.description}</p>
                    ) : null}
                  </TableCell>
                  <TableCell className="tabular text-muted-foreground">
                    {p.workload_id}
                  </TableCell>
                  <TableCell className="tabular text-muted-foreground">
                    {p.version}
                  </TableCell>
                  <TableCell className="text-sm">
                    <NextDrill plan={scheduled.get(p.id)} />
                  </TableCell>
                  <TableCell className="text-muted-foreground text-sm">
                    {formatRelative(p.updated_at)}
                  </TableCell>
                  <TableCell className="text-right">
                    {canOperate ? (
                      <Button size="sm" onClick={() => onRun(p.name)}>
                        {t("run")}
                      </Button>
                    ) : null}
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </div>
      )}
    </section>
  )
}

function PlansPage() {
  const plans = useSuspenseQuery(plansQuery()).data
  // useQuery rather than useSuspenseQuery: the schedule is one column, and a
  // deployment whose slot table cannot be read should still get its
  // catalogue rather than an error boundary over the whole screen.
  const schedule = useQuery(scheduleQuery()).data
  const { can } = useRouteContext({ from: "/_authed" })
  const navigate = useNavigate()
  const trigger = useTriggerDrill()

  return (
    <PlansContent
      plans={plans}
      schedule={schedule}
      canManage={can("manage")}
      canOperate={can("operate")}
      onRun={(name) =>
        // The plan names its own workload, so the body carries nothing else:
        // mixing a plan with ad-hoc fields is a 400 that says exactly that.
        trigger.mutate(
          { plan: name },
          {
            onSuccess: (run) =>
              void navigate({ to: "/runs/$id", params: { id: run.id } }),
          },
        )
      }
    />
  )
}

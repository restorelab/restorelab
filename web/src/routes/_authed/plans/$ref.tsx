import { useTriggerDrill } from "@/api/mutations"
import { planQuery } from "@/api/queries"
import { AppLink } from "@/components/app-link"
import { DeletePlan } from "@/components/delete-plan"
import { ErrorState } from "@/components/error-state"
import { PlanEditor } from "@/components/plan-editor"
import { Button } from "@/components/ui/button"
import { addNamespace } from "@/i18n"
import plansLocale from "@/i18n/locales/en/plans.json"
import { useQueryClient, useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, useNavigate, useRouteContext } from "@tanstack/react-router"
import { ArrowLeft } from "lucide-react"
import { useTranslation } from "react-i18next"

addNamespace("plans", plansLocale)

export const Route = createFileRoute("/_authed/plans/$ref")({
  loader: ({ context: { queryClient }, params }) =>
    queryClient.ensureQueryData(planQuery(params.ref)),
  component: PlanDetailPage,
  errorComponent: ({ error }) => <ErrorState error={error} />,
})

function PlanDetailPage() {
  const { ref } = Route.useParams()
  const { t } = useTranslation("plans")
  const plan = useSuspenseQuery(planQuery(ref)).data
  const { can } = useRouteContext({ from: "/_authed" })
  const navigate = useNavigate()
  const queryClient = useQueryClient()
  const trigger = useTriggerDrill()

  return (
    <div className="space-y-6">
      <AppLink
        to="/plans"
        className="inline-flex items-center gap-1 text-muted-foreground text-sm hover:text-foreground"
      >
        <ArrowLeft className="size-4" aria-hidden="true" />
        {t("back")}
      </AppLink>

      <header className="flex flex-wrap items-center gap-3">
        <h1 className="font-semibold text-lg">{plan.name}</h1>
        <span className="tabular text-muted-foreground text-sm">
          {t("columns.version")} {plan.version}
        </span>
        <div className="ml-auto flex items-center gap-2">
          {can("operate") ? (
            <Button
              size="sm"
              onClick={() =>
                trigger.mutate(
                  { plan: plan.name },
                  {
                    onSuccess: (run) =>
                      void navigate({ to: "/runs/$id", params: { id: run.id } }),
                  },
                )
              }
            >
              {t("run")}
            </Button>
          ) : null}
          <DeletePlan
            plan={plan}
            canManage={can("manage")}
            onDeleted={() => void navigate({ to: "/plans" })}
          />
        </div>
      </header>

      {plan.description ? (
        <p className="text-muted-foreground text-sm">{plan.description}</p>
      ) : null}

      {can("manage") ? (
        <PlanEditor
          // Keyed on the version so that taking the server's side after a
          // conflict rebuilds the editor on the document that came back,
          // rather than leaving the old text in a box that now says it is
          // current.
          key={plan.version}
          initialDocument={plan.yaml ?? ""}
          plan={plan}
          onSaved={() =>
            void queryClient.invalidateQueries({ queryKey: ["plan", ref] })
          }
          onTakeTheirs={() =>
            void queryClient.invalidateQueries({ queryKey: ["plan", ref] })
          }
        />
      ) : (
        <div className="space-y-2">
          <p className="font-medium text-muted-foreground text-xs uppercase tracking-wide">
            {t("document")}
          </p>
          <pre className="overflow-auto rounded-md border bg-muted p-3 font-mono text-xs">
            {plan.yaml}
          </pre>
          <p className="text-muted-foreground text-sm">{t("readOnly")}</p>
        </div>
      )}
    </div>
  )
}

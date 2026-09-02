import { reportUrl } from "@/api/client"
import { runQuery } from "@/api/queries"
import { isTerminal } from "@/api/types"
import { ErrorState } from "@/components/error-state"
import { PhaseTimeline } from "@/components/phase-timeline"
import { RunStatusBadge, toneClass } from "@/components/run-status"
import { Alert, AlertTitle } from "@/components/ui/alert"
import { Card, CardContent } from "@/components/ui/card"
import { useRunStream } from "@/hooks/useRunStream"
import { addNamespace } from "@/i18n"
import run from "@/i18n/locales/en/run.json"
import { formatAbsolute, formatRelative } from "@/lib/time"
import { useQueryClient, useSuspenseQuery } from "@tanstack/react-query"
import { Link, createFileRoute } from "@tanstack/react-router"
import { Download, PlugZap } from "lucide-react"
import { type ReactNode, useEffect } from "react"
import { useTranslation } from "react-i18next"

addNamespace("run", run)

export const Route = createFileRoute("/_authed/runs/$id")({
  // GET /recovery-runs/{id} answers the whole report document, steps and
  // checks included, so this screen makes one request and not two.
  loader: ({ context: { queryClient }, params }) =>
    queryClient.ensureQueryData(runQuery(params.id)),
  component: RunDetailPage,
  errorComponent: ({ error }) => <ErrorState error={error} />,
})

function Fact({
  label,
  children,
}: {
  label: string
  children: ReactNode
}) {
  return (
    <div className="space-y-1">
      <dt className="text-muted-foreground text-xs uppercase tracking-wide">{label}</dt>
      <dd className="text-sm">{children}</dd>
    </div>
  )
}

function RunDetailPage() {
  const { id } = Route.useParams()
  const { t } = useTranslation("run")
  const queryClient = useQueryClient()
  const { data: doc } = useSuspenseQuery(runQuery(id))

  // A finished drill has nothing left to stream; the connection is only opened
  // while the run is still going.
  const streaming = !isTerminal(doc.state)
  const live = useRunStream(id, streaming)

  // `done` says the drill ended. Refetching once here picks up the final
  // document - result, RTO, cleanup - instead of waiting for the next poll.
  useEffect(() => {
    if (live.finished) {
      void queryClient.invalidateQueries({ queryKey: ["run", id] })
    }
  }, [live.finished, queryClient, id])

  return (
    <div className="space-y-6">
      <Link to="/runs" className="text-muted-foreground text-sm hover:text-foreground">
        ← {t("back")}
      </Link>

      <header className="space-y-2">
        <p className="text-muted-foreground text-sm">{t("question")}</p>
        <div className="flex flex-wrap items-center gap-4">
          <h1 className="font-semibold text-xl">{doc.source_name}</h1>
          <RunStatusBadge state={doc.state} result={doc.result} />
          <a
            href={reportUrl(id)}
            download
            className="ml-auto inline-flex items-center gap-2 rounded-md border px-3 py-1.5 text-sm hover:bg-muted"
          >
            <Download className="size-4" aria-hidden="true" />
            {t("report")}
          </a>
        </div>
      </header>

      <Card>
        <CardContent>
          <dl className="grid gap-6 sm:grid-cols-3">
            <Fact label={t("rto")}>
              <span
                className={`tabular ${doc.rto_exceeded ? toneClass("failed") : ""}`}
              >
                {doc.rto}
              </span>
              {doc.rto_target ? (
                <span className="tabular ml-2 text-muted-foreground text-xs">
                  {t("rtoTarget", { target: doc.rto_target })}
                </span>
              ) : null}
            </Fact>

            <Fact label={t("backup")}>
              {doc.backup ? (
                <>
                  <span className="tabular">
                    {t("backupAge", { age: doc.backup.age })}
                  </span>
                  <span className="tabular ml-2 text-muted-foreground text-xs">
                    {formatAbsolute(doc.backup.created_at)}
                  </span>
                </>
              ) : (
                <span className="text-muted-foreground">{t("noBackup")}</span>
              )}
            </Fact>

            <Fact label={t("started")}>
              <span className="tabular">{formatAbsolute(doc.started_at)}</span>
              <span className="tabular ml-2 text-muted-foreground text-xs">
                {formatRelative(doc.started_at)}
              </span>
            </Fact>
          </dl>
        </CardContent>
      </Card>

      {/* The connection ended, not the drill. Nothing about the run's own
          displayed state changes here, and runQuery keeps polling underneath:
          that is the intended fallback. */}
      {live.disconnected ? (
        <Alert>
          <PlugZap className="size-4 text-state-warning" aria-hidden="true" />
          <AlertTitle className="line-clamp-none">{t("disconnected")}</AlertTitle>
        </Alert>
      ) : null}

      <section className="space-y-3">
        <h2 className="font-medium text-sm">{t("timeline")}</h2>
        <PhaseTimeline
          steps={doc.steps}
          checks={doc.checks}
          live={streaming ? live : null}
        />
      </section>
    </div>
  )
}

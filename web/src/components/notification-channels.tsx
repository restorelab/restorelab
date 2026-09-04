import { ApiError } from "@/api/client"
import {
  type NewNotification,
  type NotificationEdit,
  useCreateNotification,
  useDeleteNotification,
  useTestNotification,
  useUpdateNotification,
} from "@/api/mutations"
import {
  NOTIFICATION_KINDS,
  type NotificationChannel,
  type NotificationKind,
  type Page,
} from "@/api/types"
import { ConfirmDialog } from "@/components/confirm-dialog"
import { EmptyState } from "@/components/empty-state"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table"
import { addNamespace } from "@/i18n"
import notificationsLocale from "@/i18n/locales/en/notifications.json"
import { formatRelative } from "@/lib/time"
import { Bell } from "lucide-react"
import { useId, useState } from "react"
import { useTranslation } from "react-i18next"

addNamespace("notifications", notificationsLocale)

/**
 * A refusal in the server's own words, or nothing.
 *
 * Every failure on this screen is reported where the action was taken: beside
 * the row, or under the form, and it stays there. Nothing here pops up and
 * fades away on a timer - a message about a broken alerting path that vanishes
 * after three seconds would be the joke and the bug at once.
 */
function refusal(error: unknown): string | undefined {
  if (error instanceof ApiError) return error.detail ?? error.title
  return error instanceof Error ? error.message : undefined
}

/** The failure text under a form or a row, when there is one. */
function Refusal({ error }: { error: unknown }) {
  const text = refusal(error)
  if (!text) return null
  return (
    <p role="alert" className="text-sm text-state-failed">
      {text}
    </p>
  )
}

/**
 * A channel's kind, in the product's own words.
 *
 * A kind this build has never heard of is rendered verbatim rather than
 * hidden: it can only have arrived by somebody hand-editing config.yaml, and
 * showing them the string they typed is the fastest route to the typo.
 */
function kindLabel(kind: string, t: (key: string) => string): string {
  const label = t(`kinds.${kind}`)
  return label === `kinds.${kind}` ? kind : label
}

/**
 * What this channel's last real message did.
 *
 * The three states are kept apart on purpose. "Nothing sent yet" is not a
 * problem - most channels are new, and the product is quiet by design. A
 * pending delivery is not a broken channel either: it failed once and will be
 * tried again within ten minutes. Only a settled failure means somebody is
 * not being told, and it is the one this column exists for.
 */
function Health({ channel }: { channel: NotificationChannel }) {
  const { t } = useTranslation("notifications")

  if (!channel.last_state) {
    return <span className="text-muted-foreground">{t("health.never")}</span>
  }
  switch (channel.last_state) {
    case "sent":
      return (
        <span className="text-muted-foreground">
          {t("health.sent", {
            when: channel.last_sent ? formatRelative(channel.last_sent) : "",
          })}
        </span>
      )
    case "pending":
      return (
        <span className="text-state-warning">
          {t("health.pending", { status: channel.last_status ?? "--" })}
        </span>
      )
    case "failed":
      return (
        <span className="text-state-failed">
          {t("health.failed", { status: channel.last_status ?? "--" })}
          {channel.last_error ? (
            <span className="block text-xs opacity-80">{channel.last_error}</span>
          ) : null}
        </span>
      )
    default:
      return (
        <span className="text-muted-foreground">
          {t("health.unknown", { state: channel.last_state })}
        </span>
      )
  }
}

/** What a form hands back when it is submitted. */
interface FormValues {
  id: string
  kind: NotificationKind
  url: string
  enabled: boolean
}

/**
 * The one form both creating and editing use.
 *
 * The URL field is the whole reason this component is written once rather
 * than twice: it is a password field, it never carries a value, and on an
 * edit its placeholder says that leaving it blank keeps the stored URL. The
 * API hands a webhook URL back in no response at all, so there is nothing to
 * prefill, and a field that looked empty because it was empty would invite
 * somebody to save a channel into silence.
 */
function ChannelForm({
  channel,
  pending,
  error,
  onCancel,
  onSubmit,
}: {
  channel?: NotificationChannel
  pending: boolean
  error: unknown
  onCancel: () => void
  onSubmit: (values: FormValues) => void
}) {
  const { t } = useTranslation("notifications")
  const ids = useId()
  const editing = channel !== undefined

  const [id, setId] = useState(channel?.id ?? "")
  const [kind, setKind] = useState<NotificationKind>(
    (NOTIFICATION_KINDS as readonly string[]).includes(channel?.kind ?? "")
      ? (channel?.kind as NotificationKind)
      : "discord",
  )
  const [url, setUrl] = useState("")
  const [enabled, setEnabled] = useState(channel?.enabled ?? true)

  return (
    <form
      className="space-y-4 rounded-lg border bg-muted/30 p-4"
      onSubmit={(e) => {
        e.preventDefault()
        onSubmit({ id: id.trim(), kind, url: url.trim(), enabled })
      }}
    >
      <h2 className="font-medium text-sm">
        {editing ? t("form.editTitle", { id: channel.id }) : t("form.createTitle")}
      </h2>

      <div className="grid gap-4 sm:grid-cols-2">
        <div className="space-y-1.5">
          {/* An id is not editable: the channel is stored under it, and the
              deliveries already written point at it. Renaming would be a
              remove and an add, which is what the hint says. */}
          {editing ? (
            <>
              <p className="font-medium text-sm">{t("form.id")}</p>
              <p className="text-sm">
                <span className="tabular">{channel.id}</span>
                <span className="block text-muted-foreground text-xs">
                  {t("form.idFixed")}
                </span>
              </p>
            </>
          ) : (
            <>
              <Label htmlFor={`${ids}-id`}>{t("form.id")}</Label>
              <Input
                id={`${ids}-id`}
                value={id}
                onChange={(e) => setId(e.target.value)}
                autoComplete="off"
              />
              <p className="text-muted-foreground text-xs">{t("form.idHint")}</p>
            </>
          )}
        </div>

        <div className="space-y-1.5">
          <Label htmlFor={`${ids}-kind`}>{t("form.kind")}</Label>
          <select
            id={`${ids}-kind`}
            value={kind}
            onChange={(e) => setKind(e.target.value as NotificationKind)}
            className="h-9 w-full rounded-md border border-input bg-transparent px-3 py-1 text-sm shadow-xs outline-none focus-visible:border-ring focus-visible:ring-[3px] focus-visible:ring-ring/50 dark:bg-input/30"
          >
            {NOTIFICATION_KINDS.map((k) => (
              <option key={k} value={k}>
                {t(`kinds.${k}`)}
              </option>
            ))}
          </select>
        </div>
      </div>

      <div className="space-y-1.5">
        <Label htmlFor={`${ids}-url`}>{t("form.url")}</Label>
        <Input
          id={`${ids}-url`}
          // A bearer credential: typed once, never rendered back, and not
          // offered to a password manager that would try to fill it.
          type="password"
          autoComplete="off"
          value={url}
          placeholder={editing ? t("form.urlKept") : t("form.urlNew")}
          onChange={(e) => setUrl(e.target.value)}
        />
        <p className="text-muted-foreground text-xs">
          {editing ? t("form.urlKeptHint") : t("form.urlNewHint")}
        </p>
      </div>

      <div className="flex items-center gap-2">
        <input
          id={`${ids}-enabled`}
          type="checkbox"
          checked={enabled}
          onChange={(e) => setEnabled(e.target.checked)}
          className="size-4 rounded border-input"
        />
        <Label htmlFor={`${ids}-enabled`}>{t("form.enabled")}</Label>
      </div>

      <Refusal error={error} />

      <div className="flex items-center gap-2">
        <Button type="submit" size="sm" disabled={pending}>
          {pending ? t("form.saving") : t("form.submit")}
        </Button>
        <Button type="button" variant="outline" size="sm" onClick={onCancel}>
          {t("form.cancel")}
        </Button>
      </div>
    </form>
  )
}

/**
 * One channel, and everything that can be done to it.
 *
 * Its mutations are held here rather than in the parent so that each row
 * carries its own pending state and its own refusal: a test that failed on
 * one channel must not paint an error next to another.
 */
function ChannelRow({
  channel,
  canManage,
  canOperate,
}: {
  channel: NotificationChannel
  canManage: boolean
  canOperate: boolean
}) {
  const { t } = useTranslation("notifications")
  const [editing, setEditing] = useState(false)
  const [confirming, setConfirming] = useState(false)

  const update = useUpdateNotification(channel.id)
  const remove = useDeleteNotification()
  const test = useTestNotification(channel.id)

  function save(values: FormValues) {
    const body: NotificationEdit = { kind: values.kind, enabled: values.enabled }
    // The key is omitted, not sent empty. An empty string would ask the
    // server to tell "unchanged" from "cleared" apart on a value it never
    // showed us, and the wrong guess silences a channel that works.
    if (values.url !== "") body.url = values.url
    update.mutate(body, {
      onSuccess: () => {
        setEditing(false)
        update.reset()
      },
    })
  }

  return (
    <>
      <TableRow>
        <TableCell>
          <span className="tabular">{channel.id}</span>
          {channel.enabled ? null : (
            <span className="block text-muted-foreground text-xs">
              {t("enabled.offHint")}
            </span>
          )}
        </TableCell>
        <TableCell className="text-muted-foreground">
          {kindLabel(channel.kind, t)}
        </TableCell>
        <TableCell className="tabular text-muted-foreground">{channel.host}</TableCell>
        <TableCell className="text-sm">
          <Health channel={channel} />
        </TableCell>
        <TableCell className="text-right">
          <div className="flex justify-end gap-2">
            {canOperate ? (
              <Button
                variant="outline"
                size="sm"
                disabled={test.isPending}
                onClick={() => test.mutate()}
              >
                {test.isPending ? t("test.sending") : t("test.button")}
              </Button>
            ) : null}
            {canManage ? (
              <>
                <Button
                  variant="outline"
                  size="sm"
                  onClick={() => setEditing((open) => !open)}
                >
                  {t("edit")}
                </Button>
                <Button
                  variant="destructive"
                  size="sm"
                  onClick={() => setConfirming(true)}
                >
                  {t("delete.button")}
                </Button>
              </>
            ) : null}
          </div>
        </TableCell>
      </TableRow>

      {test.isSuccess || test.isError || editing ? (
        <TableRow>
          <TableCell colSpan={5} className="space-y-3">
            {test.isSuccess ? (
              <p className="text-state-success text-sm">
                {t("test.ok", { status: test.data.status })}
              </p>
            ) : null}
            <Refusal error={test.error} />
            {editing ? (
              <ChannelForm
                channel={channel}
                pending={update.isPending}
                error={update.error}
                onCancel={() => {
                  setEditing(false)
                  update.reset()
                }}
                onSubmit={save}
              />
            ) : null}
          </TableCell>
        </TableRow>
      ) : null}

      <ConfirmDialog
        open={confirming}
        onOpenChange={(next) => {
          setConfirming(next)
          if (!next) remove.reset()
        }}
        title={t("delete.title", { id: channel.id })}
        description={`${t("delete.description")} ${t("delete.keeps")}`}
        confirmLabel={t("delete.submit")}
        tone="destructive"
        pending={remove.isPending}
        error={refusal(remove.error)}
        onConfirm={() =>
          remove.mutate(channel.id, { onSuccess: () => setConfirming(false) })
        }
      />
    </>
  )
}

/**
 * Where an operator decides who hears about a change, and proves it works.
 *
 * Two powers meet here and neither implies the other: manage writes the
 * channels, operate makes one speak. What a session cannot do is not rendered
 * at all, never a disabled control - a greyed button sends somebody looking
 * for the fault in the wrong place, which is the same reasoning the plans
 * screen follows.
 */
export function NotificationChannels({
  channels,
  canManage,
  canOperate,
}: {
  channels: Page<NotificationChannel>
  canManage: boolean
  canOperate: boolean
}) {
  const { t } = useTranslation("notifications")
  const [adding, setAdding] = useState(false)
  const create = useCreateNotification()

  function add(values: FormValues) {
    const body: NewNotification = {
      id: values.id,
      kind: values.kind,
      url: values.url,
      enabled: values.enabled,
    }
    create.mutate(body, {
      onSuccess: () => {
        setAdding(false)
        create.reset()
      },
    })
  }

  return (
    <section className="space-y-4">
      <header className="flex flex-wrap items-start justify-between gap-3">
        <div className="space-y-1">
          <h1 className="font-semibold text-lg">{t("title")}</h1>
          <p className="text-muted-foreground text-sm">{t("question")}</p>
        </div>
        {canManage ? (
          <Button size="sm" onClick={() => setAdding(true)}>
            {t("add")}
          </Button>
        ) : (
          <p className="text-muted-foreground text-sm">{t("readOnly")}</p>
        )}
      </header>

      <p className="max-w-prose text-muted-foreground text-sm">{t("intro")}</p>

      {adding ? (
        <ChannelForm
          pending={create.isPending}
          error={create.error}
          onCancel={() => {
            setAdding(false)
            create.reset()
          }}
          onSubmit={add}
        />
      ) : null}

      {channels.items.length === 0 ? (
        <EmptyState
          icon={<Bell className="size-6" aria-hidden="true" />}
          title={t("empty.title")}
          description={t("empty.description")}
          command={t("empty.command")}
        />
      ) : (
        <div className="rounded-lg border">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>{t("columns.id")}</TableHead>
                <TableHead>{t("columns.kind")}</TableHead>
                <TableHead>{t("columns.host")}</TableHead>
                <TableHead>{t("columns.health")}</TableHead>
                <TableHead className="text-right">{t("columns.action")}</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {channels.items.map((c) => (
                <ChannelRow
                  key={c.id}
                  channel={c}
                  canManage={canManage}
                  canOperate={canOperate}
                />
              ))}
            </TableBody>
          </Table>
        </div>
      )}

      {canOperate ? (
        <p className="text-muted-foreground text-xs">{t("test.hint")}</p>
      ) : null}
    </section>
  )
}

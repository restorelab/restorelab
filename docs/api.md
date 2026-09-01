# HTTP API

RestoreLab exposes its drill history, the state of your fleet, and the drills
themselves over HTTP, under `/api/v1`. Reading needs a token; triggering,
cancelling and cleaning up need a token that was explicitly created with the
right to do so.

## How a write actually happens

No handler in `internal/api` calls a mutating provider method — `Restore`,
`Start`, `Stop`, `Delete` or `AllocateWorkloadID`. That is still true now that
the API can trigger a drill, and it is not a promise kept by code review: the
fake provider the handler tests run against fails the test outright if any of
those methods is reached, and a second test greps the package for those names
outside `_test.go` files.

What `POST /recovery-runs` does instead is write one row. A worker — in the
same process by default, or on another machine — claims that row and runs the
drill through the same `recovery.Engine` the CLI uses, with every guard the
engine already carries. The API and the worker never call each other; they
share a database and nothing else, which is what makes splitting them a
deployment choice rather than a rewrite.

The single exception is `POST /cleanup/{vmid}`, which does end in a `Delete`.
It makes that call through `worker.Cleanup`, so the only package holding a
mutating provider method stays the one that carries the guards and the tests
for them.

## Starting the server

```
restorelab serve
restorelab serve --listen 127.0.0.1:9000
```

`serve` binds to `127.0.0.1:8080` by default. That is deliberate: a
RestoreLab that appeared on every interface the moment someone typed `serve`
would be a surprise, and surprises with API surfaces are how clusters end up
readable by strangers.

TLS is not handled by RestoreLab. Put a reverse proxy in front of it — nginx,
Caddy, whatever you already run:

```nginx
server {
    listen 443 ssl;
    server_name restorelab.example.com;

    ssl_certificate     /etc/letsencrypt/live/restorelab.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/restorelab.example.com/privkey.pem;

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_set_header Host $host;
    }
}
```

Listening on anything but loopback requires at least one live API token.
Without one, `serve` refuses to start rather than expose an unauthenticated
API:

```
$ restorelab serve --listen 0.0.0.0:8080
error: refusing to listen on 0.0.0.0:8080 with no API token: create one with `restorelab token create <name>`
```

`serve` checks this once, at startup, by listing tokens and counting the live
ones — so a server that starts with a token and later has that token revoked
keeps running; nothing re-checks the binding on every request.

### One process, or two

By default `serve` both answers requests and executes the drills those
requests queue. The two halves can be separated, because they only ever talk
through the database:

```
restorelab serve --no-listen                        the worker alone
restorelab serve --no-worker --worker-elsewhere     the API alone
```

`--no-worker` on its own is refused, and the refusal is the point. A server
that accepts `POST /recovery-runs` with nobody draining the queue answers
`201 Created` to a caller that will then wait forever: the run is genuinely
queued, the response is genuinely correct, and the drill will never happen.
Nothing can verify from inside the process that a worker exists somewhere else
— a worker on another machine leaves no trace until it claims something — so
the honest design is to make the operator say it out loud with
`--worker-elsewhere` rather than to guess.

`--no-worker` and `--no-listen` together are refused too: that combination
leaves a process that neither serves nor executes.

A worker needs a history database, and `--no-listen` refuses to start without
one — a worker with no queue to claim from has nothing it could ever do. A
read-only API is different: it still answers questions about the cluster, so
it starts.

## Authenticating

```
restorelab token create <name>              mints a read-only token, prints it once
restorelab token create <name> --operate    ... one that can also trigger drills
restorelab token list                       name, scopes, created, last used, state
restorelab token revoke <name>
```

A token looks like `rl_` followed by 43 base64url characters — the encoding
of 32 bytes from `crypto/rand`. The prefix exists so a secret that leaks into
a log, a ticket or a public repository is recognisable as a RestoreLab token,
which is what lets it be revoked instead of puzzled over; it is the same idea
as GitHub's `ghp_` or Stripe's `sk_live_`.

Only the SHA-256 of the secret is ever stored. `token create` prints the
secret exactly once, at creation time; there is no command that can show it
again, because there is nothing left in the database to show. Losing it means
creating a new one and revoking the old.

Send it as a bearer token:

```
Authorization: Bearer rl_<secret>
```

`/health` is the only route that skips authentication — it says nothing about
the deployment beyond "the process is up". Every other route returns 401
without a valid token:

```
$ curl -i https://restorelab.example.com/api/v1/recovery-runs
HTTP/1.1 401 Unauthorized
Content-Type: application/problem+json
Www-Authenticate: Bearer realm="restorelab"
```

`last_used_at` is updated at most once a minute per token — an exact counter
would cost a database write on every single request for a field nobody reads
to the second.

## Scopes

A token holds one of two levels of access:

| Scope | What it allows |
| --- | --- |
| `read` | every `GET` route, including the live event stream |
| `operate` | `read`, plus triggering a drill, cancelling one, and cleaning up a temporary workload |

`read` is the default and is implied by every token, including an `operate`
one: an account that could start a drill but not watch it would be a strange
thing to hand anyone.

`--operate` is opt-in because of what it actually is. A token that can `POST
/recovery-runs` can make RestoreLab restore backups, boot machines and delete
them again, on a schedule of the caller's choosing. It is a key that destroys
and recreates machines, not a key that reads a dashboard, and `token create`
says so on screen at the only moment anyone is still looking at the secret.
`token list` puts `SCOPES` in the table rather than behind a flag, because
which token can destroy machines is the first thing anyone auditing that list
is looking for.

A token created before scopes existed reads back as `read`. That is the
migration's default and it was chosen deliberately: a schema change must never
hand an existing credential a power it did not have when it was issued. If one
of those tokens is meant to trigger drills, create a new one with `--operate`
and revoke the old.

A write attempted with a `read` token is **403, not 401**:

```
$ curl -i -X POST -H "Authorization: Bearer rl_..." \
    https://restorelab.example.com/api/v1/recovery-runs -d '{"workload_id":"110"}'
HTTP/1.1 403 Forbidden
Content-Type: application/problem+json

{"type":"https://restorelab.dev/problems/insufficient-scope","title":"This token may not do that","status":403,"detail":"this endpoint needs the \"operate\" scope; create a token with `restorelab token create <name> --operate`"}
```

The distinction is not pedantry. A 401 means "we do not know who you are" and
sends the caller off to regenerate a token that was never broken; a 403 means
"we know exactly who you are, and this is not yours to do", which is the only
answer that points at the actual fix. Authentication runs first, so an
anonymous request still gets its 401.

## The surface

```
GET  /api/v1/health                        unauthenticated
GET  /api/v1/recovery-runs                 workload, state, result, since, limit, cursor
GET  /api/v1/recovery-runs/{id}            id complete or a prefix
GET  /api/v1/recovery-runs/{id}/events     after=<seq>, or SSE on Accept
GET  /api/v1/recovery-runs/{id}/report     format=json|html
GET  /api/v1/queue                         limit
GET  /api/v1/workloads                     provider, temporary
GET  /api/v1/workloads/{id}                provider
GET  /api/v1/workloads/{id}/backups        provider
GET  /api/v1/workloads/{id}/confidence     provider
GET  /api/v1/providers
GET  /api/v1/doctor                        provider, workload

POST /api/v1/recovery-runs                 operate — queue a drill
POST /api/v1/recovery-runs/{id}/cancel     operate — stop one
POST /api/v1/cleanup/{vmid}                operate — destroy a leftover workload
```

`{id}` on a run accepts a prefix, exactly like `restorelab runs show`: an
exact match wins, an ambiguous prefix comes back as a 409 rather than a
guess. That applies to `/cancel` as much as to the reads.

### `GET /api/v1/health`

No token required.

```
$ curl https://restorelab.example.com/api/v1/health
{"status":"ok","version":"restorelab dev (06dc8c475026) windows/amd64 go1.27.0"}
```

### `GET /api/v1/recovery-runs`

Lists the drill history. Every parameter is optional:

| Parameter | Meaning |
| --- | --- |
| `workload` | only runs against this source workload id |
| `state` | one of `QUEUED`, `DISCOVERING_BACKUP`, `PREPARING_ENVIRONMENT`, `RESTORING`, `STARTING`, `WAITING_FOR_GUEST`, `RUNNING_CHECKS`, `GENERATING_REPORT`, `CLEANING_UP`, `SUCCESS`, `FAILED`, `CANCELLED`, `CLEANUP_FAILED` |
| `result` | `SUCCESS`, `DEGRADED` or `FAILED` |
| `since` | `30d`, `12h`, a date (`2026-08-01`), or a full RFC 3339 instant |
| `limit` | page size, default 50, capped at 200 |
| `cursor` | opaque, from a previous page's `next_cursor` |

```
$ curl -H "Authorization: Bearer rl_..." https://restorelab.example.com/api/v1/recovery-runs
{"items":[{"id":"94bce70d-36c1-470c-b02f-1fa17b6d7714","plan_name":"adhoc-110","source_workload_id":"110","source_name":"linux-test","state":"SUCCESS","result":"SUCCESS","started_at":"2026-09-01T02:44:31.4134064Z","completed_at":"2026-09-01T02:45:08.0705545Z","rto_seconds":33.037,"rto":"33.0s","rto_exceeded":false,"cleanup_done":true}]}
```

`rto_exceeded` is `true` only when the plan carried an RTO target and the
measured RTO went over it; a run with no target never sets it. `completed_at`
is `null` for a run still in progress rather than the zero time — a run that
has not finished must not read as one that finished at the epoch.

A `limit` above 200 is not refused, it is honoured up to 200: the caller gets
a smaller page and a cursor, which is what it wanted anyway. A `limit` of
zero or below is refused — that is a value the caller computed and got
wrong, not a preference to accommodate.

### `GET /api/v1/recovery-runs/{id}`

The full run: steps, checks, RTO, backup, everything `restorelab runs show`
prints. The body is `report.Document`, the exact schema `--format json`
writes to a file and `/recovery-runs/{id}/report?format=json` returns below —
one wire shape for a run, wherever it is read from.

```
$ curl -H "Authorization: Bearer rl_..." https://restorelab.example.com/api/v1/recovery-runs/94bce70d
{"schema":"1.0","run_id":"94bce70d-36c1-470c-b02f-1fa17b6d7714","plan_name":"adhoc-110","source_workload_id":"110","source_name":"linux-test","backup":{...},"steps":[...],"checks":[...],...}
```

### `GET /api/v1/recovery-runs/{id}/events`

The stored progress stream for a run — the same events a live drill printed
to the terminal, as JSON. `?after=<seq>` resumes from a sequence number, for
anything that would rather poll.

```
$ curl -H "Authorization: Bearer rl_..." "https://restorelab.example.com/api/v1/recovery-runs/94bce70d/events?after=12"
{"items":[{"seq":13,"at":"2026-09-01T02:45:02Z","state":"RUNNING_CHECKS","step":"tcp:22","status":"done","message":"reachable"}]}
```

One endpoint serves two representations, chosen by `Accept`. Send
`Accept: text/event-stream` and the same events arrive as they happen — see
[Following a drill live](#following-a-drill-live) below. Both are the same
replay of the same `seq` column, which is why a client can switch from one to
the other without translating anything.

### `GET /api/v1/recovery-runs/{id}/report`

`?format=json` (the default) returns the same `report.Document` as the run
endpoint above. `?format=html` returns the same self-contained HTML report
`restorelab recovery run --format html` writes — open it directly in a
browser.

```
$ curl -H "Authorization: Bearer rl_..." "https://restorelab.example.com/api/v1/recovery-runs/94bce70d/report?format=html" -o report.html
```

### `GET /api/v1/workloads`

The fleet, read from the provider on every call — there is no cache. `?temporary=true`
includes the temporary workloads RestoreLab itself creates during a drill,
which are excluded by default; templates are always excluded.

| Parameter | Meaning |
| --- | --- |
| `provider` | provider id; the configured default when omitted |
| `temporary` | `true` to include RestoreLab-managed workloads |

### `GET /api/v1/workloads/{id}`

One workload, plus its live status when the provider can answer (power
state, uptime, guest agent readiness, IPs). A stopped workload has no agent
to ask, and that is reflected by the absence of `status` in the response, not
by an error.

### `GET /api/v1/workloads/{id}/backups`

Its restore points, from the configured backup provider. A workload with no
backup at all comes back as an empty `items` list, not a 404 — the workload
exists, and "you have nothing to restore" is the honest answer.

### `GET /api/v1/providers`

The configured providers, with every secret already stripped — no sealed
token, no token id either. A token id is half a credential, and nothing here
needs it badly enough to risk a dashboard logging it.

```
$ curl -H "Authorization: Bearer rl_..." https://restorelab.example.com/api/v1/providers
{"items":[{"id":"proxmox-main","kind":"proxmox","roles":["hypervisor","backup"],"endpoint":"https://pve.example.com:8006","insecure":true,"default":true}]}
```

### `GET /api/v1/doctor`

The same readiness diagnostic `restorelab doctor` prints, as JSON — one
implementation of "is this cluster ready for a drill", read by both the CLI
and the API. It always answers `200`, findings and all: a misconfigured
cluster is exactly what this endpoint exists to report, and answering `502`
would make a dashboard draw an outage banner over a diagnostic that worked
perfectly.

| Parameter | Meaning |
| --- | --- |
| `provider` | provider id; the configured default when omitted |
| `workload` | also check one workload's readiness (backup, guest agent) |

```
$ curl -H "Authorization: Bearer rl_..." https://restorelab.example.com/api/v1/doctor
{"provider_id":"proxmox-main","endpoint":"https://pve.example.com:8006","ok":true,"problems":0,"findings":[{"level":"ok","area":"credentials","title":"API reachable, credentials accepted"},{"level":"ok","area":"nodes","title":"1 node(s), 1 online"},{"level":"ok","area":"storage","title":"storage \"local\" (dir): 4 backup(s)"},{"level":"ok","area":"network","title":"isolated bridge \"vmbr99\" present on pve1"}]}
```

## Running a drill

### `POST /api/v1/recovery-runs`

Queues a drill. Needs the `operate` scope. The body describes the drill in the
same terms `restorelab recovery test` takes on the command line — the two
build the same plan through the same code, so a drill triggered over HTTP and
one triggered from a terminal are the same drill.

| Field | Meaning |
| --- | --- |
| `workload_id` | **required** — the source workload to recovery-test |
| `provider` | provider id; the configured default when omitted |
| `backup` | a specific restore point; the most recent one when omitted |
| `checks` | shorthand specs: `tcp:22`, `ping`, `dns:name`, `cmd:...`, or an `http(s)://` URL. Defaults to `tcp:22` |
| `network` | network profile to restore onto; the configured default when omitted |
| `node`, `storage`, `pool` | where the temporary workload lands |
| `rto_target` | a duration such as `5m`; the run is graded against it |
| `skip_startup` | restore without booting or checking anything |

```
$ curl -i -X POST \
    -H "Authorization: Bearer rl_..." \
    -H "Content-Type: application/json" \
    -d '{"workload_id":"110","checks":["tcp:22"],"rto_target":"5m"}' \
    https://restorelab.example.com/api/v1/recovery-runs
HTTP/1.1 201 Created
Location: /api/v1/recovery-runs/94bce70d-36c1-470c-b02f-1fa17b6d7714

{"id":"94bce70d-36c1-470c-b02f-1fa17b6d7714","plan_name":"adhoc-110","source_workload_id":"110","state":"QUEUED","started_at":"2026-09-01T02:44:31.4134064Z","completed_at":null,"rto_seconds":0,"rto":"0s","rto_exceeded":false,"cleanup_done":false}
```

The response is the same shape a run reads back as, so a client that already
parses `GET /recovery-runs` needs no second parser. `started_at` on a queued
run is the moment it was queued; it is rewritten when a worker picks it up.

The plan is built and validated **synchronously, inside the request**, before
any row is written. A body that cannot become a drill — no `workload_id`, an
`rto_target` that is not a duration, a check spec that does not parse — is a
`400` and never a queued row somebody has to explain an hour later.

A second drill of a workload that already has one queued or running is a
`409`:

```json
{"type":"https://restorelab.dev/problems/already-running","title":"This workload already has a drill in flight","status":409,"detail":"run 94bce70d-... is queued or running for workload 110"}
```

Two concurrent drills of the same workload would restore the same backup
twice, and a dashboard where somebody double-clicks must not queue two of
them.

**A run that is interrupted is never replayed.** If the worker executing a
drill dies — a crash, a `kill -9`, a power-cycled machine — the next worker to
reconcile the queue marks that run `FAILED`, attempts to destroy whatever
temporary workload it had created, and moves on. It never picks the drill up
again. That is a decision, not a missing feature: a drill is destructive and
not idempotent, so replaying one would allocate a second temporary id, restore
a second time, and quite possibly leave the first workload running on the
cluster. If a cleanup also fails, the run settles as `CLEANUP_FAILED` and the
error names the exact workload and node, because a silent orphan is the worst
available outcome. Re-triggering an interrupted drill is a decision for a
human, and it is one `POST` away.

### `POST /api/v1/recovery-runs/{id}/cancel`

Asks a drill to stop. Needs the `operate` scope. The status code says which of
two genuinely different things happened:

| Status | Meaning |
| --- | --- |
| `200 OK` | the run was still queued. Nothing had been created anywhere, and it is over: the run reads back `CANCELLED`. |
| `202 Accepted` | a worker is executing it and has been told to stop. **The drill is not over yet.** |

A caller that treated the `202` as a `200` would report a machine gone that
still exists.

**What "cancelled" actually means.** The worker notices the request on its
next lease tick — within about fifteen seconds — and cancels the run's
context. The engine then stops at its next observable point and destroys the
temporary workload on the way out, on a detached context, so the teardown
happens even though the run was cancelled. The run settles as `CANCELLED`,
and `cleanup_done` says whether the temporary workload was actually removed.

What cancellation does **not** do is reach into the cluster. A Proxmox restore
task that has already started is not interrupted: Proxmox keeps writing that
disk to completion, and RestoreLab deletes the result afterwards. So a
cancellation during `RESTORING` can take as long as the restore had left to
run, and the cluster stays busy for that whole time. Read `CANCELLED` as "the
drill stopped and cleaned up after itself", never as "the cluster dropped
everything". An operator who believes the second one will cancel a drill,
watch the cluster stay saturated, and conclude that something is broken.

Cancelling a run that has already settled is a `409`; an unknown id is a
`404`.

### `POST /api/v1/cleanup/{vmid}`

Destroys a temporary workload a drill left behind. Needs the `operate` scope.
This is the one endpoint that ends in a `Delete` against the cluster.

```
$ curl -X POST -H "Authorization: Bearer rl_..." \
    https://restorelab.example.com/api/v1/cleanup/9004
{"workload_id":"9004","removed":true}
```

`?provider=<id>` selects the provider; the configured default is used
otherwise.

The reserved temporary range is checked **before a provider is even looked
up**, so a mistyped production id is a `400` and never becomes a question
asked of the cluster:

```json
{"type":"https://restorelab.dev/problems/invalid-parameter","title":"Invalid parameter","status":400,"detail":"refusing to clean up \"110\": RestoreLab only ever creates workloads in its reserved range 9000-9999, so anything outside it is not one of ours"}
```

That check is the early gate, not the only one. The provider refuses
independently: a workload without `restorelab_managed=true` in its description
is never deleted, whatever id it carries.

### `GET /api/v1/queue`

What is waiting and what is running, with the lease over each row. It needs
only the `read` scope — watching the queue is not operating it.

```
$ curl -H "Authorization: Bearer rl_..." https://restorelab.example.com/api/v1/queue
{"items":[{"id":"94bce70d-...","plan_name":"adhoc-110","source_workload_id":"110","state":"RESTORING","started_at":"2026-09-01T02:44:31Z","completed_at":null,"rto_seconds":0,"rto":"0s","rto_exceeded":false,"cleanup_done":false,"worker":"restorelab-01:2841","lease_expires_at":"2026-09-01T02:46:31Z"}]}
```

This is a listing, not a second source of truth: the same rows
`/recovery-runs` serves, filtered to the states that have not settled. A row
with no `worker` is still waiting for one. A `lease_expires_at` in the past
means the worker holding that run stopped renewing and reconciliation has not
swept it yet — which is exactly the moment an operator wants to be looking at
this endpoint.

## Following a drill live

`GET /api/v1/recovery-runs/{id}/events` with `Accept: text/event-stream`
returns the run's progress as
[Server-Sent Events](https://html.spec.whatwg.org/multipage/server-sent-events.html)
instead of a JSON page. The `read` scope is enough.

```
$ curl -N -H "Authorization: Bearer rl_..." \
       -H "Accept: text/event-stream" \
       https://restorelab.example.com/api/v1/recovery-runs/94bce70d/events

id: 1
event: progress
data: {"seq":1,"at":"2026-09-01T02:44:32Z","state":"DISCOVERING_BACKUP","step":"discover_backup","status":"started"}

id: 2
event: progress
data: {"seq":2,"at":"2026-09-01T02:44:34Z","state":"RESTORING","step":"restore","status":"started","message":"vzdump-qemu-110-2026_08_31.vma.zst"}

: heartbeat

id: 14
event: progress
data: {"seq":14,"at":"2026-09-01T02:45:08Z","state":"CLEANING_UP","step":"cleanup","status":"done"}

event: done
data: {"state":"SUCCESS"}
```

Three event types, and the difference between two of them matters:

| Event | Meaning |
| --- | --- |
| `progress` | one journal entry. `data` is exactly the JSON object the polling representation returns, and `id` is its `seq`. |
| `done` | the run reached a terminal state. `data` carries it: `SUCCESS`, `FAILED`, `CANCELLED` or `CLEANUP_FAILED`. The drill is over. |
| `disconnected` | **this connection** is ending — the server is shutting down. The drill is not over. |

A lone `: heartbeat` comment goes out after fifteen seconds of silence, so a
reverse proxy does not time out a stream that is merely waiting for a guest to
boot. It is a comment, and every SSE client ignores it by design.

**`disconnected` is not `done`.** When `serve` is stopped, every open stream
is told first, then the HTTP server drains — otherwise a stop would wait for
each stream to end, and a stream ends when its drill ends, minutes later. The
frame that goes out at that moment says the connection ended and reports the
last state the stream had seen:

```
event: disconnected
data: {"state":"WAITING_FOR_GUEST","reason":"the server is shutting down"}
```

Sending `done` there would be a lie a dashboard would act on: it would mark a
drill finished — with whatever state it happened to be in — while a worker is
still restoring, booting and checking a machine. Treat `disconnected` as
"reconnect", never as an outcome.

### Resuming

Every `progress` event carries the `seq` already stored in `run_events` as its
SSE `id`. A client that reconnects with `Last-Event-ID: <last seq seen>` gets
exactly what it missed and nothing twice — it is the same
`events after seq` query the JSON page runs for `?after=`, so no translation
happens anywhere. A stream opened without the header replays the run from its
first event, which is what a dashboard opening a run mid-flight wants.

The browser's `EventSource` does this on its own: it remembers the last id and
sends the header when it reconnects. It also cannot set an `Authorization`
header, which is worth knowing before designing around it — either put the
bearer token on at your reverse proxy, or read the stream with `fetch` and
handle reconnection, and `Last-Event-ID`, yourself.

Note that `?after=` is ignored on the streaming representation. The resumption
point of a stream is `Last-Event-ID`, and having two ways to say the same
thing is how they end up disagreeing.

## Pagination

Every listing shares one envelope:

```json
{"items": [...], "next_cursor": "..."}
```

`next_cursor` is present only when there is another page; its absence is how
a client knows it has reached the end. Pass it back as `?cursor=...` to
continue.

The cursor is opaque to the caller — it is base64url of `(started_at, id)` —
and it is a keyset, deliberately never an `OFFSET`. A `LIMIT`/`OFFSET` page
is defined relative to the *current* row count: a drill inserted while a
dashboard is three pages into a listing shifts every row after it, which
either skips a row or repeats one, and nothing about the response tells the
reader it happened. A keyset cursor says "give me everything strictly older
than this position" instead, which stays correct regardless of what gets
inserted while a client pages through it. `internal/store`'s pagination
conformance suite proves exactly this by inserting a fresh run mid-page and
asserting every original row is still seen exactly once.

Treat the cursor as opaque. What it encodes today is `(started_at, id)`;
that it changes shape without warning is the entire point of not documenting
its internals as a contract.

## Errors

Every error is `application/problem+json` ([RFC 9457](https://www.rfc-editor.org/rfc/rfc9457)):

```json
{
  "type": "https://restorelab.dev/problems/not-found",
  "title": "No such recovery run",
  "status": 404,
  "detail": "no recorded drill matches \"abcd\"",
  "instance": "/api/v1/recovery-runs/abcd"
}
```

| Situation | Status |
| --- | --- |
| Your API token is missing or invalid | 401 |
| **Your token is valid but lacks the `operate` scope** | **403** |
| The run id prefix matches nothing | 404 |
| The run id prefix is ambiguous | 409 |
| A query parameter, or a trigger body, is malformed | 400 |
| The workload already has a drill queued or running | 409 |
| The run you tried to cancel has already settled | 409 |
| A cleanup was asked for outside the reserved temporary range | 400 |
| **Proxmox refuses RestoreLab's own provider token** | **502** |
| The cluster does not answer in time | 504 |
| The drill history database is unavailable | 503 |
| Anything else on RestoreLab's side | 500 |

The row that matters most, and the one a carelessly written server gets
wrong: a Proxmox rejection of RestoreLab's *own* provider credentials is a
502, never a 401. A 401 is reserved for one thing only — your bearer token,
against this API. Answering 401 when Proxmox is the one that said no would
send you hunting through your own token for a problem that lives entirely on
RestoreLab's side of the connection, which is the classic way to burn an
afternoon on nothing. The 403 row is the same principle applied to
authorisation: your token is fine, it simply does not carry `operate`.

No error response carries a secret. Every `detail` passes through the same
redaction the CLI uses before it is written: no API token, no sealed
provider secret, no database URL with a password embedded, whether it would
have appeared directly or inside a wrapped error message.

## The confidence score

`GET /api/v1/workloads/{id}/confidence` is the endpoint the tool exists to
answer: *how much can I actually count on this restore?*

```
$ curl -H "Authorization: Bearer rl_..." https://restorelab.example.com/api/v1/workloads/110/confidence
{"workload_id":"110","score":100,"tested":true,"reasons":[],"last_run_id":"94bce70d-36c1-470c-b02f-1fa17b6d7714","runs_considered":2}
```

`score` is `null`, not `0`, for a workload that has never been drilled.
Render that as `--` in a dashboard, never as `0%`: "we have no idea" and "we
know it is bad" are different answers, and collapsing them into the same
number would make an untested workload look exactly as broken as one that
fails every drill.

`reasons` is not a debug field. It is the actual explanation the score is
built from — every penalty the scorer applied, in order — and it is the
value worth reading, not `len(reasons)` and not the number alone. Two
workloads can both score 60 for entirely different reasons (a stale backup
versus a check that keeps failing), and only `reasons` tells them apart.

## What comes next

Recovery plans still live as YAML files on disk, and the only plan the API can
run is the ad-hoc one it builds from a trigger body. Moving plans into the
database, with a CRUD surface validated by `internal/plan`, is the next phase;
a queued run already carries its plan as a snapshot, precisely so that making
plans editable cannot change the shape of a drill between the moment it was
queued and the moment it runs.

The scheduler, notifications and the web dashboard come after that. All three
are consumers of what is documented above rather than new surface underneath
it.

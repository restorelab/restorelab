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

        # Required, not cosmetic: the dashboard's CSRF guard compares Origin
        # against Host, so a proxy that rewrites Host makes every write a 403.
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-Proto $scheme;
    }
}
```

The dashboard needs that TLS. A bearer client can talk to RestoreLab in the
clear on a trusted network; a browser cannot, because the session cookie is
`Secure`. See [deployment.md](deployment.md).

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
restorelab token create <name> --manage     ... one that can also write the plan catalogue
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

## Session

A browser cannot hold a bearer token safely, and `EventSource` cannot set an
`Authorization` header at all. So the dashboard trades a token for a session
cookie once, and the browser carries it from then on.

A session **names a token and carries nothing of its own**. The scopes are
read from the token row on every single request, never copied into the
session, which is what makes `restorelab token revoke` close every session
opened with that token on the next request rather than in twelve hours' time.
A cookie is a different way to present the same credential, never a way to
hold more of it.

```
POST   /api/v1/session    unauthenticated — trade a token for a cookie
GET    /api/v1/session    describe the session this cookie carries
DELETE /api/v1/session    log out
```

### `POST /api/v1/session`

```
$ curl -i -c cookies.txt -X POST https://restorelab.example.com/api/v1/session \
    -H 'Content-Type: application/json' -d '{"token":"rl_..."}'
HTTP/1.1 200 OK
Set-Cookie: __Host-restorelab_session=rls_...; Path=/; Max-Age=43200; HttpOnly; Secure; SameSite=Strict
Content-Type: application/json

{"token_name":"dashboard","scopes":["read","operate"],"expires_at":"2026-09-02T00:00:00Z"}
```

This is the one route that authenticates nothing beforehand: it is what
creates the credential every other route checks. It answers 200 rather than
201, because a session has no URL that identifies *this* session rather than
whoever's cookie arrives — there is no `Location` to give.

`scopes` lists what the caller can actually do, with `read` spelled out even
though no token stores it: it is implied by every token, and a UI deciding
which screens to offer needs the answer it will get, not the row as it
happens to be written.

An unknown or revoked token is **401 and no cookie**, the same rejection every
other route gives. A body that is not `{"token":"rl_..."}` is a 400. A
deployment with no history database is a 503 — the session table is a table,
and saying so beats pretending the login failed.

The secret in the cookie is `rls_` followed by 32 bytes of `crypto/rand`,
base64url. The prefix is there for the reason `rl_` is: a secret that lands in
a log should be recognisable as one, so it can be revoked instead of puzzled
over. Only its SHA-256 is stored.

### The cookie, attribute by attribute

| Attribute | Why |
| --- | --- |
| `__Host-` prefix | A browser refuses to store the cookie unless it is `Secure`, is scoped to `Path=/`, and names no `Domain`. Those three properties are then enforced by the client, where no bug on this side can weaken them and no sibling subdomain can set a cookie that shadows them. |
| `HttpOnly` | A session readable from JavaScript is one an injected script can copy out and use from anywhere. This one can trigger drills. |
| `Secure` | The cookie never travels in clear. |
| `SameSite=Strict` | The browser does not attach it to a request another site started, which is the ordinary CSRF case closed at the client. |
| `Path=/` | Required by `__Host-`, and correct anyway: the dashboard and the API are the same origin. |
| no `Domain` | Required by `__Host-`. A cookie without a `Domain` is not sent to any other host. |
| `Max-Age=43200` | Twelve hours, absolute. |

### Plain HTTP is refused, off loopback

Because the cookie is always `Secure`, a browser on `http://192.168.1.5:8080`
stores nothing: the login appears to succeed, every request afterwards is
anonymous, and no error anywhere explains it. So the route refuses first, and
names the cause:

```
$ curl -i -X POST http://restorelab.lan:8080/api/v1/session -d '{"token":"rl_..."}'
HTTP/1.1 400 Bad Request

{"type":"https://restorelab.dev/problems/invalid-parameter","title":"Invalid parameter","status":400,"detail":"the dashboard session cookie is Secure, so a browser would never send it back over plain HTTP: put TLS in front of RestoreLab, or reach it on localhost"}
```

Loopback is exempt, because browsers treat `localhost` as a trustworthy
origin. `X-Forwarded-Proto: https` is believed — the guard exists against a
misconfiguration, not an attacker, and anyone able to forge that header is
already speaking to the process directly.

### The `Origin` guard on cookie writes

`SameSite=Strict` stops another *site*. What it does not stop is a sibling
subdomain: `app.example.com` and `evil.example.com` are the same site to a
cookie and two different origins to everything else. So every unsafe method
authenticated **by a cookie** must carry an `Origin` matching the request's
own `Host`:

```
$ curl -i -b cookies.txt -X POST https://restorelab.example.com/api/v1/recovery-runs \
    -H 'Origin: https://evil.example' -d '{"workload":"110"}'
HTTP/1.1 403 Forbidden

{"type":"https://restorelab.dev/problems/cross-origin","title":"This request came from another origin","status":403,"detail":"a session cookie only writes from the dashboard it was issued to; an API client should send `Authorization: Bearer rl_...` instead"}
```

A missing `Origin` is refused too: browsers send it on every write, so its
absence on a cookie request is not the ordinary case.

The reference is the request's own `Host`, not a configured origin — the
dashboard is served by this same binary, so the legitimate origin is by
construction the one just reached, and a value to configure is a value to get
wrong. **This is a deployment requirement**: a reverse proxy that does not
pass the original `Host` makes every dashboard write a 403. See
`docs/deployment.md`.

The guard never applies to a bearer request. `GET`, `HEAD` and `OPTIONS` are
exempt whatever the credential.

### Expiry

Twelve hours from the moment the session is opened, absolute, never extended.
A sliding expiry would be more comfortable — nobody would be logged out
mid-drill — but an open tab polling a listing would then hold a session
forever, and this one can destroy machines. Twelve hours covers a working day;
coming back tomorrow means logging in.

An expired session is a 401, exactly like an unknown one. So is a session
whose token has been revoked, from the next request onwards. Opening a session
also sweeps every session that has already expired, in the same transaction —
the table cleans itself at exactly the rate it fills.

### `GET /api/v1/session`

```
$ curl -b cookies.txt https://restorelab.example.com/api/v1/session
{"token_name":"dashboard","scopes":["read","operate"],"expires_at":"2026-09-02T00:00:00Z"}
```

What a dashboard calls on load to decide between its login screen and its
application, and what tells it which actions to offer at all. No cookie and no
token is a 401. A request authenticated with a **bearer token** is a 400: this
route describes a cookie session, and a bearer request has none.

### `DELETE /api/v1/session`

```
$ curl -i -b cookies.txt -X DELETE https://restorelab.example.com/api/v1/session \
    -H 'Origin: https://restorelab.example.com'
HTTP/1.1 204 No Content
Set-Cookie: __Host-restorelab_session=; Path=/; Max-Age=0; HttpOnly; Secure; SameSite=Strict
```

The row is deleted and the cookie is expired whatever happened — a browser
holding a cookie for a row that is gone would keep sending it forever. Calling
it twice is not a failure: the second call arrives with a cookie the store no
longer knows, so it is simply unauthenticated (401). It is an unsafe method,
so it is subject to the `Origin` guard like every other cookie write.

## Scopes

A token holds one or more levels of access:

| Scope | What it allows |
| --- | --- |
| `read` | every `GET` route, including the live event stream and the plan catalogue |
| `operate` | `read`, plus triggering a drill, cancelling one, and cleaning up a temporary workload |
| `manage` | `read`, plus creating, changing and deleting stored plans |

`read` is the default and is implied by every token, including an `operate`
one: an account that could start a drill but not watch it would be a strange
thing to hand anyone.

**`operate` and `manage` do not imply each other**, in either direction, and
that is the whole reason `manage` exists as a separate scope rather than as
more room inside `operate`. Triggering a drill and deciding what a drill *is*
are two different powers. A token handed to a dashboard so it can launch and
cancel has no business rewriting the definition of what it launches — and a
token given to a CI job so it can `plan apply` from a git repository has no
business restoring backups by itself. A token can hold both; it has to say so.

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
GET  /                                     unauthenticated — the dashboard
GET  /api/v1/health                        unauthenticated
GET  /api/v1/session                       describe this cookie session
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
GET  /api/v1/plans                         workload, limit
GET  /api/v1/plans/{ref}                   format=yaml for the document itself
GET  /api/v1/schedule                      the plans that drill themselves
GET  /api/v1/schedule/slots                plan, workload, limit

POST   /api/v1/session                     unauthenticated — token for a cookie
DELETE /api/v1/session                     log out
POST   /api/v1/recovery-runs               operate — queue a drill
POST   /api/v1/recovery-runs/{id}/cancel   operate — stop one
POST   /api/v1/cleanup/{vmid}              operate — destroy a leftover workload
POST   /api/v1/plans/validate              manage  — check a document, store nothing
POST   /api/v1/plans                       manage  — store a new plan
PUT    /api/v1/plans/{ref}                 manage  — replace one, version=N to guard
DELETE /api/v1/plans/{ref}                 manage  — remove one
```

`GET /` is the dashboard, served from the binary. It needs no token — it is
the login screen as much as the application, and the API underneath it checks
every request the page then makes. Anything that is not a file in the bundle
falls back to `index.html`, so the client's own router owns `/runs/94bce70d`.
A path under `/api/` that matched no route still answers as the API, never
with HTML.

`{id}` on a run accepts a prefix, exactly like `restorelab runs show`: an
exact match wins, an ambiguous prefix comes back as a 409 rather than a
guess. That applies to `/cancel` as much as to the reads.

`{ref}` on a plan is a **name first**, then a full id, then a unique id
prefix. The name wins outright, because it is what a human types and what a
plan is called everywhere else; an ambiguous id prefix is a 409, never a
guess.

### `GET /api/v1/schedule`

The plans that drill themselves, and when each one drills next. A plan with no
`schedule` is absent from the listing; most plans have none.

```json
{
  "items": [
    {
      "plan_id": "1f0b2a44-0000-4000-8000-00000000000a",
      "name": "web-tier",
      "workload_id": "110",
      "schedule": "0 3 * * *",
      "timezone": "UTC",
      "next_slot_at": "2026-09-04T03:00:00Z",
      "last_slot": {
        "slot_at": "2026-09-01T04:00:00Z",
        "outcome": "skipped",
        "reason": "the slot was 6h0m late, past the 2h grace period: ..."
      }
    }
  ]
}
```

`next_slot_at` is `null` when the schedule could not be read, and `error` then
says why. **A plan whose cron stopped parsing stays in the listing**, carrying
its error: it is a plan somebody believes is scheduled, and dropping it would
read as "not scheduled" for a machine that has silently stopped being tested.

### `GET /api/v1/schedule/slots`

The slots the scheduler has decided, most recent first. `plan` narrows to one
plan (name, id, or unique id prefix); `workload` narrows to every plan
covering one machine, which is the shape the question usually has.

```json
{
  "items": [
    {
      "plan_id": "1f0b2a44-...",
      "plan_name": "web-tier",
      "slot_at": "2026-09-01T04:00:00Z",
      "decided_at": "2026-09-01T10:00:00Z",
      "outcome": "skipped",
      "reason": "the slot was 6h0m late, past the 2h grace period: ..."
    }
  ]
}
```

`outcome` is `queued` or `skipped`. A queued slot names its `run_id`; a
skipped one carries a `reason` and no run — and that row is the only place
"why was this machine not tested" can be answered from, because a skipped slot
produced no run to look at.

There is no write here. A slot is decided by the scheduler, which queues the
drill in the same transaction, and an HTTP handler has no business doing that.

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
| `state` | one of `QUEUED`, `DISCOVERING_BACKUP`, `PREPARING_ENVIRONMENT`, `RESTORING`, `STARTING`, `WAITING_FOR_GUEST`, `RUNNING_CHECKS`, `GENERATING_REPORT`, `CLEANING_UP`, `SUCCESS`, `FAILED`, `CANCELLED`, `CLEANUP_FAILED`, `INCONCLUSIVE` |
| `result` | `SUCCESS`, `DEGRADED` or `FAILED` |
| `since` | `30d`, `12h`, a date (`2026-08-01`), or a full RFC 3339 instant |
| `limit` | page size, default 50, capped at 200 |
| `cursor` | opaque, from a previous page's `next_cursor` |

```
$ curl -H "Authorization: Bearer rl_..." https://restorelab.example.com/api/v1/recovery-runs
{"items":[{"id":"94bce70d-36c1-470c-b02f-1fa17b6d7714","plan_name":"adhoc-110","source_workload_id":"110","source_name":"linux-test","state":"SUCCESS","result":"SUCCESS","started_at":"2026-09-01T02:44:31.4134064Z","completed_at":"2026-09-01T02:45:08.0705545Z","rto_seconds":33.037,"rto":"33.0s","rto_exceeded":false,"cleanup_done":true,"proof_level":"BOOT"}]}
```

`rto_exceeded` is `true` only when the plan carried an RTO target and the
measured RTO went over it; a run with no target never sets it. `completed_at`
is `null` for a run still in progress rather than the zero time — a run that
has not finished must not read as one that finished at the epoch.

`proof_level` is what the drill **established** — `NONE`, `BOOT`, `SERVICE` or
`DATA` — as opposed to `result`, which says how the drill went. The two are
different sentences and a listing that shows only the second one is the
reassuring kind of useless: a `SUCCESS` at `BOOT` is a real success that proved
the kernel comes up and nothing else. See
[architecture.md](architecture.md#the-proof-level) for the ladder and
[recovery-plans.md](recovery-plans.md#proves) for where a check's level comes
from. The field is **absent** on a run recorded before it existed, and that
means "not recorded", not "nothing was proven" — render the two differently.

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
{"schema":"1.0","run_id":"94bce70d-36c1-470c-b02f-1fa17b6d7714","plan_name":"adhoc-110","source_workload_id":"110","source_name":"linux-test","proof_level":"BOOT","backup":{...},"steps":[...],"checks":[...],...}
```

The document carries `proof_level` too, with the same meaning and the same
absence on a run that predates it. It was added without bumping the document's
schema: adding an optional field is not a breaking change, and the rule is
written next to the constant in `internal/report/json.go` so the next person
does not have to relitigate it.

### `GET /api/v1/recovery-runs/{id}/events`

The stored progress stream for a run — the same events a live drill printed
to the terminal, as JSON. `?after=<seq>` resumes from a sequence number, for
anything that would rather poll.

```
$ curl -H "Authorization: Bearer rl_..." "https://restorelab.example.com/api/v1/recovery-runs/94bce70d/events?after=12"
{"items":[{"seq":13,"at":"2026-09-01T02:45:02Z","state":"RUNNING_CHECKS","step":"hostname","status":"done","message":"web-01"}]}
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

Every workload also carries its most recent drill, so a listing can be
coloured without one request per row:

| Field | Meaning |
| --- | --- |
| `last_run_id` | the drill's id, for a link straight to it |
| `last_run_at` | when that drill **started** — a drill still running has no completion time, and "last tested" has to have an answer while it is in flight |
| `last_run_state` | its state, `SUCCESS` through `CLEANUP_FAILED` |
| `last_run_result` | its grade, absent while the drill is still going or when it was cancelled |
| `last_run_proof` | what that drill established: `NONE`, `BOOT`, `SERVICE` or `DATA` |

All five are absent from a workload that has never been drilled. That is the
difference between "never tested" and "tested, and it went badly", and a
client that cannot tell them apart will render one as the other.

`last_run_proof` is absent for a second reason as well: a drill recorded
before the field existed has no level, and "not recorded" is not the claim
"nothing was proven". A screen that collapses the two would mark down every
workload in an existing installation on the strength of a fact nobody ever
wrote down.

They come from one query over the drill history for the whole page, and they
deliberately stop short of the confidence score: scoring reaches the backup
provider for a restore point's date, which would be one cluster round-trip per
row. The score stays on its own route below.

Reading them is best-effort. A deployment whose history database cannot be
read still answers with its inventory, simply without these fields — the same
shape a never-drilled workload has.

### `GET /api/v1/workloads/{id}`

One workload, plus its live status when the provider can answer (power
state, uptime, guest agent readiness, IPs). A stopped workload has no agent
to ask, and that is reflected by the absence of `status` in the response, not
by an error.

### `GET /api/v1/workloads/{id}/backups`

Its restore points, from the configured backup provider. A workload with no
backup at all comes back as an empty `items` list, not a 404 — the workload
exists, and "you have nothing to restore" is the honest answer.

### `GET /api/v1/setup` and `POST /api/v1/setup`

**These two exist only on a server that has no cluster connected.** They are
not mounted otherwise — a configured RestoreLab answers 404, not 403, because
the absence of a route is a stronger guarantee than a check on one.

`GET` says that installing is possible and needs no token: somebody who
opened the bare address has to be told what to paste, and the answer reveals
nothing they could not learn from the port being open.

`POST` provisions the cluster. It needs the one-time token `serve` printed on
its console, as `Authorization: Bearer rls_...`, and that token is spent by
this request whether it succeeds or fails. Over plain HTTP to anything but
this machine it is refused with a 400 naming TLS, before the body is read.

```
$ curl -X POST -H "Authorization: Bearer rls_..." -H "content-type: application/json" \
    -d '{"endpoint":"https://pve.example.com:8006","admin_user":"root@pam",
         "admin_password":"...","storages":["local-zfs"],
         "create_bridge":true,"apply_bridge":true}' \
    http://127.0.0.1:8080/api/v1/setup
```

The answer carries the provisioning steps in the order they were performed,
the provider it stored, the bridge it created, and — exactly once — an API
token named `dashboard` with the read, operate and manage scopes. A refusal
is a `problem+json` carrying the same `steps`, so a caller can show how far it
got; every step is idempotent, so running it again is safe.

The administrator password is used in memory and never stored, logged or
echoed back. See `docs/security.md`, "The first-run setup token".

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

Queues a drill. Needs the `operate` scope. A body either **names a stored
plan** or **describes a drill**:

```json
{"plan": "web-tier"}
```

| Field | Meaning |
| --- | --- |
| `plan` | a stored plan, by name or id. Exclusive of every field below |

Naming a plan is enough on its own: the workload, the provider, the checks and
the RTO target all come from the plan, so the body says nothing the plan
already says. The plan is loaded, defaulted and validated inside the request;
an unknown one is a `404`, and one that no longer parses is a `400` naming the
field at fault, never a queued row that fails an hour later. Triggering stays
in `operate` — naming a plan in order to run it does not require the right to
write one.

The other form describes the drill in the same terms `restorelab recovery
test` takes on the command line — the two build the same plan through the same
code, so a drill triggered over HTTP and one triggered from a terminal are the
same drill.

| Field | Meaning |
| --- | --- |
| `workload_id` | **required** in this form — the source workload to recovery-test |
| `provider` | provider id; the configured default when omitted |
| `backup` | a specific restore point; the most recent one when omitted |
| `checks` | shorthand specs: `tcp:22`, `ping`, `dns:name`, `cmd:...`, or an `http(s)://` URL. Defaults to `cmd:hostname`, which runs inside the guest and needs no route into the isolated recovery network. A network check that cannot reach the guest ends the run `INCONCLUSIVE`, not `FAILED` |
| `network` | network profile to restore onto; the configured default when omitted |
| `node`, `storage`, `pool` | where the temporary workload lands |
| `rto_target` | a duration such as `5m`; the run is graded against it |
| `skip_startup` | restore without booting or checking anything |

```
$ curl -i -X POST \
    -H "Authorization: Bearer rl_..." \
    -H "Content-Type: application/json" \
    -d '{"workload_id":"110","checks":["cmd:systemctl is-active ssh"],"rto_target":"5m"}' \
    https://restorelab.example.com/api/v1/recovery-runs
HTTP/1.1 201 Created
Location: /api/v1/recovery-runs/94bce70d-36c1-470c-b02f-1fa17b6d7714

{"id":"94bce70d-36c1-470c-b02f-1fa17b6d7714","plan_name":"adhoc-110","source_workload_id":"110","state":"QUEUED","started_at":"2026-09-01T02:44:31.4134064Z","completed_at":null,"rto_seconds":0,"rto":"0s","rto_exceeded":false,"cleanup_done":false}
```

The response is the same shape a run reads back as, so a client that already
parses `GET /recovery-runs` needs no second parser. `started_at` on a queued
run is the moment it was queued; it is rewritten when a worker picks it up.

A run queued from a stored plan carries `plan_id` — in this response, in
every listing afterwards, and in the run's own report, which adds
`plan_version` so a report can say *which version* of the plan ran. A
dashboard can therefore group drills by plan without fetching each one, and a
report read six months later still names the plan revision it was graded
against. An ad-hoc drill has neither field, and both are simply absent rather
than empty.

Deleting the plan clears `plan_id` and nothing else: the name, the version
that ran, the timeline, the checks and the verdict are untouched, because the
plan a run executed is copied into the run rather than referenced.

**The two forms cannot be mixed.** A body carrying both `plan` and an ad-hoc
field is a `400` that names the fields in conflict:

```json
{"type":"https://restorelab.dev/problems/bad-request","title":"That request cannot be served as written","status":400,"detail":"a request either names a plan or describes a drill, not both: drop checks, workload_id, or drop \"plan\""}
```

Merging them would be convenient and expensive. Two runs of the same plan
could then differ with nothing to signal it, and "what does this plan do"
would stop having a single answer. A drill that deviates from a plan is an
ad-hoc drill, and this endpoint already knows how to make one.

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

## The plan catalogue

A stored plan is what `POST /recovery-runs` triggers by name and what the
scheduler will reference. Storing one is not a condition for running it: a
plan file on disk still runs directly with `restorelab recovery run <file>`,
with no database involved. Storing it is how it becomes something other
machines can name.

### `GET /api/v1/plans`

The catalogue, ordered by name. `read` is enough.

```
$ curl -H "Authorization: Bearer rl_..." https://restorelab.example.com/api/v1/plans
{"items":[{"id":"2f1a4c76-...","name":"web-tier","description":"nightly drill","workload_id":"110","provider_id":"proxmox-main","version":3,"created_at":"2026-08-14T09:12:00Z","updated_at":"2026-09-01T07:41:00Z"}]}
```

`?workload=110` narrows it to one workload's plans. The listing carries **no
documents**: a catalogue of fifty plans must not ship fifty YAML files to draw
a table. There is no cursor either — the catalogue is dozens of rows on a
stable ordering, and a keyset over that would be ceremony. `limit` exists so a
listing is never unbounded.

### `GET /api/v1/plans/{ref}`

One plan, document included.

```
$ curl -H "Authorization: Bearer rl_..." \
    https://restorelab.example.com/api/v1/plans/web-tier?format=yaml > web-tier.yaml
```

`format=yaml` returns the document naked, as `application/yaml`, so a plan can
be written straight to a file without pulling a string out of a JSON object
and unescaping it. Without it, the JSON carries the document in `yaml`.

**The document comes back exactly as it was submitted** — comments, key order,
everything. A plan exported and re-imported is the same bytes. What each run
actually executed is kept separately, in that run's snapshot, so there is no
information to gain from canonicalising the text and a comment to lose.

### `POST /api/v1/plans/validate`

Says whether a document is a valid plan, and stores nothing. Needs `manage`.

```
$ curl -X POST -H "Authorization: Bearer rl_..." \
    -H "Content-Type: application/yaml" \
    --data-binary @web-tier.yaml \
    https://restorelab.example.com/api/v1/plans/validate

{"valid":true,"name":"web-tier","workload_id":"110","provider_id":"pve","normalized_yaml":"name: web-tier\n...","proof_level":"SERVICE","proof_summary":"SERVICE, the service would be verified, the data would not"}
```

`normalized_yaml` is the document with its defaults applied — "here is what
this actually says". It is the difference between a field left out and a field
left out *meaning something*, which is exactly what an editor needs to show
before anyone commits to it.

`proof_level` and `proof_summary` say what this plan **would** establish if
every one of its checks passed. They read in the conditional on purpose: a
plan has not drilled anything, so "the service was verified" would be a claim
about a run nobody made. They are here because "is this document valid" and
"is this document worth running" are different questions, and only the first
one used to have an answer on this route. A plan whose only check is a liveness
probe answers `BOOT`, which is worth knowing while it is still five seconds'
work to improve. The same summary is what `restorelab plan validate` prints.

An invalid document is a `400` carrying every problem found, in the same shape
`POST /plans` returns. The route requires `manage` even though it writes
nothing: it is the plan editor's own route, and the editor is the thing
`manage` exists to gate.

It goes through the same `catalog.Validate` the write routes use. A second
definition of "a valid plan", in TypeScript, would drift from this one at the
first field added on this side.

### `POST /api/v1/plans`

Stores a new plan. Needs `manage`.

**The body is the plan document itself**, not an envelope around it. `yaml.v3`
reads JSON as YAML, so a dashboard can send JSON without this project
inventing and maintaining a second schema for the same thing.

```
$ curl -i -X POST \
    -H "Authorization: Bearer rl_..." \
    -H "Content-Type: application/yaml" \
    --data-binary @web-tier.yaml \
    https://restorelab.example.com/api/v1/plans
HTTP/1.1 201 Created
Location: /api/v1/plans/2f1a4c76-0b1e-4d2a-9a51-1d0f8c2b3e44
```

The document is parsed, defaulted and validated before anything is written; an
invalid one is a `400` carrying **every** problem `Validate` found, not just
the first. A name another plan already holds is a `409`: creating means
creating, and a `POST` must never quietly replace somebody else's plan.

### `PUT /api/v1/plans/{ref}`

Replaces a plan and increments its version. Needs `manage`.

```
$ curl -i -X PUT -H "Authorization: Bearer rl_..." \
    --data-binary @web-tier.yaml \
    "https://restorelab.example.com/api/v1/plans/web-tier?version=3"
```

`?version=N` is an optional guard: if the plan is no longer at that version,
somebody wrote in between and the answer is a `409` rather than an overwrite.
Omit it to overwrite whatever is there — which is what a CI applying a
directory of plans wants, since it has no idea what the current version is.

A document naming a *different* plan than the URL is a `409`, and nothing is
written. Renaming a plan is deleting one and creating another; it is worth
saying out loud rather than doing quietly behind a `PUT`.

### `DELETE /api/v1/plans/{ref}`

Removes a plan. Needs `manage`. Answers `204`.

**Its runs are not touched.** They keep their name and the copy of the plan
they actually executed, so their reports, their timelines and the confidence
score read identically before and after — only the `plan_id` link goes. A
drill in flight is unaffected too: the worker executes the snapshot taken when
the run was queued, never the catalogue row, so deleting a plan cannot disturb
an execution or rewrite a report.

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
| `done` | the run reached a terminal state. `data` carries it: `SUCCESS`, `FAILED`, `CANCELLED`, `CLEANUP_FAILED` or `INCONCLUSIVE`. The drill is over. |
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
sends the header when it reconnects. It cannot set an `Authorization` header —
which is why the dashboard authenticates with a session cookie instead.
`EventSource` sends cookies without being asked, so a browser gets the
reconnection, the backoff and the resumption for nothing. An API client
holding a bearer token reads the stream with whatever HTTP client it already
has.

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
| **Your token is valid but lacks the `operate` or `manage` scope** | **403** |
| The run id prefix matches nothing | 404 |
| The run id prefix is ambiguous | 409 |
| A query parameter, or a trigger body, is malformed | 400 |
| The workload already has a drill queued or running | 409 |
| A plan reference matches nothing | 404 |
| A plan id prefix is ambiguous | 409 |
| A plan document does not parse, or `Validate` rejects it | 400 |
| A `POST /plans` uses a name another plan already holds | 409 |
| A `PUT /plans` carries a `version` that is no longer current | 409 |
| A `PUT /plans` document names a different plan than the URL | 409 |
| A trigger body carries both `plan` and an ad-hoc field | 400 |
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
authorisation: your token is fine, it simply does not carry the scope.

The same reasoning shapes the plan rows. A `404` on a plan says "No such
plan", not "No such recovery run": the two are different tables, and pointing
a reader at the wrong one costs the same afternoon.

No error response carries a secret. Every `detail` passes through the same
redaction the CLI uses before it is written: no API token, no sealed
provider secret, no database URL with a password embedded, whether it would
have appeared directly or inside a wrapped error message.

## The confidence score

`GET /api/v1/workloads/{id}/confidence` is the endpoint the tool exists to
answer: *how much can I actually count on this restore?*

```
$ curl -H "Authorization: Bearer rl_..." https://restorelab.example.com/api/v1/workloads/110/confidence
{"workload_id":"110","score":60,"tested":true,"reasons":["only the boot was verified (capped at 60)"],"last_run_id":"94bce70d-36c1-470c-b02f-1fa17b6d7714","runs_considered":2,"proof_level":"BOOT","proof_cap":60}
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

`proof_level` and `proof_cap` say what the newest drill that reached a verdict
established, and the **ceiling** that puts on the score. The ceiling is applied
last, after every penalty: `NONE` → 40, `BOOT` → 60, `SERVICE` → 85, `DATA` →
no ceiling. A workload drilled daily, on time, from a fresh backup, whose only
check prints its hostname has earned every point the penalties leave it and
still proven only that the kernel boots — so it is capped, and the reason that
says so is in `reasons`.

The two fields are here so a client can *explain* the number instead of
printing it: a 60 next to "only the boot was verified" is a sentence, a bare 60
is a mystery. Both are absent when no drill that reached a verdict recorded a
level — a history from before the field, or a workload whose only runs are
still in flight — and an absent level caps nothing at all. See
[architecture.md](architecture.md#the-ceiling-on-the-confidence-score) for why
it is a ceiling rather than another penalty, and how to retune it.

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

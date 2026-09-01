# HTTP API

RestoreLab exposes its drill history and the state of your fleet over HTTP,
under `/api/v1`. This document covers phase B1: everything it serves today is
read-only.

## What it does and does not do

Every route under `/api/v1` answers a question; none of them changes
anything. No handler in `internal/api` calls a mutating provider method —
`Restore`, `Start`, `Stop`, `Delete` or `AllocateWorkloadID` — and that is not
a promise kept by code review. The fake provider the handler tests run
against fails the test outright if any of those methods is ever reached, and
a second test greps the package for the same four names outside `_test.go`
files. Triggering a drill, cancelling one, or cleaning up a workload over HTTP
is phase B2, on a server whose read paths will already be proven.

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

## Authenticating

```
restorelab token create <name>    mints a token, prints it once
restorelab token list             name, created, last used, state
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

## The surface

```
GET /api/v1/health                        unauthenticated
GET /api/v1/recovery-runs                 workload, state, result, since, limit, cursor
GET /api/v1/recovery-runs/{id}             id complete or a prefix
GET /api/v1/recovery-runs/{id}/events      after=<seq>
GET /api/v1/recovery-runs/{id}/report      format=json|html
GET /api/v1/workloads                      provider, temporary
GET /api/v1/workloads/{id}                 provider
GET /api/v1/workloads/{id}/backups         provider
GET /api/v1/workloads/{id}/confidence      provider
GET /api/v1/providers
GET /api/v1/doctor                         provider, workload
```

`{id}` on a run accepts a prefix, exactly like `restorelab runs show`: an
exact match wins, an ambiguous prefix comes back as a 409 rather than a
guess.

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
to the terminal, as JSON. `?after=<seq>` resumes from a sequence number: it
is the same replay B2's Server-Sent Events endpoint will do on
`Last-Event-ID`, usable today by anything willing to poll.

```
$ curl -H "Authorization: Bearer rl_..." "https://restorelab.example.com/api/v1/recovery-runs/94bce70d/events?after=12"
{"items":[{"seq":13,"at":"2026-09-01T02:45:02Z","state":"RUNNING_CHECKS","step":"tcp:22","status":"done","message":"reachable"}]}
```

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
| The run id prefix matches nothing | 404 |
| The run id prefix is ambiguous | 409 |
| A query parameter is malformed | 400 |
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
afternoon on nothing.

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

B1 stops at reading. Two further phases are already scoped:

- **B2** adds the queue, the worker, and the write paths: `POST
  /recovery-runs` to trigger a drill, `POST /{id}/cancel` to cancel one,
  cleanup of an orphaned temporary workload, and Server-Sent Events on
  `/{id}/events` for a live-updating dashboard instead of polling.
- **B3** moves recovery plans into the database, with a CRUD surface
  validated by `internal/plan`.

The absence of any write endpoint here is a deliberate cut, not an
oversight: B1 is a server that can be exposed and tested against a real
cluster without any risk of it changing anything, and B2 builds the
dangerous part on top of foundations already proven.

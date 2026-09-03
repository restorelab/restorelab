# Architecture

RestoreLab is a **modular monolith** in Go. One binary, several roles
(`server`, `worker`, `probe`, plus the CLI), one codebase. Microservices,
Temporal, Kafka and a service mesh are explicit non-goals: the hard part of
this product is being correct about Proxmox and about cleanup, not distributing
itself.

## Dependency direction

```text
        cli ──────────────┬───────────────┐
         │                │               │
         ▼                ▼               ▼
        api            worker          report
         │                │               ▲
         │                ▼               │
         │           recovery (engine) ───┘
         │            │  │  │
         │  ┌─────────┘  │  └──────────┐
         │  ▼            ▼             ▼
         │ plan       checks      providers/{proxmox,pbs}
         │  │            │             │
         ▼  └────────────┴─────────────┘
       store             ▼
         └────────────► core
```

`api` sits beside `cli` rather than under it: both are entry points into the
same domain, not one wrapping the other. `api` imports `core`, `store`,
`report`, `config`, `diag`, `adhoc`, `worker` and `version`, and deliberately
**not** `internal/providers` and **not** `crypto`. Unsealing a provider
secret needs the master key, and keeping that on the CLI's side of an
interface (`api.ProviderSet`, implemented in `internal/cli/serve.go`) is what
stops the API package from ever being able to import them. A provider client
is something the CLI hands the API, not something the API knows how to build.

**`api` and `worker` do not know each other.** The API writes a queued row;
the worker claims it. Neither imports the other for that purpose, neither
holds a reference to the other, and the only thing they share is the database.
That is what makes running them as one process or two a deployment flag
(`serve --no-worker` / `serve --no-listen`) rather than a rewrite, and it is
why the queue's correctness lives in SQL (a claim that excludes a run
another worker already holds) rather than in a mutex that only works inside
one process.

The one edge from `api` to `worker` is `worker.Cleanup`, the single mutating
provider call reachable from an HTTP request. It points that way on purpose:
the package that owns the destructive calls owns their guards and their
tests, and duplicating those in `api` would create a second place for them to
drift. No handler in `internal/api` calls `Restore`, `Start`, `Stop`, `Delete`
or `AllocateWorkloadID` itself, and a test greps the package to keep it that
way.

**Everything points at `core`, `core` points at nothing.** `core` holds the
domain model (`Workload`, `Backup`, `RecoveryRun`, `CheckResult`) and the four
interfaces that make the rest replaceable:

| Interface | Implemented by | Purpose |
| --- | --- | --- |
| `core.HypervisorProvider` | `providers/proxmox` | list / restore / start / stop / delete workloads |
| `core.BackupProvider` | `providers/proxmox`, `providers/pbs` | find restore points |
| `core.Check` | `checks/*` | validate a restored service |
| `core.NetworkValidator`, `core.CapacityReporter` | optional, `providers/proxmox` | prove isolation, report free capacity |

Adding VMware or Hyper-V means writing one package under `providers/` and
registering it. The engine never learns a provider's name.

## Packages

```text
cmd/restorelab          entrypoint, role dispatch
internal/
    core                domain model + interfaces (no dependencies)
    plan                recovery plan YAML: parse, default, validate
    config              on-disk configuration, provider registry, network profiles
    crypto              AES-256-GCM secret sealing, master key resolution
    providers/proxmox   Proxmox VE API client + hypervisor & backup provider
    providers/pbs       Proxmox Backup Server API client + backup provider
    checks              check registry, retries, timeouts + ping/tcp/http/dns
    recovery            the engine: workflow, isolation, cleanup, RTO, grading
    adhoc               a drill described by a workload id, turned into a plan
    journal             a run recorded as it happens: row, events, checks, timeline
    store               drill history, queue and leases, tokens (SQLite / PostgreSQL)
    report              text / JSON / HTML reports, recovery confidence score
    diag                doctor's readiness checks, as data (Level, Finding, Report)
    api                 the HTTP API: routing, auth and scopes, pagination, SSE, problem+json
    worker              the queue loop: claim, execute, renew, release, reconcile
    cli                 cobra commands
    version             build metadata
```

Three of those exist because two entry points needed the same thing and a
second implementation would have become a second answer:

- **`adhoc`** builds the plan behind a drill described by nothing more than a
  workload id. It moved out of `internal/cli` when the API gained
  `POST /recovery-runs`: a drill triggered over HTTP and one triggered from a
  terminal must be the same drill, defaults included.
- **`journal`** records a run as it happens. It moved out of `internal/cli`
  for the same reason: the CLI and the worker write the same history, and two
  implementations of "what happened during this drill" would drift into two
  stories about the same run. Nothing it does returns an error, deliberately:
  a locked database must never abort a destructive drill, and a compiler
  enforcing that is worth more than a convention.
- **`worker`** holds the only mutating provider calls reachable from an HTTP
  request, and reaches them through `recovery.Engine`.

- **`scheduler`** queues the drills a stored plan's cron asks for, and does
  nothing else. It holds no provider and no engine, and cannot be constructed
  with either: automating drills had to add no destructive surface, and the
  compiler is what guarantees it. Idempotence lives in the database: a slot is
  a row keyed by `(plan_id, slot_at)`, claimed in the same transaction as the
  run it queues, so a scheduler that dies mid-write cannot drill twice.
- **`trigger`** builds and queues a run: the conflict check, the plan snapshot,
  the row. The API and the scheduler both go through it, because a drill
  launched by a cron and one launched from the dashboard must obey the same
  guards.

Planned, not yet present: `notifications`, `audit`, `probe`.

## The recovery workflow

The engine is a linear state machine with one guarantee: **from the moment a
temporary workload might exist, cleanup runs (on success, on failure, on
timeout, on cancellation, on panic).**

```text
QUEUED
  ↓ discover_backup        resolve latest/specific, enforce max_age
DISCOVERING_BACKUP
  ↓ prepare_environment    verify isolation, verify capacity, allocate temp ID
PREPARING_ENVIRONMENT
  ↓ restore                create temp workload, wait for the task,
RESTORING                    then harden it (network rewrite, limits, metadata)
  ↓ start
STARTING
  ↓ wait_for_guest         poll status until powered on and addressable
WAITING_FOR_GUEST
  ↓ run_checks             cmd (in-guest) / tcp / http / dns / ping, with retries
RUNNING_CHECKS
  ↓ generate_report
GENERATING_REPORT
  ↓ cleanup                stop + delete, on a detached context
CLEANING_UP
  ↓
SUCCESS | DEGRADED | FAILED | INCONCLUSIVE | CLEANUP_FAILED
```

**RTO** is measured from the start of the run to the end of the checks. Cleanup
and report generation are excluded: they are RestoreLab's housekeeping, not part
of the recovery a business would experience.

**Grading**: every critical check passed and the RTO target met → `SUCCESS`;
recovered but a non-critical check failed or the RTO target was exceeded →
`DEGRADED`; a step failed or a critical check failed → `FAILED`.

A fourth ending exists for the case where the drill reached no verdict at all:
the workload restored and booted, but a critical check **could not be
evaluated** → `INCONCLUSIVE`, carrying no result. It is a distinct ending
because RestoreLab isolates the recovery network on purpose, so a `tcp:`,
`http:`, `dns:` or `ping` check dialled from wherever RestoreLab runs comes
back silent unless the operator arranged a route. That silence is a fact about
the operator's topology, not about their backup. Grading it `FAILED` would
charge a workload's confidence score for where it was tested from, and a
report nobody can trust is worth less than no report: the same reasoning that
already makes a cancelled run carry no verdict.

Because that ending has to be reachable, the check layer distinguishes "the
target answered, badly" from "nothing answered": `internal/checks/reachability.go`
classifies by errno rather than by message text, per platform. Windows forced
that. There, `net.Error.Timeout()` reports false for `WSAETIMEDOUT`, the
portable `syscall.ECONNREFUSED` is a placeholder that never matches a real
dial, and the message strings are localised.

## The proof level

`INCONCLUSIVE` is one half of the honesty rule: RestoreLab does not accuse a
backup it could not verify. The proof level is the other half: **it does not
congratulate one it did not verify either.**

A run carries a level beside its verdict, and the two answer different
questions: the verdict says how the drill went, the level says what it
established. A `SUCCESS` at `BOOT` is a real success that proves the kernel
comes up, and nothing else. Said only as `SUCCESS`, it becomes a dashboard of
reassuring green that stops people from looking, which is the most dangerous
way for a backup-verification tool to fail.

| Level | What is established | Where it comes from |
| --- | --- | --- |
| `NONE` | nothing: the drill never ran code inside the guest | `startup.skip`, an agent that never answered, a run that ended before its checks |
| `BOOT` | the OS started and executes code | the guest agent answered, or a trivial in-guest command passed |
| `SERVICE` | a service started with its real configuration and its real data | any non-trivial check that passed: a `command`, or a `tcp`/`http`/`dns` probe |
| `DATA` | the data is there and coherent | a passing check declaring `proves: data` in the plan |

The ladder is ordered by **what is proven, not by what it costs to arrange**.
A passing `tcp:5432` proves a service listens, no more and no less, and it does
not outrank a query against the data because it happens to need a route into
the recovery network.

A run's level is the maximum over the checks that **actually passed**. One that
failed, errored or never ran establishes nothing, and contributes nothing in
either direction: the same reasoning as `INCONCLUSIVE`, applied upwards.

**Powering on is not booting.** The hypervisor reports a running process, which
a guest stuck at its boot loader also has; so `BOOT` is established by the guest
agent answering, which happens *inside* the guest, or by an in-guest check
passing. Never by power state alone.

Where a single check's level comes from is a plan question, documented in
[recovery-plans.md](recovery-plans.md#proves): declared with `proves:`, or
deduced by a rule that only ever understates. `DATA` is never deduced: no
amount of reading a command line tells you it looked at a row.

### The ceiling on the confidence score

The level of the newest run that reached a verdict is a **ceiling** on the
workload's Recovery Confidence: `NONE` → 40, `BOOT` → 60, `SERVICE` → 85,
`DATA` → no ceiling. It is applied last, after every penalty, and it enters
`reasons` when it bites: `only the boot was verified (capped at 60)`.

A ceiling rather than another penalty, because the statement is not "this drill
went worse" but "the score cannot exceed what was proven". Penalties accumulate
and can be tuned away; a drill that only ran `hostname` must not be able to
display 100 however well it went. The three values are fields on
`report.ConfidenceWeights` (`ProofNoneCap`, `ProofBootCap`, `ProofServiceCap`)
like every other weight, and a cap of zero or less means no cap.

A run recorded before the level existed carries **no level at all**, and that
means "not recorded", not "nothing was proven". It caps nothing, and the score
walks past it to the newest run that did record one. There is no backfill:
inventing a level for a run that predates the feature, even a cautious one,
would be the mirror image of the lie this exists to correct.

## Retries

Only errors explicitly marked retryable by a provider (`core.Retryable`) are
retried: 5xx responses, connection resets, timeouts, a guest agent that is not
up yet. Deliberately **not** retried: `Restore` (not idempotent: a retry would
leave a half-created workload), `Delete` (destructive), and anything indicating
corruption or a failed integrity check. That distinction lives in the provider
transport, so the engine never has to know what a Proxmox 596 means.

## Concurrency

The CLI runs one drill at a time. A worker runs `limits.max_concurrent_restores`
at a time, which defaults to 1. The failure mode that actually matters is a
recovery drill saturating the cluster it is supposed to protect, so the
default is the cautious one and raising it is a decision an operator makes
about their own cluster. Per-provider and per-node limits are still a design
point, not yet built.

Two workers on the same database never execute the same run. That is not a
convention, it is the `WHERE` clause of the claim: a run whose `lease_owner`
is set cannot be claimed again, by anyone, ever. The claim is the one query in
the project written differently for SQLite and for PostgreSQL (the engines
genuinely differ on how a row is locked and returned in a single statement),
and the queue conformance suite runs the concurrent-claim test against both,
because a claim proven on one engine says nothing about the other.

A lease expires if a worker stops renewing it. Reconciliation then settles
that run as `FAILED` and cleans up after it; it never re-runs it. See
[Interrupted runs](#interrupted-runs) below.

## Interrupted runs

A worker that dies mid-drill (crash, `kill -9`, power cut) leaves a claimed
run in a non-terminal state with a lease that stops being renewed. The next
worker to reconcile the queue finds it, marks it `FAILED`, destroys the
temporary workload the run row recorded, and releases the lease. If that
cleanup fails, the run settles as `CLEANUP_FAILED` with the node and VMID in
the error, because a silent orphan is worse than a loud one.

**Nothing is ever replayed.** A drill restores a backup onto a freshly
allocated temporary id; running one twice allocates a second id, restores a
second time, and orphans the first workload. There is no retry queue, and its
absence is the design. The one case reconciliation deliberately skips is a run
this same process is currently executing: a frozen program (a suspended
laptop, a stalled database) can let its own lease look expired, and settling
it would destroy the temporary workload of a live restore.

## Testing strategy

- **No real cluster is required.** Provider packages are tested against an
  in-process mock Proxmox/PBS API (`httptest`) that records requests, so
  assertions can be made about the exact parameters sent, including that
  `force` is never sent and that delete refuses unmanaged workloads.
- The engine is tested against an in-memory fake provider with injectable
  failures, a fake clock and a fake sleep: the whole suite runs in
  milliseconds and covers every failure path, including cleanup after
  cancellation and after a panic.
- An optional end-to-end suite against a real Proxmox cluster comes later,
  behind a build tag. It will never be required to contribute.

## Roadmap

| Version | Content |
| --- | --- |
| v0.1 | Proxmox VE + PBS, QEMU VMs, CLI drill, isolated restore, ping/tcp/http checks, cleanup, text/JSON/HTML report |
| v0.2 | **The web interface**: watching drills live, launching and cancelling them, editing the plan catalogue, and a first-run setup that replaces the install commands. Shipped alongside it, earlier than this list planned: **scheduled drills** and **the proof level** |
| v0.3 | SSH / PostgreSQL / MySQL checks, Discord & Slack alerts |
| v0.4 | Multi-workload plans, dependencies, restore ordering, parallel restores |
| v0.5 | Remote probes, RBAC, OIDC, PDF reports |
| v0.6 | LXC, multi-cluster, multiple PBS, capacity checks |

The interface moved to the front of that list, from the v1.0 it used to sit
in, because it is not a convenience layer over this tool. It is how most
people will ever use it. A recovery drill is worth running by an operations
team, not only by whoever is comfortable in a terminal, and every command
that stands between someone and their first drill is a reason they never run
one. The CLI keeps every capability: it is what automation drives, and it is
the only place that touches the master key.

Delivered ahead of that order: persistence (SQLite and PostgreSQL), the HTTP
API, the queue and worker behind its write paths, stored recovery plans,
scheduled drills, the confidence score with the proof level that caps it, and
the dashboard's server half (a session cookie, the static handler that serves
the compiled interface, and plan validation for its editor). The confidence
score and any dashboard need a history to read before anything else can be
built on them, and a dashboard that can only watch drills it cannot start is
half a product.

The interface is being built in five slices: **C1** the server half (done),
**C2** the read-only interface (done), **C3** the write paths (done),
**C3b** the plan catalogue (done), **C4** the first-run setup (done).

C2 is what a browser first showed: an overview that says whether anything
needs attention, the drill history, a drill's phases filling in live over the
event stream, the workload inventory with its confidence scores, and the
cluster diagnostic.

C3 gave those screens their verbs (start a drill, cancel one in flight,
destroy what a drill left behind), and C3b added the catalogue, where a plan
is written in the browser with the binary validating each document as it is
typed. Neither added a single HTTP route: everything they drive existed
already, and the interface only had to grow the buttons.

C3b was split out of C3 rather than planned from the start. An editor with
live validation, versioned saves and a conflict path weighs about as much as
the three buttons put together, and the day-one journey is unblocked by the
buttons alone.

What C3 did add is the guard that stops the two halves drifting: the Go tests
capture the real body of every route the dashboard reads, and the TypeScript
types are checked against those captures. It is one-directional (it catches a
key the server renamed or dropped, not one it added), and that limit is
written where somebody will read it.

The compiled interface is embedded in the binary, but its absence is not a
failure: a build made without the front-end toolchain still runs, and `/`
explains itself rather than 404ing. That is what lets `go build ./...` work
with no Node installed.

C4 turned installation into one command. With no configuration, `serve`
starts anyway and prints a URL carrying a single-use setup token; the browser
collects the cluster, the administrator's password and the storage, and the
server does in one call what `init`, `connect`, `network create` and
`token create` did in five.

That endpoint accepts a Proxmox administrator's password, so four things hold
it in place: the token is spent on first use whatever the outcome, the
request is refused in clear off loopback by the same function `POST /session`
uses, the routes are not mounted at all once a cluster is connected, and the
master key stays behind the interface the CLI implements. `internal/api`
imports neither `crypto` nor `internal/providers`, and a test now reads the
package's own imports and fails if either appears.

The setup server does not become the real one. A `Server`'s dependencies are
built once in `api.New` and never rewritten, which is what makes it simple to
reason about; making them mutable for something that happens once in the life
of an installation would be a permanent cost for a momentary convenience. So
`serve` hands the port over instead: the wizard's success closes a channel,
the setup server is torn down, and the configured one opens on the same
address. The browser never reloads: it holds the API token it was handed,
polls until the new server answers, and exchanges it for a session.

Two things C4 found are worth keeping. Provisioning writes the configuration
and the master key *before* it logs in to Proxmox, because a token must have
somewhere to be sealed; a wrong password therefore leaves a configuration with
no providers, and treating that as "configured" locked somebody out of the
only screen that could fix their typo. "Configured" means there is a provider,
not that there is a file. And `connect` and the wizard share one provisioning
sequence rather than two: the order in it is load-bearing (the provider is
stored before the token is verified, because Proxmox reveals a secret exactly
once), and two copies would have drifted on somebody's cluster.

**E1**, the proof level, is delivered on top of all that, and is the first
slice of a direction rather than of the interface: what makes a drill
conclusive. It adds no new kind of check: it classifies the ones that already
exist, records what each run established, and stops a workload whose only check
prints a hostname from displaying a Recovery Confidence of 100. See
[The proof level](#the-proof-level) above.

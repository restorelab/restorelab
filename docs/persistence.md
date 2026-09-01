# Drill history

RestoreLab records every drill it runs, so you can answer the questions that
only make sense over time:

- Is this workload's RTO degrading?
- When was it last validated?
- Has this check been failing for the first time, or for three weeks?

It needs no setup. The first drill creates `~/.restorelab/history.db` and
`restorelab runs list` works from there.

```
$ restorelab runs list
ID         STARTED            WORKLOAD           RESULT    RTO     CLEANUP
94bce70d   2026-09-01 04:44   linux-test (110)   SUCCESS   33.0s   done
f79c8575   2026-09-01 04:42   linux-test (110)   SUCCESS   28.9s   done

$ restorelab runs show 94bce70d
```

`runs show` accepts a shortened id, the way git accepts a short sha, and
renders exactly what the live drill printed — it is the same renderer.

## A broken history never fails a drill

**This is the rule the whole design is built around.** A drill is a
destructive operation on a production cluster, guarded by seven invariants
(see [security.md](security.md)). A locked database, a full disk or a corrupt
file must never abort one, because an aborted drill can leave a temporary
workload alive on the cluster.

So every write is best-effort. If the history cannot be opened or written,
RestoreLab says so once and carries on recording nothing:

```
! drill history is not being recorded: store: open ~/.restorelab/history.db: unable to open database file (14)
```

The drill that follows is identical — same steps, same RTO, same verdict, same
cleanup. The journal does not command the operation.

A test in `internal/cli` drives a whole drill through a store that fails every
single call and asserts nothing changes. The recorder's methods return no
error at all, so that guarantee is enforced by the compiler as much as by the
test.

## Two engines

| | SQLite | PostgreSQL |
| --- | --- | --- |
| When | the default, always | a shared or server deployment |
| Setup | none | create a database, `restorelab db migrate` |
| Migrations | applied automatically, after a backup copy | explicit only |
| Configured by | nothing | `database.url` |

SQLite is embedded through `modernc.org/sqlite`, which is pure Go: the binary
stays static and cross-compilable. The cgo-based driver would have broken
that, which ruled it out.

The asymmetry on migrations is deliberate. The SQLite file belongs to
RestoreLab, so making the operator run `db migrate` after each upgrade would
be friction, and it is exactly the command one forgets until a run fails. A
PostgreSQL database is shared and may serve several instances; migrating
someone else's schema as a side effect of running a CLI would be rude, so a
schema that is behind makes RestoreLab refuse to write and name the command
that fixes it.

Before applying a migration to a SQLite file that already holds history, the
file is copied to `history.db.bak`. A fresh database has nothing to lose and
gets no backup — a `.bak` appearing on the day of install would be alarming
for nothing.

### Keeping the two honest

Two engines are a standing risk of divergence. It is handled by construction,
not by discipline:

1. **One query set.** Every statement is written once, in the SQL subset both
   engines accept, and executed through `database/sql`. A twelve-line shim
   renumbers `?` into `$1` for PostgreSQL. One query set cannot drift from
   itself.
2. **One conformance suite**, written against the `store.Store` interface and
   run against both engines. Because SQLite is embedded, it runs on every
   `go test ./...` with nothing installed; PostgreSQL joins in as soon as
   `RESTORELAB_TEST_DATABASE_URL` is set.
3. **Paired migrations.** Every migration exists in both dialects under the
   same number, and a test fails if one side is missing.

When a test passes on one engine and fails on the other, the fix is to correct
the shared query — never to write a second one.

## Schema

Five tables. `schema_migrations` tracks what has been applied.

**`runs`** — one row per drill. Beyond the obvious fields it carries
`plan_snapshot`: the plan **in full**, as it was when the drill started.

That copy is not a convenience. Plans become editable once they live in the
database, and a run that only referenced its plan would let a report from
March describe checks that were never performed. The history would lie, on a
tool whose entire value is that its journal can be trusted.

**`run_steps`** and **`run_checks`** — the timeline and the verdicts, keyed by
`(run_id, seq)` and upserted at that position. A step is written twice, once
running and once settled, and the second write replaces the first.

**`run_events`** — the progress stream, exactly as the engine emitted it.
`seq` is assigned by the caller rather than by a database sequence: the order
must be emission order, not the order writes happened to land — two things
that diverge the moment a write is retried. The REST API's SSE endpoint
replays a reconnecting browser from this table.

### Conventions, if you read the database by hand

SQLite has no `uuid`, `timestamptz`, `jsonb` or boolean, so one set of
conventions serves both engines and the translation is hidden in the store.

| Concept | Column | Convention |
| --- | --- | --- |
| Identifier | `text` | UUID, canonical form |
| Timestamp | `text` | RFC 3339, **UTC**, all nine nanosecond digits |
| JSON | `text` | compact JSON, `NULL` when absent |
| Boolean | `integer` | 0 / 1 |
| Duration | `integer` | milliseconds |

The timestamp format earns its own note. It is written at **fixed width**, so
`ORDER BY started_at DESC` — a text comparison — is a chronological sort.
Go's `time.RFC3339Nano` trims trailing zeros, which would sort `…:05.1Z` after
`…:05.05Z` even though it is earlier: the order of the whole history, wrong.
Forcing UTC removes the other ambiguity, since a column of mixed offsets
cannot be meaningfully ordered at all. Local time is a rendering concern.

Durations are milliseconds. An RTO is a number of seconds and the report
renders milliseconds, so finer resolution would be digits nobody reads.

## Commands

```
restorelab runs list       history; --workload --state --result --since --limit
restorelab runs show <id>  one drill in full; the id may be shortened
restorelab db status       which database is in use, and whether it is reachable
restorelab db migrate      apply pending migrations (PostgreSQL, mostly)
```

`--since` takes `30d`, `12h`, or `2026-08-01`. Anything it cannot read is
refused rather than guessed at: silently listing the wrong window is worse
than an error.

## Backing it up

The SQLite file is the only thing here that exists nowhere else — the cluster
can be re-drilled, the history cannot be reconstructed. It lives beside the
config and the master key:

```
~/.restorelab/
    config.yaml     providers, sealed secrets
    master.key      the key that unseals them
    history.db      drill history
```

Copy it like any other file; WAL mode means `history.db-wal` and
`history.db-shm` may exist alongside it, so copy the three together, or stop
RestoreLab first.

## What is not recorded

RestoreLab records what its drills did. It does not record the cluster's
state, the contents of a backup, or anything about the production workload
beyond its id and name. Nothing secret reaches this database: provider tokens
stay sealed in `config.yaml`, and a PostgreSQL URL's password is never
printed back, not in an error, not in `doctor`.

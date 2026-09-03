# Scheduling drills

A drill you have to remember to launch is not a verification, it is a reminder.

RestoreLab penalises a stale backup in its confidence score — a reproach it can
only justify if something in the product takes care of testing regularly. That
something is the scheduler: a plan carrying a `schedule` is drilled at the
stated time, with nobody at the keyboard.

```yaml
name: postgres-prod
workload:
  id: "101"
schedule: "0 3 * * 0"          # every Sunday at 03:00
schedule_timezone: Europe/Paris # optional; the server's zone by default
checks:
  - type: command
    run: psql -U postgres -d production -tAc "select count(*) from orders"
    stdout_matches: '^[1-9][0-9]*$'
```

Store it, and that is all there is to do:

```bash
restorelab plan apply postgres-prod.yaml
restorelab schedule list
```

## What the scheduler does, and what it does not

The scheduler **queues** drills. It does not run them.

It reads the catalogue, works out which cron slots are due, and writes one
queue row per slot. The worker — the same one that already executes drills
triggered from the API or the dashboard — picks them up and does the work, with
every guard it already carries. The scheduler holds no provider credential, no
recovery engine and no path to your cluster; it cannot be built with one.

This matters for a practical reason: **automating drills added no new
destructive surface to the product.** A scheduled drill and a drill you click
in the dashboard are the same drill, taking the same path, with the same
refusals.

## The cron expression

Standard five-field crontab syntax:

```
┌───────────── minute (0-59)
│ ┌─────────── hour (0-23)
│ │ ┌───────── day of month (1-31)
│ │ │ ┌─────── month (1-12)
│ │ │ │ ┌───── day of week (0-6, Sunday = 0)
│ │ │ │ │
0 3 * * 0
```

| Expression | Meaning |
| --- | --- |
| `0 3 * * 0` | Sundays at 03:00 |
| `0 3 * * *` | every day at 03:00 |
| `0 2 1 * *` | the 1st of each month at 02:00 |
| `30 1 * * 1-5` | weekdays at 01:30 |
| `@weekly` | shorthand for `0 0 * * 0` |

**One caveat worth knowing**, because it is the one everybody gets wrong: when
**both** the day-of-month and the day-of-week fields are restricted, crontab
combines them with **OR**, not AND. So `0 3 13 * 5` means *the 13th of the
month, or any Friday* — not *Friday the 13th*. RestoreLab follows the standard
here rather than what looks intuitive.

### Timezones

`0 3 * * 0` means three in the morning where you are. The expression is
therefore evaluated in **the server's local timezone** by default, and
`schedule_timezone: Europe/Paris` makes it explicit — which is worth doing on a
server whose clock you do not control.

Slots are stored in UTC. Daylight saving is handled the boring way: when a
scheduled hour does not exist (the spring jump), the drill runs at the first
valid instant; when it happens twice (the autumn fall-back), it runs **once**.

## A late slot is skipped, not caught up

Your server is off at 03:00 and comes back at 09:00. The 03:00 slot is due, six
hours late.

**RestoreLab skips it, records that it did, and never runs it.**

That is a deliberate choice, and the reasoning is worth stating: a drill
restores tens of gigabytes and occupies your storage. One that starts in the
middle of a working day, because a server happened to reboot, is an incident —
not a test. Nothing about a backup is learned more usefully at 09:00 than it
would be next Sunday.

The threshold is the **grace period**, two hours by default. A slot late by less
than that runs; past it, the slot is skipped with a reason you can read:

```bash
$ restorelab schedule slots postgres-prod
SLOT                  OUTCOME   DETAIL
2026-09-06 03:00 UTC  skipped   the slot was 6h0m late, past the 2h grace
                                period: a drill that starts during working
                                hours is an incident, not a test
2026-08-30 03:00 UTC  queued    run 4f2a9c31 - SUCCESS in 28.4s
```

The corollary is assumed: **a machine that is switched off every night will
never be drilled**, and the product says so plainly rather than forcing a drill
at noon. If that describes your situation, pick a slot when the machine is up.

## When several plans are due at once

Twelve plans that all say `0 3 * * 0`, with a worker running one drill at a
time, would mean a twelfth drill starting at 11:00. Two things prevent that:

- **The queue depth.** The scheduler stops queueing when the queue is already
  `max_queue_depth` deep (5 by default) and tries again at the next tick. A
  full queue is a postponement, not a decision — nothing is written, and no
  slot is burned.
- **The grace period** then settles the ones that will never get their turn:
  they are skipped, with their reason, and next week's slot is unaffected.

If you have more plans than nights, spread them across the week rather than
raising the queue depth. `0 3 * * 1` through `0 3 * * 5` tells you exactly what
runs when, which is worth more than a queue that eventually drains.

## Why a slot cannot run twice

A drill is not idempotent. Running one twice allocates a second temporary VMID,
restores a second time, and can leave the first clone behind on the cluster. A
scheduler that double-queues would break the invariant the whole product rests
on.

So a slot is **a row in the database**, keyed by the plan and the exact instant
the cron designated, written in the same transaction as the run it queues. The
database is what refuses a duplicate — not a lock, not a leader election, not
an in-memory flag. Consequences:

- a process that dies at any point between the two writes cannot double-queue;
- two `restorelab serve` instances can run against the same database, and the
  second one's claim is simply refused;
- every scheduled run can say which slot it belongs to, and every skipped slot
  can say why it was skipped.

## Turning it off

```yaml
scheduler:
  enabled: false        # nothing is scheduled; plans keep their cron
  grace_period: 2h      # past this, a late slot is skipped
  max_queue_depth: 5    # the scheduler stops queueing beyond this
```

Or for one run of the server:

```bash
restorelab serve --no-scheduler
```

Use the global switch for what it is good at — "we are migrating the cluster
tonight, stop everything". To stop scheduling **one** plan, remove its
`schedule` field, in the dashboard's plan editor or with `plan apply`. There is
deliberately no third way: two mechanisms for the same thing would mean two
places to look when a drill did not run.

## No database, no scheduler

RestoreLab works without a database — a drill runs and reports, history is
simply not recorded. **Scheduling is the one feature that genuinely requires
one**, and it says so rather than pretending.

The reason is the slot table: with nowhere to record that a slot was decided, a
scheduler would re-decide the same slot at every tick and queue a drill every
minute. SQLite is the default and needs no installation
(`~/.restorelab/history.db`), so in practice this only affects an installation
whose database is broken — where the warning is the point.

## Reading what the scheduler did

```bash
restorelab schedule list            # scheduled plans and their next slot
restorelab schedule slots [plan]    # slot history, skipped ones included
```

The dashboard shows the same two things: a **next drill** column on the plan
list, and a workload's recently skipped slots on its detail page. A slot that
was skipped is information, not a non-event — a machine reading "never
tested" with no explanation is the failure mode this product exists to avoid.

## See also

- [recovery-plans.md](recovery-plans.md) — the plan format, and the `schedule`
  field in the full reference
- [persistence.md](persistence.md) — where slots are stored
- [network-isolation.md](network-isolation.md) — why a scheduled drill is still
  safe to run unattended

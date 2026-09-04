# Notifications

A drill that runs at 03:00 with nobody watching writes its verdict to a
database nobody is watching either. The scheduler removed the need to remember
to launch a drill; it did not give the result anywhere to go.

RestoreLab posts to Discord, Slack or a webhook of your own **when what it
proves about a workload changes**. Not on every run.

```yaml
# ~/.restorelab/config.yaml
notifications:
  - id: ops
    kind: discord
    url: rlsec:v1:...      # sealed, see below
```

```bash
restorelab notify add ops --kind discord --url-file -
restorelab notify test ops
```

Run the test. A channel nobody has ever seen fire is a channel nobody should
trust, and this one command is the difference between believing you are
covered and knowing it.

## When it speaks

A workload's story is the pair **(verdict, proof level)** of its last drill
that reached a verdict. A finished drill produces a message when, against that
story:

| What happened | Why you hear about it |
| --- | --- |
| the verdict changed, either way | `SUCCESS` to `FAILED` is the alert you expect. `FAILED` to `SUCCESS` is the one that tells you to stop worrying, and it is worth as much |
| the proof level dropped, at an unchanged verdict | a workload still green that fell from `DATA` to `SERVICE` has regressed. Nothing else would tell you |
| it was the workload's first verdict | nothing to compare against, but the baseline is now set |
| the workload stopped being evaluable | the first drill that reached no verdict after one that did. A workload whose drills cannot be evaluated is not being verified at all |
| it became evaluable again | whatever the verdict is. Being able to see the workload again is the news |
| a captured value fell to zero | a workload that restores, boots and answers, holding an empty database. Only zero from a non-zero baseline: see below |

## When it stays silent

This is the more important half, and it is deliberate.

**A green run that follows a green run produces nothing.** A scheduler drilling
twenty workloads every night would otherwise post twenty messages a night. The
channel gets muted inside a week, and the red message on the following Tuesday
is never read. An alerting path that is ignored is not an alerting path, so
RestoreLab treats your attention as the scarce resource it is.

**A cancelled drill produces nothing.** Somebody stopped it, and that somebody
already knows.

**Consecutive unevaluable drills produce one message, not one per night.** Only
the edge into that state is news.

**A run that predates this feature produces nothing.** Upgrading does not
replay your history into your chat channel.

**Nothing older than a day produces anything.** If RestoreLab has been
drilling from the CLI with no server, or running with `--no-notify`, there is
a pile of runs nobody has been told about. Configuring your first channel does
not empty that pile into it. Those runs are marked as considered and passed
over: a drill from last month is not an alert, it is archaeology, and the
dashboard is where that belongs. A dispatcher that was down overnight still
catches up on everything that mattered.

### Why only zero

A [captured value](recovery-plans.md#capture-assert-and-drift) that trips a
bound you declared already fails its check, which changes the verdict, which
is announced by the first row of that table. A second message would say the
same thing twice.

What is left is drift nobody declared a tolerance for, and there RestoreLab
has no agreed threshold. Picking one, and deciding on your behalf that losing
a fifth of a table is worth waking you up, is how a channel starts crying
wolf. Zero from a non-zero baseline needs no agreed threshold: a database that
had 1 204 331 rows last night and has none tonight is not a judgement call.

A value that merely halved, with no tolerance declared, produces nothing. Say
what you consider wrong with `drift.max_drop` and it becomes a verdict, which
is announced like any other.

## The message

It says what changed, not just what happened:

```
web-01: FAILED, was SUCCESS on 2026-09-03
```

```
postgres-prod: proof dropped from DATA to SERVICE
```

Set `server.base_url` and each message carries a link to the run in the
dashboard:

```yaml
server:
  base_url: https://restorelab.internal
```

Without it the message is the same, just without the link.

## The three kinds

| `kind` | What it posts to |
| --- | --- |
| `discord` | a Discord webhook URL, as a coloured embed |
| `slack` | a Slack incoming webhook, as Block Kit |
| `webhook` | anything of yours, as JSON |

The generic payload carries `"schema": "restorelab.notification.v1"`. That
string and the transition names in it are a stable interface: a filter you
write today keeps working.

## The URL is a credential

Anyone holding a Discord or Slack webhook URL can post into that channel. There
is no second factor and no way to tell a legitimate post from any other. So
RestoreLab treats it exactly as it treats a Proxmox API token:

- it is **sealed with the master key** before it touches disk, and appears in
  `config.yaml` as `rlsec:v1:...`;
- RestoreLab refuses to write a configuration file that would put one there in
  plaintext;
- it never appears in a log line, an error message, a diagnostic, or any API
  response. `restorelab notify list` shows the host, never the path, because on
  a Discord webhook the path **is** the secret;
- the dashboard can set it and never reads it back.

A backup of `config.yaml` without the master key is therefore useless to
whoever finds it, and a copy of both is a copy of your credentials. Same rule
as everywhere else in this product: see [security.md](security.md).

## When delivery fails

Four attempts, at 0s, 30s, 2min and 8min.

RestoreLab retries a 5xx, a 429 and a transport error, because asking again can
plausibly produce a different answer. It does **not** retry any other 4xx: a
revoked webhook or a deleted channel will not start working because we asked
twice, and retrying only delays the moment you find out the path is broken.

After the last attempt the delivery is recorded as failed, with the HTTP status
and the reason. `restorelab doctor` reports it, and so does
`GET /api/v1/doctor`. A channel that quietly stopped working is the exact
failure this feature exists to prevent, so it is not allowed to fail quietly
either.

One accepted risk, stated rather than hidden: **a timeout on a request that
actually arrived produces a duplicate message.** From here a slow success and a
lost request look identical. A duplicate is the better failure: you notice it
and ignore it, while a silence is neither noticed nor ignored.

## What it does not do

- **No email, no SMS, no PagerDuty.** Route a `webhook` into whatever you
  already run.
- **No digest.** Every enabled channel gets the transitions as they happen.
- **No routing by plan or workload.** Every enabled channel receives every
  transition. Two teams wanting two different channels is a real need and a
  later addition; fixing a routing model before anybody has used the simple one
  would be guessing.
- **It cannot touch your cluster.** The dispatcher holds no provider and no
  recovery engine, and cannot be constructed with one. Adding notifications
  added no destructive surface to this product, and that is enforced by the
  compiler rather than promised in this paragraph. Same guarantee the scheduler
  carries, for the same reason.

## Turning it off

```bash
restorelab serve --no-notify
```

Configured channels stay configured and nothing is sent. `doctor` says so, so
that a quiet channel is never mistaken for a quiet fleet.

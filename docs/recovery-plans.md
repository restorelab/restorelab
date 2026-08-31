# Recovery plans

A recovery plan is a YAML file describing **what to restore, where to restore
it, and what must be true afterwards** for the recovery to count as proven.

```bash
restorelab recovery run examples/plans/postgres-prod.yaml
```

Everything except `name` and `workload` has a safe default. Unknown fields are
rejected, so a typo fails the plan instead of silently changing its meaning.

## Full reference

```yaml
name: postgres-prod                 # required, identifies the plan and its runs
description: Nightly database drill # optional
tags: [production, database]        # optional, free-form

workload:                           # required
  provider: proxmox-main            # provider id from the config
  id: "101"                         # VMID, quoted so YAML keeps it a string
  name: postgres-prod               # optional, cosmetic

backup:
  provider: pbs-main                # default: the workload's provider
  strategy: latest                  # latest | specific
  id: "pbs-main:backup/vm/101/..."  # required when strategy is "specific"
  max_age: 26h                      # fail the run if the newest backup is older

restore:
  node: pve02                       # default: the provider's default node
  storage: local-lvm                # default: the provider's default storage
  network: isolated                 # a network profile name from the config
  bridge: vmbr99                    # overrides the profile's bridge
  cpu_limit: 2                      # cores given to the temporary workload
  memory_limit: 4096                # MiB given to the temporary workload
  bandwidth_limit: 200000           # restore throughput cap, KiB/s
  timeout: 60m                      # how long the restore may take

startup:
  skip: false                       # true: restore only, never boot the guest
  timeout: 180s                     # how long to wait for the guest
  wait_for_ip: true                 # require an address before running checks
  wait_for_agent: true              # require the guest agent to answer
  ip: 10.99.0.14                    # pin the address instead of discovering it

checks:                             # see below
  - type: tcp
    port: 22

cleanup:
  always: true                      # destroy the temporary workload, even on failure
  keep_on_failure: false            # keep it for debugging after a failed run

rto_target: 5m                      # the run is graded against this
schedule: "0 3 * * 0"               # consumed by the scheduler, ignored by the CLI
```

### `backup.max_age`

The most underrated field in the file. A drill that restores a three-week-old
backup and passes every check is not good news. Set `max_age` slightly above
your backup interval (`26h` for a nightly job) and a stale backup becomes a
loud, immediate failure instead of a detail buried in a report.

### `restore.network`

Names a network profile from the configuration. `isolated` is the default and
must stay so: see [network-isolation.md](network-isolation.md) for why booting a
restored production clone on a production network is an incident, not a test.

### `startup.wait_for_ip` and `startup.wait_for_agent`

Both default to what the checks actually need, so most plans never set them:

| Plan contains | Waits for an address | Waits for the agent |
| --- | --- | --- |
| network checks (`tcp`, `http`, `ping`, `dns`) | yes | no |
| only in-guest checks (`command`) | **no** | yes |
| both | yes | yes |
| no checks at all | yes | no |

That default is what makes an in-guest drill work with no DHCP on the isolated
bridge: there is no address to wait for, so the run waits for the guest agent
instead. An explicit value in the plan always wins over the default.

### `startup.skip`

Restore-only drill: the backup is restored and the workload created, but never
booted. Useful for workloads you cannot safely start even in isolation, and as
a first onboarding step. Checks are not allowed in a plan that skips startup.

### `cleanup`

`always: true` is the default and should stay that way. `keep_on_failure: true`
preserves the temporary workload after a failed run so you can open its console
— remember to destroy it afterwards (`restorelab cleanup <vmid>`), because
RestoreLab will not do it for you on the next run.

### `rto_target`

The recovery time objective the run is graded against. RTO is measured from the
start of the run to the end of the checks; cleanup and report generation are
excluded, because they are RestoreLab's housekeeping and not part of the
recovery a business would experience. Exceeding the target does not fail the
run, it downgrades it to `DEGRADED`.

## Checks

Every check shares these fields:

| Field | Default | Meaning |
| --- | --- | --- |
| `type` | required | Check type (below) |
| `name` | derived | Name shown in reports (`TCP 22`) |
| `timeout` | `30s` | Per-attempt deadline |
| `retries` | `0` | Extra attempts after the first |
| `retry_interval` | `5s` | Wait between attempts |
| `critical` | `true` | A failure fails the run; `false` downgrades it to `DEGRADED` |

A freshly booted service is rarely ready on the first attempt. `retries: 10`
with `retry_interval: 6s` is a better answer than a single long timeout,
because the check passes as soon as the service is up — which is also what makes
the measured RTO honest.

String parameters support templates against the target:
`{{ .ip }}`, `{{ .id }}`, `{{ .node }}`, `{{ .name }}`. An unknown variable is
an error, never a silently empty string.

### `ping`

Is the guest answering ICMP at all?

| Parameter | Default | Meaning |
| --- | --- | --- |
| `host` | the target IP | Host to ping |
| `count` | `3` | Echo requests to send |
| `interval` | `500ms` | Delay between requests |
| `privileged` | `false` | Raw ICMP instead of unprivileged UDP ping |

Reports `packets_sent`, `packets_recv`, `avg_rtt_ms`, `packet_loss`. ICMP is
often filtered — a failing ping check with a passing TCP check usually means a
firewall, not a broken recovery. If the OS refuses the socket, the check reports
an *error* (not a failure) and points you at `privileged`.

### `tcp`

The workhorse: is the port actually open?

| Parameter | Default | Meaning |
| --- | --- | --- |
| `host` | the target IP | Host to connect to |
| `port` | required | TCP port |
| `expect_banner` | — | Substring the server must send on connect |

```yaml
- type: tcp
  name: PostgreSQL
  port: 5432
  retries: 5
  retry_interval: 5s
```

### `http` / `https`

Does the application answer, and answer correctly?

| Parameter | Default | Meaning |
| --- | --- | --- |
| `url` | required | Target URL, template-expanded |
| `method` | `GET` | HTTP method |
| `expected_status` | `200` | Expected status code |
| `expected_statuses` | — | List of acceptable codes; wins over `expected_status` |
| `headers` | — | Request headers (values template-expanded) |
| `body` | — | Request body |
| `body_contains` | — | Substring the response must contain |
| `body_matches` | — | Regular expression the response must match |
| `json_path` | — | Dotted path into the JSON response (`status.database`, `items.0.name`) |
| `json_equals` | — | Value that path must equal |
| `insecure_tls` | `false` | Skip certificate verification |
| `follow_redirects` | `true` | Follow 3xx |
| `max_body_bytes` | `1 MiB` | Response bytes read before truncating |

```yaml
- type: http
  name: health endpoint
  url: http://{{ .ip }}:8080/health
  expected_status: 200
  json_path: status
  json_equals: ok
  retries: 10
  retry_interval: 6s
```

Reports `status_code`, `latency_ms`, `body_size` and a truncated `body_snippet`.

### `command`

Runs a command **inside** the restored guest through the hypervisor's guest
agent. It travels over the Proxmox API, so it needs no network path into the
recovery network at all — no route, no DHCP on the isolated bridge, no runner
beside it. It also sees things a network probe never could: a systemd unit's
real state, a file's contents, a local socket, a database query over a Unix
socket.

| Parameter | Default | Meaning |
| --- | --- | --- |
| `run` | — | Command line, executed through a shell in the guest |
| `argv` | — | List of strings, executed directly with no shell |
| `shell` | `/bin/sh` | Interpreter for `run`: a path, or `sh`, `bash`, `cmd`, `powershell` |
| `expect` | — | Trimmed stdout must equal this exactly |
| `stdout_contains` | — | Substring stdout must contain |
| `stdout_matches` | — | Regular expression stdout must match |
| `expect_exit_code` | `0` | Required exit code |
| `input` | — | Written to the command's standard input |
| `max_output_bytes` | `65536` | Cap on captured output |

Exactly one of `run` and `argv` is required. Assertions are evaluated in
order: exit code, `expect`, `stdout_contains`, `stdout_matches`.

```yaml
- type: command
  name: PostgreSQL
  run: systemctl is-active postgresql
  expect: active
  retries: 10
  retry_interval: 6s

- type: command
  name: production database queryable
  run: psql -U postgres -d production -tAc 'select 1'
  expect: "1"
```

Requires `qemu-guest-agent` installed and running in the guest, and the agent
enabled on the VM (*VM → Options → QEMU Guest Agent*) — which is also what
gives you filesystem-consistent backups, so it is worth having anyway.

A command that runs and gives the wrong answer is a **failure**. A guest with
no agent, or an API that refuses the call, is an **error** — the report says
"I could not ask", never "your service is broken". See
[deployment.md](deployment.md) for when to prefer in-guest over network checks.

Reports `exit_code`, `stdout`, `stderr`, `truncated` and the resolved `argv`.

### `dns`

Does the restored resolver answer — or does a name resolve from inside the
recovery network?

| Parameter | Default | Meaning |
| --- | --- | --- |
| `name` | required | Name to resolve |
| `server` | the target IP | DNS server to query |
| `port` | `53` | Server port |
| `type` | `A` | `A`, `AAAA`, `CNAME`, `MX`, `TXT` |
| `expect` | — | Expected answers; passes when **any** answer matches |

## Grading

| Verdict | When |
| --- | --- |
| `SUCCESS` | Every critical check passed and the RTO target was met |
| `DEGRADED` | Recovered, but a non-critical check failed or the RTO target was exceeded |
| `FAILED` | A workflow step failed, or a critical check failed |
| `CLEANUP_FAILED` | The drill finished but the temporary workload could not be destroyed — needs manual attention, with the node and VMID named in the error |

## Writing a good plan

1. **Start with `tcp` on SSH, or a `command` check.** Either proves the guest
   booted and started a service. Prefer `command` when you have no route into
   the recovery network — which is the common case. Add application checks
   after that works.
2. **Check the thing that matters, not the thing that is easy.** "Port 5432 is
   open" is weaker than "the health endpoint reports the database is
   reachable". The whole point of RestoreLab is the difference between the two.
3. **Mark cosmetic checks `critical: false`** so a broken favicon does not turn
   a genuine successful recovery into a red alert.
4. **Set `rto_target` to your actual contractual objective**, not to what you
   currently achieve. A `DEGRADED` verdict on a real target is information; a
   green run against a target you invented is not.
5. **Set `max_age`.** A plan without it will happily prove that a very old
   backup restores beautifully.

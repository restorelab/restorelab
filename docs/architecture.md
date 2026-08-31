# Architecture

RestoreLab is a **modular monolith** in Go. One binary, several roles
(`server`, `worker`, `probe`, plus the CLI), one codebase. Microservices,
Temporal, Kafka and a service mesh are explicit non-goals — the hard part of
this product is being correct about Proxmox and about cleanup, not distributing
itself.

## Dependency direction

```text
        cli / api  ───────────────┐
             │                    │
             ▼                    ▼
        recovery (engine)     report
             │  │  │
   ┌─────────┘  │  └──────────┐
   ▼            ▼             ▼
plan        checks        providers/{proxmox,pbs}
   │            │             │
   └────────────┴─────────────┘
                ▼
              core
```

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
    report              text / JSON / HTML reports, recovery confidence score
    cli                 cobra commands
    version             build metadata
```

Planned, not yet present: `api` (REST + SSE), `store` (PostgreSQL via sqlc),
`jobs` (Asynq workers), `scheduler`, `notifications`, `audit`, `probe`.

## The recovery workflow

The engine is a linear state machine with one guarantee: **from the moment a
temporary workload might exist, cleanup runs — on success, on failure, on
timeout, on cancellation, on panic.**

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
  ↓ run_checks             ping / tcp / http / dns, with retries
RUNNING_CHECKS
  ↓ generate_report
GENERATING_REPORT
  ↓ cleanup                stop + delete, on a detached context
CLEANING_UP
  ↓
SUCCESS | DEGRADED | FAILED | CLEANUP_FAILED
```

**RTO** is measured from the start of the run to the end of the checks. Cleanup
and report generation are excluded: they are RestoreLab's housekeeping, not part
of the recovery a business would experience.

**Grading**: every critical check passed and the RTO target met → `SUCCESS`;
recovered but a non-critical check failed or the RTO target was exceeded →
`DEGRADED`; a step failed or a critical check failed → `FAILED`.

## Retries

Only errors explicitly marked retryable by a provider (`core.Retryable`) are
retried: 5xx responses, connection resets, timeouts, a guest agent that is not
up yet. Deliberately **not** retried: `Restore` (not idempotent — a retry would
leave a half-created workload), `Delete` (destructive), and anything indicating
corruption or a failed integrity check. That distinction lives in the provider
transport, so the engine never has to know what a Proxmox 596 means.

## Concurrency

v0.1 runs one drill at a time from the CLI. The design point for the worker
release is three concurrency limits — global, per provider, per node — because
the failure mode that actually matters is a recovery drill saturating the
cluster it is supposed to protect.

## Testing strategy

- **No real cluster is required.** Provider packages are tested against an
  in-process mock Proxmox/PBS API (`httptest`) that records requests, so
  assertions can be made about the exact parameters sent — including that
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
| v0.2 | Scheduled drills, SSH / PostgreSQL / MySQL checks, Discord & Slack alerts, RTO targets |
| v0.3 | Multi-workload plans, dependencies, restore ordering, parallel restores |
| v0.4 | Remote probes, RBAC, OIDC, PDF reports |
| v0.5 | LXC, multi-cluster, multiple PBS, recovery confidence, capacity checks |
| v1.0 | REST API, PostgreSQL persistence, workers, web dashboard, audit, notifications |

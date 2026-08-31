<h1 align="center">RestoreLab</h1>

<p align="center">
  <strong>Your backups are green. But can you actually recover?</strong>
</p>

<p align="center">
  <a href="LICENSE"><img alt="License: AGPL v3" src="https://img.shields.io/badge/license-AGPL--3.0-blue.svg"></a>
  <img alt="Go" src="https://img.shields.io/badge/go-1.27-00ADD8.svg">
  <img alt="Status" src="https://img.shields.io/badge/status-alpha-orange.svg">
</p>

---

RestoreLab automatically restores your backups into isolated environments, boots
the workloads, validates the services, measures your real recovery time, and
cleans everything up.

```text
Backup verification says:          RestoreLab says:

  ✓ Backup exists                    ✓ VM restored
  ✓ Checksum valid                   ✓ OS booted
                                     ✓ PostgreSQL started
                                     ✓ API returned HTTP 200
                                     ✓ Recovery completed in 2m06
```

A backup that restores is not the same thing as a service that comes back. The
VM boots but PostgreSQL does not start. The database starts but the schema is
inconsistent. The API answers but Redis was never restored. Recovery takes 45
minutes against a 15-minute RTO. **RestoreLab tests the whole chain, on a
schedule, and proves it.**

## What it does

```text
Backup exists → available → restore succeeds → guest boots → OS reachable
    → services start → application responds → dependencies usable → RTO measured
```

Every recovery drill runs against a **temporary workload on an isolated
network**, never against production, and every temporary resource RestoreLab
creates is stamped with ownership metadata so cleanup can never touch anything
it did not create.

## Status

Alpha, under active development. The Proxmox recovery drill pipeline works
end to end behind the CLI; the API, the scheduler and the web dashboard come
after it has been proven against real clusters.

| Area | State |
| --- | --- |
| Proxmox VE provider (restore / harden / start / status / delete) | done |
| Proxmox Backup Server discovery | done |
| Recovery engine (isolation, capacity, cleanup, RTO, grading) | done |
| Checks: ping, tcp, http/https, dns | done |
| CLI (`init`, `provider`, `workloads`, `backups`, `recovery`, `cleanup`) | done |
| Reports: terminal, JSON, self-contained HTML | done |
| Scheduled drills, SSH / PostgreSQL / MySQL checks, notifications | next |
| REST API + PostgreSQL + workers + web dashboard | planned |

## Quick start

> Requires Go 1.27+ until the first binary release.

```bash
go build -o bin/restorelab ./cmd/restorelab

# create ~/.restorelab/config.yaml and a master key
bin/restorelab init

# register your hypervisor and your backup server
bin/restorelab provider add proxmox \
    --id proxmox-main \
    --endpoint https://pve.example.com:8006 \
    --token-id 'restorelab@pve!drills' \
    --token-secret 'xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx'

# see what can be recovery-tested
bin/restorelab workloads list

# run a drill on VM 101, from its latest backup
bin/restorelab recovery test 101
```

```text
[✓] Connected to Proxmox
[✓] VM 101 found
[✓] Backup found
[✓] Restore started
[✓] Temporary VM created
[✓] VM booted
[✓] TCP/22 reachable
[✓] VM removed

Recovery successful
RTO: 2m17s
```

## Recovery plans

A plan describes what to restore, where, and what must be true afterwards.

```yaml
name: postgres-prod

workload:
  provider: proxmox-main
  id: "101"

backup:
  strategy: latest
  max_age: 26h

restore:
  node: pve02
  network: isolated
  cpu_limit: 2
  memory_limit: 4096

startup:
  timeout: 180s

checks:
  - type: tcp
    port: 22

  - type: http
    url: http://{{ .ip }}:8080/health
    expected_status: 200

cleanup:
  always: true

rto_target: 5m
```

```bash
restorelab recovery run examples/plans/postgres-prod.yaml
```

## Safety model

RestoreLab holds credentials that can restore, start and delete workloads. It is
built so that a bug cannot cost you production:

- **Isolated by default** — restores land on a dedicated bridge with no uplink,
  the network configuration inherited from the backup is rewritten, and a run is
  refused when isolation cannot be verified.
- **Never touches production** — every temporary resource is created by
  RestoreLab with `restorelab_managed=true` metadata, and delete refuses any
  workload that does not carry it.
- **Temporary IDs only** — restores go to a reserved VMID range (9000–9999 by
  default), never over an existing workload.
- **Cleanup always runs** — including after a failure, a timeout or a cancelled
  run; a failed cleanup is a loud, named alert, never a silent orphan.
- **No plaintext secrets** — API tokens are sealed with AES-256-GCM under a
  master key that is never stored in the config file.

See [docs/security.md](docs/security.md) and
[docs/proxmox-permissions.md](docs/proxmox-permissions.md) for the minimal
Proxmox permission set — RestoreLab does not want, and should never be given,
global administrator rights.

## Documentation

| Document | Contents |
| --- | --- |
| [docs/configuration.md](docs/configuration.md) | Config file, providers, network profiles, limits |
| [docs/recovery-plans.md](docs/recovery-plans.md) | Plan reference and every check type |
| [docs/network-isolation.md](docs/network-isolation.md) | Building the isolated bridge, why it matters |
| [docs/proxmox-permissions.md](docs/proxmox-permissions.md) | Dedicated service account and least privilege |
| [docs/security.md](docs/security.md) | Threat model, secret handling, audit |
| [docs/architecture.md](docs/architecture.md) | Packages, provider abstraction, roadmap |

## Contributing

Issues and pull requests are welcome. See [CONTRIBUTING.md](CONTRIBUTING.md).
Providers and checks are deliberately behind small interfaces
(`core.HypervisorProvider`, `core.BackupProvider`, `core.Check`) — adding a new
one should not require touching the engine.

## License

[AGPL-3.0](LICENSE). RestoreLab is free to self-host, modify and run. If you
offer it as a network service, the same freedoms must reach your users.

# Configuration

RestoreLab reads a single YAML file, by default `~/.restorelab/config.yaml`.
Override it with `$RESTORELAB_CONFIG` or `--config`.

Create it with:

```bash
restorelab init
```

which also generates the master key used to seal provider secrets.

## Example

```yaml
version: 1

providers:
  - id: proxmox-main
    kind: proxmox
    roles: [hypervisor, backup]
    endpoint: https://pve.example.com:8006
    token_id: restorelab@pve!drills
    token_secret: rlsec:v1:8Kx2...          # sealed, never plaintext
    backup_storage: pbs-main
    pool: restorelab
    temp_id_min: 9000
    temp_id_max: 9999

  - id: pbs-main
    kind: pbs
    roles: [backup]
    endpoint: https://pbs.example.com:8007
    token_id: restorelab@pbs!drills
    token_secret: rlsec:v1:Qz91...
    datastore: main
    pve_storage: pbs-main                    # the storage name this datastore has in PVE
    fingerprint: "AA:BB:CC:...:FF"

networks:
  isolated:
    bridge: vmbr99
    isolated: true
  # A non-isolated profile can exist, but can never be the default and must be
  # named explicitly by a plan.
  staging:
    bridge: vmbr1
    vlan_tag: 42
    firewall: true
    isolated: false

limits:
  max_concurrent_restores: 1
  max_recovery_memory_mb: 16384
  max_recovery_disk_gb: 500

defaults:
  provider: proxmox-main
  backup_provider: pbs-main
  network: isolated
  node: pve02
  storage: local-lvm

scheduler:
  enabled: true
  grace_period: 2h
  max_queue_depth: 5
```

## `providers`

| Field | Applies to | Meaning |
| --- | --- | --- |
| `id` | all | Name plans refer to. Must be unique. |
| `kind` | all | `proxmox` or `pbs`. |
| `roles` | all | `hypervisor` (can restore/start/delete) and/or `backup` (can find restore points). Proxmox VE can be both; PBS is `backup` only. |
| `endpoint` | all | Base URL, including the port (`:8006` for PVE, `:8007` for PBS). |
| `token_id` | all | API token identifier, e.g. `restorelab@pve!drills`. |
| `token_secret` | all | Always stored sealed (`rlsec:v1:…`). Saving an unsealed value is refused. |
| `insecure` | all | Skip TLS verification. Homelab escape hatch — prefer `fingerprint` or `ca_cert_path`. |
| `fingerprint` | pbs | SHA-256 certificate fingerprint to pin. The right answer for a self-signed PBS. |
| `ca_cert_path` | all | PEM file for a private CA. |
| `node` | proxmox | Default node for API calls that need one. |
| `backup_storage` | proxmox | Storage holding backups. When empty, every backup-capable storage is scanned. |
| `pool` | proxmox | Resource pool temporary workloads are created in. Required when the token's destructive rights are scoped to a pool, which is what `restorelab connect` sets up. |
| `temp_id_min` / `temp_id_max` | proxmox | Reserved VMID range for temporary workloads. Default 9000–9999. |
| `datastore` | pbs | PBS datastore name. |
| `pve_storage` | pbs | The name that datastore is attached under in PVE — used to build the restore volid. Defaults to `datastore`. |

The fastest way to create all of this, including the service account itself:

```bash
restorelab connect https://pve.example.com:8006
```

Or add a provider from an existing token:

```bash
restorelab provider add proxmox \
    --id proxmox-main \
    --endpoint https://pve.example.com:8006 \
    --token-id 'restorelab@pve!drills' \
    --token-secret '...'

restorelab provider list
restorelab provider test proxmox-main
```

See [proxmox-permissions.md](proxmox-permissions.md) for creating the token with
minimal rights.

## `networks`

Named network profiles, referenced by `restore.network` in a plan.

| Field | Meaning |
| --- | --- |
| `bridge` | The bridge a restored workload is attached to. |
| `vlan_tag` | Optional VLAN tag. |
| `firewall` | Enable the Proxmox firewall on the interface. |
| `isolated` | Asserts this network has no path to production. |

`isolated: true` is an assertion you make; RestoreLab verifies it against the
node (a bridge with physical ports or a gateway is rejected) and refuses the run
if it cannot. A profile with `isolated: false` cannot be used as
`defaults.network` — production-network restores are opt-in per plan, never a
default. See [network-isolation.md](network-isolation.md).

## `limits`

| Field | Meaning |
| --- | --- |
| `max_concurrent_restores` | Global cap on simultaneous restores. Keep it low: a drill must never starve the cluster it protects. |
| `max_recovery_memory_mb` | Total RAM RestoreLab may allocate to temporary workloads. |
| `max_recovery_disk_gb` | Total disk temporary workloads may consume. |

## `defaults`

Values used when a plan or a CLI flag does not specify them: `provider`,
`backup_provider`, `network`, `node`, `storage`. `defaults.network` must name an
isolated profile.

## `scheduler`

Governs the drills stored plans queue for themselves. Every field has a
working default, so the whole block is absent from most configurations — and
**its absence means scheduling is on**, so an installation that upgrades into
a version with a scheduler starts honouring the schedules its plans already
carry.

| Field | Default | Meaning |
| --- | --- | --- |
| `enabled` | `true` | `false` stops all scheduling. Plans keep their `schedule`; nothing acts on it. |
| `grace_period` | `2h` | How late a slot may be and still run. Past it the slot is skipped and recorded. |
| `max_queue_depth` | `5` | The scheduler stops queueing beyond this depth and tries again at the next tick. |

`grace_period` is the field worth thinking about. A drill restores tens of
gigabytes onto production storage; one that starts hours outside its window,
because a server happened to reboot, is an incident rather than a test. Raising
this to a day means a machine that was off overnight gets drilled at lunchtime.

Only a process that runs the worker schedules anything — a process that queued
scheduled drills without draining them would fill a queue nobody empties.
`restorelab serve --no-scheduler` turns it off for one run of the server,
which is the switch for a night of cluster maintenance.

See [scheduling.md](scheduling.md) for what the scheduler does with all this.

## `database` — drill history

RestoreLab records every drill so you can see whether an RTO is degrading,
when a workload was last validated, and whether a check has been failing for
weeks.

**It needs no setup.** The first drill creates `~/.restorelab/history.db`, an
embedded SQLite file, and `restorelab runs list` works from there. There is no
service to install and nothing to configure.

To keep the history in PostgreSQL instead — for a shared or server deployment
— point `database.url` at it and run `restorelab db migrate` once:

```yaml
database:
  url: postgres://restorelab@db.internal:5432/restorelab
```

The scheme picks the engine. `sqlite:///path/to/history.db` puts the embedded
database somewhere other than the default.

PostgreSQL migrations are never applied automatically: a shared database may
serve several instances, and migrating someone else's schema as a side effect
of running a command would be rude. The SQLite file belongs to RestoreLab, so
it migrates itself — after copying the file aside first.

A PostgreSQL URL may carry a password. It is treated as a secret: never
printed back, never in an error, never in `doctor`.

**A broken history never fails a drill.** Database unreachable, schema behind,
file corrupt — RestoreLab says so once and carries on without recording. The
journal does not command the operation.

## The master key

Provider secrets are sealed with AES-256-GCM. The key is resolved in this order:

1. `RESTORELAB_MASTER_KEY` — base64 or hex, 32 bytes. Use this in containers,
   systemd units and CI.
2. `--master-key-file <path>`.
3. `~/.restorelab/master.key`, created with mode `0600` by `restorelab init`.

Generate one for a container deployment:

```bash
restorelab key generate            # prints a base64 key, stores nothing
export RESTORELAB_MASTER_KEY=...
```

**Back the key up separately from the config file.** Losing it means every
stored token has to be re-entered. Details in [security.md](security.md).

## Environment variables

| Variable | Effect |
| --- | --- |
| `RESTORELAB_CONFIG` | Path to the configuration file. |
| `RESTORELAB_MASTER_KEY` | Master key (base64 or hex), takes precedence over any key file. |
| `RESTORELAB_DATABASE_URL` | Drill history database, overrides `database.url`. |
| `NO_COLOR` | Disable coloured terminal output. |

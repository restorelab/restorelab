# Proxmox permissions

RestoreLab restores, boots and destroys virtual machines. That is exactly the
set of capabilities you do **not** want to hand out as `Administrator`.

## The short version

```bash
restorelab connect https://pve.example.com:8006
```

It asks for a Proxmox administrator's credentials **once**, uses them in memory
to create a dedicated service account with the minimal privilege set below, and
throws them away. Nothing is stored but the resulting API token, sealed.

That is the recommended path, and not only because it is shorter: faced with a
long permissions document, most people grant `Administrator` and move on. When
least privilege is also the easiest option, it is what people actually deploy.

Start read-only, which is enough for discovery and `--dry-run`:

```bash
restorelab connect https://pve.example.com:8006 --read-only
```

Then widen it when you are ready to run a real drill:

```bash
restorelab connect https://pve.example.com:8006 --token-name drills-rw
```

The rest of this document is what `connect` does on your behalf — read it if you
would rather do it by hand, if your security policy requires reviewing it, or if
you need to explain it to whoever owns the cluster.

## 1. A dedicated pool for recovery drills

Every workload RestoreLab creates lands in its own resource pool. This is what
makes least privilege possible: destructive rights are granted on the pool, not
on `/vms`.

```bash
pveum pool add restorelab --comment "Temporary workloads created by RestoreLab"
```

## 2. Roles

Two roles: one that can create and destroy inside the pool, one that can only
look at production.

```bash
# Full lifecycle, but only ever applied to the restorelab pool
pveum role add RestoreLabDrill --privs "\
VM.Allocate,\
VM.Audit,\
VM.Config.CPU,\
VM.Config.Disk,\
VM.Config.HWType,\
VM.Config.Memory,\
VM.Config.Network,\
VM.Config.Options,\
VM.GuestAgent.Audit,\
VM.Monitor,\
VM.PowerMgmt"

# Read-only discovery of production workloads and their backups
pveum role add RestoreLabRead --privs "VM.Audit,VM.Backup,Datastore.Audit,Sys.Audit"

# Writing restored disks onto the target storage
pveum role add RestoreLabStorage --privs "Datastore.Audit,Datastore.AllocateSpace"
```

| Privilege | Why RestoreLab needs it |
| --- | --- |
| `VM.Allocate` | Create the temporary workload, and destroy it during cleanup |
| `VM.Config.*` | Rewrite the network onto the isolated bridge, cap CPU/RAM, stamp ownership metadata |
| `VM.PowerMgmt` | Start and stop the temporary workload |
| `VM.Monitor`, `VM.GuestAgent.Audit` | Read the guest agent to learn the restored IP address |
| `VM.Audit` | List workloads and read their configuration |
| `VM.Backup` | Read the backup catalogue of a production workload (no write access to it) |
| `Datastore.Audit` | List backup snapshots on the backup storage |
| `Datastore.AllocateSpace` | Write the restored disks onto the target storage |
| `Sys.Audit` | Read node capacity and the bridge list, to verify isolation and free RAM |

RestoreLab never needs `VM.Console`, `VM.Clone`, `Sys.Modify`, `Realm.*`,
`User.Modify` or `Permissions.Modify`. If your token has them, remove them.

## 3. Service account and API token

```bash
pveum user add restorelab@pve --comment "RestoreLab recovery drills"

# Destructive rights, scoped to the drill pool only
pveum acl modify /pool/restorelab --users restorelab@pve --roles RestoreLabDrill

# Read-only on production workloads and node metadata
pveum acl modify /vms   --users restorelab@pve --roles RestoreLabRead
pveum acl modify /nodes --users restorelab@pve --roles RestoreLabRead

# Storages: the backup source (read) and the restore target (write)
pveum acl modify /storage/pbs-main   --users restorelab@pve --roles RestoreLabRead
pveum acl modify /storage/local-lvm  --users restorelab@pve --roles RestoreLabStorage

# The token itself. --privsep 1 keeps the token's rights a subset of the user's;
# grant the token the same ACLs explicitly if you keep privilege separation on.
pveum user token add restorelab@pve drills --privsep 0
```

The command prints the secret **once**. Store it with:

```bash
restorelab provider add proxmox \
    --id proxmox-main \
    --endpoint https://pve.example.com:8006 \
    --token-id 'restorelab@pve!drills' \
    --token-secret '<value printed above>'
```

RestoreLab seals it with AES-256-GCM before writing anything to disk. See
[security.md](security.md).

## 4. Proxmox Backup Server

PBS is only used for discovery and metadata in v0.1 — the restore itself is
driven by PVE. A read-only token on the datastore is enough:

```bash
proxmox-backup-manager user create restorelab@pbs --comment "RestoreLab (read-only)"
proxmox-backup-manager user generate-token restorelab@pbs drills
proxmox-backup-manager acl update /datastore/main DatastoreAudit \
    --auth-id 'restorelab@pbs!drills'
```

`DatastoreAudit` allows listing snapshots and reading their verification state.
Do **not** grant `DatastoreBackup` or `DatastoreAdmin`: RestoreLab must never be
able to write to, prune, or delete your backups.

## 5. Verifying the setup

```bash
restorelab provider test proxmox-main
```

The command checks reachability, authentication, and reports which required
privileges are missing rather than failing halfway through a drill.

## 6. What a compromised token can do

Being explicit about this is part of the threat model:

- **Can**: read the configuration and backup catalogue of every VM, create and
  destroy VMs inside the `restorelab` pool, write to the restore target storage.
- **Cannot**: modify or delete a production VM, delete or prune backups, change
  cluster or user configuration, open a console on a production workload.

If your target storage is shared with production data, the token can fill it.
Use a dedicated storage or a quota for recovery drills, and set
`limits.max_recovery_disk_gb` in the RestoreLab configuration.

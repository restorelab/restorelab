# Security model

RestoreLab is a privileged piece of infrastructure. It holds credentials that
can restore, boot and destroy virtual machines, and it reads your backup
catalogue. This document states what it protects, how, and what it does not
protect against.

## Threat model

| Asset | Threat | Mitigation |
| --- | --- | --- |
| Proxmox / PBS API tokens | Read from disk, from a backup of the config, or from a leaked log | Sealed with AES-256-GCM under a master key stored separately; never logged, never included in error messages |
| Production workloads | Deleted, modified or overwritten by a bug in RestoreLab | Delete requires ownership metadata; restores only ever create new workloads in a reserved ID range; no `force` flag is ever sent to Proxmox |
| Production network | A restored clone takes traffic, sends mail, or claims an IP | Isolated bridge by default, network configuration rewritten before first boot, isolation verified on the node, runs refused otherwise |
| Cluster capacity | Drills saturate the cluster and impact production | Concurrency limits, per-run CPU/RAM caps, capacity check before restore |
| Backups | Pruned, deleted or overwritten | The PBS token is read-only (`DatastoreAudit`); RestoreLab never writes to a backup datastore, and never asks for `Datastore.Allocate`, the privilege that would let it delete one |

## Secret handling

- The only secrets RestoreLab stores are provider API tokens.
- They are sealed with **AES-256-GCM**, a random 12-byte nonce per value, and
  written as `rlsec:v1:<base64>`. The version prefix exists so the scheme can be
  rotated later.
- The **master key is never stored in the configuration file**. It is resolved,
  in order, from:
  1. `RESTORELAB_MASTER_KEY` (base64 or hex, 32 bytes) — the right choice for
     containers, systemd units and CI;
  2. an explicit `--master-key-file`;
  3. `~/.restorelab/master.key`, created with `0600` by `restorelab init`.
- Saving a configuration containing an unsealed secret is **refused** by the
  config layer. That guard is what keeps a plaintext token off disk even if a
  future code path forgets to seal.
- Secrets are never printed: the provider type redacts itself in `String()`, and
  API errors truncate and sanitise response bodies.

**Losing the master key means losing every stored token.** They must be
re-entered — RestoreLab cannot and will not recover them. Back the key up
somewhere your configuration backup is not.

## The daemon keeps the master key in memory

Every other RestoreLab command loads the master key, unseals what it needs,
and exits within seconds. `restorelab serve` cannot do that: it has to answer
requests and query the cluster with no human present to unlock anything, so
it unseals provider secrets at startup and **keeps the master key in memory
for as long as the process runs**.

Stated plainly, because softening it would just mean someone finds out the
hard way: **a memory dump of the `serve` process hands an attacker your
unsealed Proxmox secrets.** There is no honest way around this. A separate
decryption agent that `serve` calls into for every request would face the
identical problem — the secret still has to exist, unsealed, somewhere
reachable by the process doing the querying — and it would add a second
process to secure instead of removing the exposure.

What actually reduces the risk is operational, not cryptographic:

- Run `serve` as a **dedicated, unprivileged user with no interactive
  shell**. It has no business being the same account you SSH in as.
- **Do not colocate it** with anything else. A daemon sharing a host with
  unrelated services multiplies what a single compromise of that host
  reaches.
- Keep the listener on **loopback**, behind a reverse proxy that terminates
  TLS and — where it matters — client authentication (see
  [Starting the server](api.md#starting-the-server)). A process that never
  binds a public interface is a process an external attacker cannot reach
  directly at all.
- Give that account the strict minimum on `~/.restorelab`: read access to
  `config.yaml` and `master.key`, write access to `history.db`. Nothing else.

None of this closes the exposure. It shrinks who can reach the process well
enough to read its memory in the first place, which is the only lever
available once a design commits to querying a cluster without a human
present at every request.

## API tokens

`restorelab serve` accepts its own credential, separate from any provider
secret: a bearer token created with `restorelab token create <name>`. A few
choices about it are worth stating explicitly:

- **Stored hashed, never in the clear.** Only the SHA-256 of a token is
  written to the database; `token create` prints the secret exactly once,
  and there is nothing left afterward that could be used to print it again.
- **SHA-256, deliberately, not argon2id.** argon2id exists to slow down
  cracking a *guessable* secret — a password a human chose. A RestoreLab
  token is 32 bytes straight out of `crypto/rand`: there is nothing to
  guess, and brute-forcing it costs 2²⁵⁶ operations regardless of how fast
  the hash is. A slow hash would buy no additional security here, would burn
  CPU on every authenticated request, and would hand an unauthenticated
  caller a trivial denial-of-service lever — one argon2 computation per
  guess thrown at the server. GitHub and Stripe hash their own tokens the
  same way, for the same reason. See the comment on `HashToken` in
  `internal/api/auth.go` for the full reasoning.
- **Compared in constant time.** The lookup is by hash, but the match is
  still verified with `crypto/subtle.ConstantTimeCompare`, so no comparison
  in the authentication path leaks how many leading bytes matched.
- **Prefixed `rl_`.** A token that leaks into a log, a ticket or a public
  repository is recognisable as a RestoreLab credential on sight, which is
  what lets it be revoked instead of puzzled over.

## Destructive-operation guardrails

RestoreLab only ever destroys what it created. Concretely:

1. Every temporary workload is stamped at creation with
   `restorelab_managed=true`, `restorelab_run_id`, `restorelab_source_id`,
   `restorelab_created_at`, and the `restorelab` tag.
2. `Delete` re-reads the workload and returns `resource is not managed by
   restorelab` unless that metadata is present. There is no override flag.
3. Temporary IDs come from a reserved range (`9000–9999` by default) and are
   allocated from the cluster's free-ID endpoint; a restore never targets an
   existing workload and never sends Proxmox's `force` parameter.
4. Cleanup runs on a **detached context**: cancelling a run, or a timeout, still
   destroys the temporary workload. A cleanup that fails is reported as
   `CLEANUP_FAILED` with the exact node and VMID, because a silent orphan is the
   worst possible outcome.

## Transport security

- TLS certificate verification is on by default. `insecure: true` exists for
  homelabs with self-signed Proxmox certificates and is a per-provider,
  explicitly opted-in setting.
- PBS certificate **fingerprint pinning** is supported and is the correct choice
  for a self-signed PBS: it verifies the exact certificate instead of trusting
  the system roots.
- A custom CA can be supplied per provider with `ca_cert_path`.

## Deployment guidance

- Run RestoreLab on a host that is **not** your Proxmox node, with network
  access to the API and to the isolated recovery bridge.
- Give the process its own unprivileged user; the master key file and the
  configuration should be readable by that user only.
- In containers, inject `RESTORELAB_MASTER_KEY` as a secret (not an environment
  variable baked into an image, not a build argument).
- Restrict outbound access from the RestoreLab host: it needs your Proxmox API,
  your PBS API, the isolated bridge, and your notification endpoints. Nothing
  else.

## What RestoreLab does not protect against

Stated plainly, because a security document that only lists strengths is
marketing:

- **A compromised RestoreLab host.** An attacker with the master key and the
  configuration has your provider tokens. The scoped Proxmox role limits the
  blast radius (see [proxmox-permissions.md](proxmox-permissions.md)) but
  does not eliminate it.
- **A malicious restored guest.** You are booting an untrusted disk image. The
  isolated bridge contains it at the network layer; it is not a hypervisor
  escape mitigation. Do not run drills for workloads you do not trust on a node
  that hosts production.
- **Backup integrity.** RestoreLab proves a backup *recovers*. It does not prove
  it is free of a compromise that predates it: a drill of a backdoored server
  will pass every check.
- **Data confidentiality inside the drill.** A restored production database
  contains production data. The isolated network is the boundary; if someone can
  reach that bridge, they can reach the data.

## Reporting a vulnerability

Do not open a public issue. See [SECURITY.md](../SECURITY.md) for the private
disclosure process.

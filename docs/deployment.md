# Where to run RestoreLab

**You do not install RestoreLab on your Proxmox node.** It talks to the API over
HTTPS and can run anywhere: your laptop, a container, a CI runner, a small VM.

The one question worth thinking about is not *where the binary lives* — it is
*how your checks reach the restored service*.

## Two different network needs

```text
RestoreLab ──── HTTPS :8006 ─────► Proxmox API        (from anywhere)
           └─── TCP / ICMP / HTTP ► restored guest on vmbr99   (needs a route)
```

The first connection is enough for everything structural: listing workloads,
finding backups, restoring, starting, **discovering the guest's IP through the
guest agent**, and destroying the temporary workload. All of it travels over the
API.

The second is what network checks (`tcp`, `http`, `ping`, `dns`) need. And the
recovery bridge deliberately has no uplink, so a laptop on your office LAN
cannot reach `10.99.0.14` on `vmbr99`.

> "Isolated" means the guest cannot get **out**. It does not mean nothing can
> get **in**. You need exactly one controlled path inwards — that asymmetry is
> the design, and it is what the `nftables` rules in
> [network-isolation.md](network-isolation.md) enforce.

## The simple answer: in-guest checks

Use `command` checks. They run **inside** the restored guest through the QEMU
guest agent, which means they travel over the Proxmox API like everything else:

```yaml
checks:
  - type: command
    name: PostgreSQL
    run: pg_isready -h localhost
  - type: command
    name: nginx
    run: systemctl is-active nginx
    expect: active
```

With a plan built this way you need **no route into the recovery network, no
DHCP server on the isolated bridge, and no agent deployed next to it**.
RestoreLab runs on your laptop and still proves the service actually came back.
The drill waits for the guest agent to answer instead of waiting for an address.

Requirements: `qemu-guest-agent` installed and running in the guest, and the
agent enabled on the VM (*VM → Options → QEMU Guest Agent*). That is good
practice anyway — it is also what gives you filesystem-consistent backups.

## When you do want network checks

Testing the service the way a client sees it — through its listening socket, its
TLS certificate, its HTTP stack — has real value that an in-guest command cannot
replace. For that you need a path into the recovery network. In order of
preference:

### 1. A runner with a leg on the recovery bridge (recommended)

```text
                    ┌──────────────────────────┐
   your LAN ────────┤ CT restorelab-runner     ├──── vmbr99 (10.99.0.2)
                    │  eth0: LAN               │      │
                    │  eth1: vmbr99            │      ▼
                    └──────────┬───────────────┘  restored guest
                               │                  (10.99.0.14)
                               └── HTTPS :8006 ──► Proxmox API
```

A 512 MB Debian container on the cluster, with nothing on it but the RestoreLab
binary. The token that can destroy workloads does not live on the hypervisor,
and you get the route without punching a hole from outside.

### 2. An address on the bridge, plus a route

Give the node an address on `vmbr99` (`ip addr add 10.99.0.1/24 dev vmbr99`) and
route to it from your machine, over WireGuard for anything that is not a lab.
Works, but it is a route into the drill network that you now have to maintain
and to remember.

### 3. On the Proxmox node itself

It works — the node has an interface on the bridge. But you are putting a tool
that can destroy virtual machines on the hypervisor itself, which is the one
place where a compromise costs you everything. Lab only.

### 4. Remote probes

Planned for v0.4: a small `restorelab probe` agent that lives on the network
that matters and executes checks on the server's behalf. That is the clean
answer for "does this service answer from the DMZ?", and it is why the
architecture separates the two roles.

## Deployment options

### Binary

```bash
go build -o bin/restorelab ./cmd/restorelab
./bin/restorelab init
```

### Container

```bash
docker build -f deployments/docker/Dockerfile -t restorelab .

docker run --rm \
    -e RESTORELAB_MASTER_KEY="$(cat master.key)" \
    -v ~/.restorelab:/home/restorelab/.restorelab \
    restorelab recovery test 101
```

Pass the master key as a secret, not as a value baked into an image. Attach the
container to the recovery bridge (`--network`, or a macvlan on `vmbr99`) only if
your plan uses network checks.

### Scheduled drills

Until the built-in scheduler lands, a cron entry or a systemd timer on the
runner is enough:

```
0 3 * * 0  restorelab recovery run /etc/restorelab/plans/postgres-prod.yaml --report /var/log/restorelab/$(date +\%F).json
```

## What to give the process

- Its own unprivileged user.
- The configuration and the master key readable by that user only.
- Outbound access to: the Proxmox API, the PBS API, the recovery network if you
  use network checks, and your notification endpoints. Nothing else.

See [security.md](security.md) for the full model.

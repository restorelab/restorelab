# Where to run RestoreLab

**You do not install RestoreLab on your Proxmox node.** It talks to the API over
HTTPS and can run anywhere: your laptop, a container, a CI runner, a small VM.

The one question worth thinking about is not *where the binary lives*. It is
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
> get **in**. You need exactly one controlled path inwards. That asymmetry is
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
practice anyway. It is also what gives you filesystem-consistent backups.

## When you do want network checks

Testing the service the way a client sees it (through its listening socket, its
TLS certificate, its HTTP stack) has real value that an in-guest command cannot
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

It works: the node has an interface on the bridge. But you are putting a tool
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

## Serving the dashboard

`restorelab serve` answers on `/` as well as on `/api/v1`. The web interface is
compiled into the binary, so there is nothing to deploy beside it: no static
directory to copy, no second web server, no path to keep in sync with a
release. If a build carries no dashboard, `/` says so in a sentence instead of
returning a puzzling 404.

That last case is a build choice, not a fault. The published release archives
and the container image both carry the interface; a binary produced by
`make build` alone does not, because compiling the front-end needs Node and a
Go developer must not. `make ui` compiles it, and `make dist` depends on that,
so a release cannot ship without one.

Signing in needs an API token: `restorelab token create <name>`, read-only
unless `--operate` or `--manage` is given. It is exchanged once for a session
cookie that lasts twelve hours and is never extended; `restorelab token revoke`
cuts every session that token opened, at the next request.

### TLS is no longer optional

The bearer API could be run in the clear on a trusted LAN and only lose
confidentiality. The dashboard cannot: its session cookie is `Secure`, so a
browser reached over plain HTTP stores nothing, the login appears to succeed,
and every request afterwards is silently anonymous. `POST /api/v1/session`
therefore refuses with a `400` naming TLS on any host that is not loopback.

Either put a TLS-terminating proxy in front of RestoreLab, or reach it on
`localhost` (an SSH tunnel counts).

### `proxy_set_header Host $host;` is mandatory

Not a nicety. The CSRF guard on cookie-authenticated writes compares the
request's `Origin` against its `Host`, and the reference is the request's own
`Host` precisely so that there is no origin to configure and get wrong. A
reverse proxy that rewrites `Host` to `127.0.0.1:8080` makes the browser's
`Origin: https://restorelab.example.com` disagree with it, and **every write
from the dashboard becomes a 403**: reads keep working, which makes it look
like a permissions bug rather than a proxy one.

```nginx
server {
    listen 443 ssl;
    server_name restorelab.example.com;

    ssl_certificate     /etc/letsencrypt/live/restorelab.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/restorelab.example.com/privkey.pem;

    location / {
        proxy_pass http://127.0.0.1:8080;

        # Required. The API compares Origin against Host; without the original
        # Host, every dashboard write is a 403.
        proxy_set_header Host $host;

        # Lets the login know TLS was terminated in front of it.
        proxy_set_header X-Forwarded-Proto $scheme;

        # The event stream is long-lived and must not be buffered.
        proxy_buffering off;
        proxy_read_timeout 1h;
    }
}
```

Caddy's `reverse_proxy` preserves the original `Host` by default and needs no
equivalent line; Traefik does too. nginx is the one that rewrites it unless
told otherwise.

## What to give the process

- Its own unprivileged user.
- The configuration and the master key readable by that user only.
- Outbound access to: the Proxmox API, the PBS API, the recovery network if you
  use network checks, and your notification endpoints. Nothing else.

See [security.md](security.md) for the full model.

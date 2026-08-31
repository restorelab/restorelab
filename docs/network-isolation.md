# Network isolation

This is the single most important safety property in RestoreLab.

A restored production workload is a **perfect clone** of a live server: the same
hostname, the same IP configuration, the same MAC address, the same credentials,
the same cron jobs, the same mail queue, the same replication configuration, the
same certificates. Booting that clone on the production network is not a test —
it is an incident.

Concretely, an unisolated recovery drill can:

- claim the production IP address and blackhole the real service (ARP conflict);
- send the emails that were queued at the moment of the backup;
- resume a replication stream and write to a live database;
- re-register with a service discovery, a load balancer or a cluster and take
  real traffic;
- call external APIs — payment providers, webhooks, monitoring — as itself;
- renew or revoke a certificate under the production identity.

RestoreLab therefore restores onto an isolated network **by default**, rewrites
the network configuration inherited from the backup, and refuses to run when it
cannot verify isolation.

## What RestoreLab does on every restore

1. **Reserved ID range** — the temporary workload is created with a VMID in
   `9000–9999` (configurable), never over an existing workload.
2. **Network rewrite** — after the restore and *before the first boot*, every
   interface from the backup is dropped and a single interface is attached to
   the isolated bridge, with a freshly generated MAC address. The production
   bridge and MAC never make it to a running workload.
3. **Isolation validation** — the target bridge is inspected on the node. A
   bridge with physical ports (`bridge_ports`) or a gateway has an uplink and is
   rejected with `restore network is not isolated`.
4. **Boot hygiene** — `onboot=0` (the clone must never come back after a node
   reboot) and `protection=0` (so cleanup can always destroy it).
5. **Ownership metadata** — `restorelab_managed=true`, the run ID and the source
   workload ID are stamped on the description, and the `restorelab` tag is set.

## Creating the isolated bridge on Proxmox

A Linux bridge with **no ports and no gateway** is a switch that goes nowhere.
That is what you want.

### With RestoreLab

```bash
restorelab network create
```

It asks for an administrator's password, creates the bridge through the
Proxmox API and applies it. `restorelab connect --create-bridge` does the same
during onboarding, and plain `connect` offers it when it notices the bridge is
missing.

This needs administrator credentials rather than RestoreLab's own token: the
service account deliberately has no `Sys.Modify`, because a tool that runs
recovery drills has no business reconfiguring your hypervisor's network. The
password is used once, in memory, and never stored.

Two things it will refuse to do, on purpose:

- **touch a bridge that already has ports, an address or a gateway.** Turning
  such a bridge into an isolated one would cut the node off its own network.
  It stops and tells you what it found.
- **pretend applying is free.** Activating the configuration reloads the
  node's networking. Adding a portless bridge touches no existing interface,
  but the change is real — `--no-apply` writes the configuration and leaves it
  to take effect at the next reboot.

The manual alternatives below remain valid, and are what `network create` does
on your behalf.

### Web UI

*Datacenter → node → System → Network → Create → Linux Bridge*

| Field | Value |
| --- | --- |
| Name | `vmbr99` |
| IPv4/CIDR | *(empty)* |
| Gateway | *(empty)* |
| Bridge ports | *(empty)* |
| Comment | `RestoreLab isolated recovery network` |

Apply the configuration. Repeat on **every node** that can be a restore target —
a bridge missing on one node turns into a failed drill, or worse, a silent
fallback if you configured one.

### /etc/network/interfaces

```
auto vmbr99
iface vmbr99 inet manual
    bridge-ports none
    bridge-stp off
    bridge-fd 0
#RestoreLab isolated recovery network
```

```bash
ifreload -a
```

Then point RestoreLab at it:

```yaml
networks:
  isolated:
    bridge: vmbr99
    isolated: true
```

## Getting an IP address inside the isolation

**Or don't.** If your plan uses `command` checks, the validation runs inside the
guest through the agent and never needs an address at all — no DHCP, no route,
nothing on this bridge but the workload under test. That is the simplest
answer and usually the right one; see [recovery-plans.md](recovery-plans.md#command)
and [deployment.md](deployment.md).

For network checks (`tcp`, `http`, `ping`), the guest does need an address, and
an isolated bridge has no DHCP. Pick one of:

**a. QEMU guest agent (recommended, no extra infrastructure).**
The agent reports the interfaces the guest configured itself, including static
addresses baked into the image. RestoreLab reads them through the Proxmox API.
Requires `qemu-guest-agent` installed in the guest and the agent enabled on the
VM — which is good practice for backup consistency anyway.

**b. A DHCP server on the isolated bridge.**
Run `dnsmasq` on the Proxmox node, bound to `vmbr99` only:

```bash
apt install dnsmasq
cat >/etc/dnsmasq.d/restorelab.conf <<'EOF'
interface=vmbr99
bind-interfaces
dhcp-range=10.99.0.50,10.99.0.200,12h
# No default route is advertised: the guest gets an address, not an exit.
dhcp-option=3
dhcp-option=6
EOF
ip addr add 10.99.0.1/24 dev vmbr99
systemctl restart dnsmasq
```

Giving the node an address on `vmbr99` is what lets RestoreLab's checks reach
the restored guest at all. The bridge still has no uplink, so the guest can talk
to the node and to nothing else.

**c. A static address in the plan.**

```yaml
startup:
  ip: 10.99.0.14
  wait_for_ip: false
```

## Hardening further (optional, recommended in production)

A bridge with no uplink already blocks routing. If the node itself is on a
sensitive network and you want defence in depth, add egress filtering with
`nftables` on the node:

```bash
nft add table inet restorelab
nft add chain inet restorelab forward '{ type filter hook forward priority 0; policy accept; }'
# Nothing leaves the recovery bridge, ever.
nft add rule inet restorelab forward iifname "vmbr99" oifname != "vmbr99" drop
# The node may reach the guests (checks), the guests may not reach the node's services.
nft add chain inet restorelab input '{ type filter hook input priority 0; policy accept; }'
nft add rule inet restorelab input iifname "vmbr99" ct state new drop
```

Additional layers worth considering, in rough order of value:

- a **dedicated VLAN** for `vmbr99` if the bridge must span nodes;
- the **Proxmox firewall** on the temporary VM with a deny-all outbound rule
  (RestoreLab can set `firewall=1` on the interface via the network profile);
- a **separate cluster or node** for recovery drills, if your RTO testing volume
  justifies it.

## Verifying isolation yourself

Do not take our word for it. Restore something once with cleanup disabled and
look:

```bash
restorelab recovery test 101 --keep --network isolated
```

On the node:

```bash
brctl show vmbr99          # no physical interface should be attached
ip -br link show master vmbr99
qm config 9101 | grep -E '^(net|onboot|protection|tags)'
```

Inside the guest (via the Proxmox console, not the network):

```bash
ip route            # no default route
ping -c1 1.1.1.1    # must fail
```

Then destroy it:

```bash
restorelab cleanup 9101
```

## When isolation is deliberately not what you want

Some drills need real connectivity — validating that a restored application can
actually reach an external dependency, for example. That is a legitimate but
dangerous scenario, so it is never a default:

```yaml
restore:
  network: staging-vlan   # a profile with isolated: false
```

RestoreLab will refuse to use a non-isolated profile unless the plan names it
explicitly, and every such run is flagged in the report. Before you do this, be
sure the restored guest cannot claim a production IP or send mail — usually that
means preparing the workload for it, not weakening the network.

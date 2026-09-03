# Security policy

## Supported versions

RestoreLab is in alpha. Only the tip of `master` and the most recent tagged
release receive security fixes.

## Reporting a vulnerability

**Do not open a public issue.**

Use GitHub's private vulnerability reporting on this repository:
**Security → Report a vulnerability**. It is the only channel, and it is
deliberately the only one - a report that arrives there is private, threaded,
and cannot be lost in a mailbox.

Please include:

- what you found and where (file, endpoint, command);
- the impact you believe it has;
- a minimal reproduction, or the reasoning that led you there;
- your assessment of severity.

You will get an acknowledgement within 72 hours and an assessment within 7 days.
Fixes are released as soon as they are ready; credit is given unless you prefer
otherwise.

## Scope

Especially relevant to RestoreLab:

- anything that could cause a **production** workload to be modified or
  destroyed;
- anything that could place a restored workload on a **non-isolated** network;
- **secret exposure**: tokens in logs, in error messages, in reports, or
  written unsealed to disk;
- privilege escalation through the Proxmox or PBS API beyond the documented
  minimal role;
- injection into the generated HTML or JSON reports.

Out of scope: findings that require an already-compromised RestoreLab host, and
the documented limitations in [docs/security.md](docs/security.md#what-restorelab-does-not-protect-against).

## Please do not

Test against someone else's infrastructure, or against a production Proxmox
cluster you do not own. This project restores and deletes virtual machines.
Use a lab.

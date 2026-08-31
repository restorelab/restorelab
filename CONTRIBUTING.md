# Contributing to RestoreLab

Thanks for considering it. RestoreLab is infrastructure software that deletes
virtual machines for a living, so the bar for correctness is higher than the bar
for features.

## Getting set up

```bash
git clone https://github.com/restorelab/restorelab
cd restorelab
make build      # binary in bin/
make check      # gofmt + go vet + go test — what CI runs
```

Requires Go 1.27+. No Proxmox cluster is needed: providers are tested against an
in-process mock API, and the engine against an in-memory fake provider.

## Ground rules

1. **`internal/core` depends on nothing.** Domain types and interfaces live
   there; everything else points at it. A provider importing the engine, or the
   engine importing Proxmox, is a design bug.
2. **Tests come with the change.** Table-driven, standard library only
   (`testing`, `httptest`) — no assertion frameworks. Tests must not touch the
   network or require privileges.
3. **Safety invariants are not refactorable.** Cleanup always runs; delete
   requires ownership metadata; restores never target an existing workload;
   restores land on an isolated network. If a change touches one of these, say
   so explicitly in the pull request and add the test that proves it still
   holds.
4. **No new dependencies without discussion.** Open an issue first. The
   dependency list is short on purpose.
5. **Secrets never reach a log, an error message or a report.**

## Adding a check

Implement `core.Check` in `internal/checks`, register it in `Default()`, and
document its parameters in `docs/recovery-plans.md`. A check must:

- honour the context and never implement its own retry loop or timeout (the
  registry owns both);
- return `CheckFail` for "the service is not healthy" and `CheckError` for "I
  could not run" — the distinction drives the run's verdict;
- produce a failure message an administrator can act on at 3am
  (`connection refused on 10.99.0.14:5432 after 51ms`, not `check failed`).

## Adding a provider

Implement `core.HypervisorProvider` and/or `core.BackupProvider` in
`internal/providers/<name>`, with a mock API server in the package's tests.
Implement `core.NetworkValidator` if the platform can prove network isolation,
and `core.CapacityReporter` if it can report free capacity. Do not add
provider-specific behaviour to the engine.

## Commits and pull requests

- Atomic commits, one logical change each.
- Message format: `type: description` (`feat`, `fix`, `refactor`, `chore`,
  `docs`, `test`), imperative mood, English.
- Pull requests explain *why*, not just what. Screenshots or terminal output for
  anything user-visible.
- `make check` must pass.

## Reporting bugs

Include your Proxmox VE / PBS versions, the RestoreLab version
(`restorelab version`), the plan you ran (with secrets removed), and the JSON
report if you have one. For anything touching security, follow
[SECURITY.md](SECURITY.md) instead of opening an issue.

## Licence

Contributions are accepted under the AGPL-3.0, the licence of this project.

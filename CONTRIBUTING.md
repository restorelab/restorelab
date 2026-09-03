# Contributing to RestoreLab

Thanks for considering it. RestoreLab is infrastructure software that deletes
virtual machines for a living, so the bar for correctness is higher than the bar
for features.

## Getting set up

```bash
git clone https://github.com/restorelab/restorelab
cd restorelab
make build      # binary in bin/
make check      # gofmt + go vet + go test: what CI runs
```

Requires Go 1.27+. No Proxmox cluster is needed: providers are tested against an
in-process mock API, and the engine against an in-memory fake provider.

**Node is not required to build or test the Go side.** The dashboard is
compiled separately and embedded, and `internal/ui` tolerates an empty build
directory precisely so that `make build`, `make check` and `go test ./...` work
on a machine that has never run npm. Its own targets, which need Node 22+:

```bash
make ui         # compile the dashboard into internal/ui/dist
make ui-dev     # dev server on :5173, proxying to serve on :8080
make ui-lint    # Biome: lint and format check
make ui-test    # Vitest
```

`make dist` depends on `make ui`; `make build` and `make lint` deliberately do
not.

## Ground rules

1. **`internal/core` depends on nothing.** Domain types and interfaces live
   there; everything else points at it. A provider importing the engine, or the
   engine importing Proxmox, is a design bug.
2. **Tests come with the change.** Table-driven, standard library only
   (`testing`, `httptest`), no assertion frameworks. Tests must not touch the
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
  could not run": the distinction drives the run's verdict;
- produce a failure message an administrator can act on at 3am
  (`connection refused on 10.99.0.14:5432 after 51ms`, not `check failed`).

## Adding a provider

Implement `core.HypervisorProvider` and/or `core.BackupProvider` in
`internal/providers/<name>`, with a mock API server in the package's tests.
Implement `core.NetworkValidator` if the platform can prove network isolation,
and `core.CapacityReporter` if it can report free capacity. Do not add
provider-specific behaviour to the engine.

## Running the dashboard in development

The dashboard is compiled into the binary from `internal/ui/dist`, so
`go build ./...` and `go test ./...` work with no Node installed: the embedded
directory is allowed to be empty, and `/` then serves a page saying the
dashboard was not built into this binary.

The development loop runs the front-end dev server on `:5173` against
`restorelab serve` on `:8080`, with the dev server proxying `/api`. **The proxy
must rewrite the `Origin` header to the proxy target, not just the `Host`.**

`changeOrigin: true` rewrites `Host` alone. The API compares `Origin` against
`Host` (that is the CSRF guard on cookie-authenticated writes, and it has no
configured origin to relax), so the dev server sends
`Origin: http://localhost:5173` against `Host: localhost:8080` and **every
write from the dev server is a 403**. Reads keep working, the login works, the
cookie is stored, and nothing in the response explains why creating a plan
fails. Rewrite the header:

```js
// vite.config.ts
server: {
  proxy: {
    '/api': {
      target: 'http://localhost:8080',
      changeOrigin: true,
      configure: (proxy) => {
        // changeOrigin only fixes Host. The API's CSRF guard reads Origin.
        proxy.on('proxyReq', (proxyReq) => {
          proxyReq.setHeader('origin', 'http://localhost:8080')
        })
      },
    },
  },
}
```

**The dev server does not send the Content-Security-Policy the binary does.**
So a font from Google, an image as a `data:` URI, a script from a CDN or a call
to a third-party endpoint all work on `:5173` and are blocked, silently, once
the same code is served from `restorelab serve`. What the policy allows, and
why, is in `docs/security.md` under "The bundle's Content-Security-Policy".
Build the binary and look at the real page before trusting a change that adds
an external resource.

Two other things about the loop are worth knowing before they cost an hour:

- The event stream must not be buffered by the proxy, or a drill's progress
  arrives in one burst when it ends instead of as it happens.
- If you write a Go test that drives a session with `net/http/cookiejar`, point
  it at `localhost`, not at `127.0.0.1`. The jar applies the browser rule (a
  `Secure` cookie goes back over https, or to localhost), and it spells that
  exemption by the literal name, which `127.0.0.1` is not. The jar stores the
  cookie and then never sends it, and the test fails for a reason that has
  nothing to do with the server. `internal/e2e/session_test.go` has the
  one-line helper.

## The API fixtures

`web/src/api/types.ts` mirrors the Go DTOs by hand, and there is no OpenAPI
document to derive either side from. What keeps the two honest is a set of
captured responses: `internal/api/fixtures_test.go` drives the real handlers
and writes the actual body of every route the dashboard reads into
`web/src/api/__fixtures__`, and `web/src/api/fixtures.ts` reads them back
under their declared types, which `tsc` checks.

Change a DTO and the golden test fails, naming the file:

```bash
go test ./internal/api/ -run TestFixturesMatchTheWire -update
```

Then run `npx tsc --noEmit` in `web/`. If it fails, `types.ts` is behind the
API, which is exactly what this is for.

Three things worth knowing before they cost an hour:

- **Everything captured must be deterministic.** The server takes an injected
  clock (`Options.Now`) and an injected id generator (`Options.NewID`) for
  this reason. A handler that reads `time.Now()` on its own cannot be
  captured; that is how `GET /workloads/{id}/backups` was found to be ageing
  its rows against a wall clock instead of the server's.
- **The fixtures are excluded from Biome** (`biome.json`). Their canonical
  formatting is the Go writer's, and letting a formatter reflow them would
  make every regeneration a failing check.
- **The check runs in one direction.** It proves every key `types.ts`
  requires is in the capture with the right type, so a key the server renames
  or drops fails on the spot. It cannot see a key the capture has and the type
  does not: excess properties are only flagged on object literals, and a
  fixture is an imported module. So a field *added* to a DTO, and a field
  *deleted* from `types.ts`, both pass. When you add a field to a DTO, add it
  to `types.ts` too. Nothing will remind you.

The front-end tests answer with these captures rather than building payloads
of their own. That is the other half of the point: a test that invents its own
response proves only that the code agrees with the test.

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

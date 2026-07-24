# Contributing to docker-renovate-scheduler

A small Go scheduler that wraps the official
[Renovate](https://github.com/renovatebot/renovate) bot image and runs it on a
schedule (or on an external trigger) as a resident container. This guide covers
what the
[org-wide defaults](https://github.com/cplieger/.github/blob/main/CONTRIBUTING.md)
don't: why this image is **not** distroless, the package layout, the guardrails
to preserve, and how to run the checks locally.

## Why this image wraps `renovate/renovate` (not distroless)

The [README's "Not distroless" section](README.md#not-distroless-on-purpose)
covers _why_ the final stage is `FROM renovate/renovate:<version>` (Renovate
installs toolchains at runtime via containerbase, so there is no static form to
drop on `scratch`). The contributor-facing consequence: every run must inherit
the containerbase environment (PATH, `CONTAINERBASE_*`, `USER_*`) that the
upstream `renovate-entrypoint.sh` sets up, so the daemon **execs renovate
through that entrypoint** rather than invoking the `renovate` binary directly
(see `renovateInvocation` in `runner.go`). This matters doubly now: a
triggered run executes with the CLIENT's forwarded environment, which never
passed through the image ENTRYPOINT at all. Do not "simplify" this to a bare
`exec renovate`; tool installation breaks without the entrypoint's env.

The Dockerfile strips the bundled `docker` CLI from that base
(`/opt/containerbase/tools/docker` plus its shim and PATH symlink); the README
carries the user-facing rationale. That removal was verified safe against
Renovate's exec layer (`lib/util/exec/`): every `docker` invocation is gated on
`binarySource === 'docker'`, and the docker datasource resolves digests over
HTTP, not via the CLI.

## Layout

Go module `github.com/cplieger/docker-renovate-scheduler`; binary
`docker-renovate-scheduler`. Flat package. The architecture is
**single-owner**: the daemon (PID 1) executes every Renovate run as its own
child; triggers only submit requests.

- `main.go`: subcommand dispatch (`daemon` / `run` / `health`).
- `daemon.go`: the daemon composition root (`runDaemon`): health marker,
  trigger socket, the executor goroutine (`runJobs`, the ONLY code that
  starts Renovate), and the built-in ticker (`startTicker`, a
  `scheduler.RunLoop` that submits tick jobs like any other trigger).
  Shutdown is `signal.NotifyContext` + queue close: the in-flight run drains
  uncancelled (bounded by `SCHED_TIMEOUT`), queued requests are cancelled
  with explicit results.
- `client.go`: the `run` subcommand: a thin synchronous client that forwards
  its repo args and its **complete environment** (that is what makes
  `docker exec -e RENOVATE_X=… … run` overrides reach Renovate) and exits
  with the run's own result.
- `payload.go`: the `runPayload` wire type (repos + forwarded environment)
  submitted over the scheduler library's trigger broker
  (`scheduler/v3/trigger`: bounded FIFO queue, owner-only unix-socket server,
  newline-JSON queued/started/done protocol, synchronous client). The broker
  mechanics live and are tested in that library; this app supplies only the
  payload type, the executor policy, and the log wording.
- `runner.go`: `renovateInvocation` builds the command that routes through
  `/usr/local/sbin/renovate-entrypoint.sh` (re-establishing containerbase per
  run, whatever environment the request carried; the Dockerfile asserts the
  path at build time); `runRenovateOnce` executes one pass under
  `SCHED_TIMEOUT`; `defaultCommandRunner` wraps `scheduler.NewCommandRunner`
  (wiring `Stdout`/`Stderr` to the daemon's streams, so every run's output
  lands in the container log).
- `config.go`: env loading (`loadInterval` via `scheduler.ParseInterval`,
  `loadRunTimeout`), `setupLogger` (`slogx`), and the base-dir verification
  (boot + per-run).
- `rootless.go`: the non-default-UID startup warning
  (`warnIfRootlessCacheUnwritable`): a custom UID with no tool-cache
  redirection breaks lockfile regeneration silently, so it is surfaced
  loudly at boot (see "Running as a non-default user" in the README).
- `health.go`: thin wrapper over `github.com/cplieger/health` (file marker).

There is no cross-process coordination state: no flock, no rerun flag, no
drain latch. Mutual exclusion is the executor loop; the socket is the only
trigger path.

## Env-var convention (don't collide with Renovate)

Scheduler knobs use the **`SCHED_*`** prefix (`SCHED_INTERVAL`,
`SCHED_TIMEOUT`), never `RENOVATE_*`: that is Renovate's own config
namespace and the bot would try to interpret our knobs as config. `LOG_LEVEL`
is intentionally shared. Renovate itself is configured the normal way
(`config.js` / `RENOVATE_*` env / each repo's `renovate.json`); the scheduler
never parses or rewrites Renovate config.

## Guardrails (don't weaken)

- Runs as the renovate image's non-root user (UID `12021`); `RENOVATE_BASE_DIR`
  is `/data` (writable). No network listener, no exposed ports. Overriding the
  UID is an operator choice with a caveat: a custom UID has no writable home, so
  each language cache must be redirected to a writable volume and forwarded via
  `RENOVATE_CUSTOM_ENV_VARIABLES` (see "Running as a non-default user" in the
  README); don't try to paper over that inside the image.
- Every renovate run is executed by the daemon's single executor goroutine;
  that is the overlap guard. Do not add a second execution path (a client
  that runs Renovate itself, a bypass around the queue): the built-in ticker
  and every external trigger MUST submit through the queue, or two Renovate
  processes can race the same base directory. Every accepted request gets its
  own run and its own true exit code; keep both properties.
- The trigger socket stays owner-only (`0600`) and in-container. Do not add a
  TCP/HTTP listener; "no network listener" is a documented product
  differentiator, and the forwarded request environment (which can carry
  `RENOVATE_TOKEN`) must never cross a wider boundary than the same-user
  socket.
- `exec.CommandContext` arg lists only; never build a `sh -c` string from env.
- The run timeout (`SCHED_TIMEOUT`) is propagated via context so a wedged
  renovate run is killed, not left running into the next tick.
- The bundled `docker` CLI is removed in the Dockerfile (`binarySource=install`
  never invokes it). Don't re-add it or switch to `binarySource=docker`; if a
  feature ever seems to need it, re-check Renovate's exec layer first (every
  `docker` call is gated on `binarySource === 'docker'`, which is deprecated
  upstream).

## Scheduling modes

- **Built-in** (default): `SCHED_INTERVAL=<Go duration>` (e.g. `1h`) → a
  `scheduler.RunLoop` (startup pass, then every interval).
- **External**: `SCHED_INTERVAL=off` (or `disabled` / `0` / `0s`) → the
  container idles and an external scheduler triggers
  `docker exec renovate docker-renovate-scheduler run` (e.g. Ofelia on a fixed
  cadence, or a webhook-driven action for ad-hoc runs).

## Conventions and gotchas

- Logs are slog logfmt to stderr (`key=value`) with UTC timestamps via `slogx` (its `UTCTime` `ReplaceAttr`, so the image needs no `TZ` and embeds no `time/tzdata`); always use key/value pairs,
  never a formatted string (the `sloglint` linter enforces it).
- `main()` orchestration and the renovate subprocess exec are
  intentionally not unit-tested (process-level I/O, validated by container logs
  and Grafana alerting). New logic in `config.go` / `runner.go`
  is expected to come with tests.
- Tests are table-driven and live beside the code (`*_test.go`). They cover
  the daemon executor's policy as observed over a real unix socket (the
  broker mechanics of queue semantics, socket hygiene, and wire ordering are
  the scheduler library's and are tested there), scope + environment forwarding
  end-to-end (the client's env reaches the Renovate child), the executor's
  shutdown drain (in-flight finishes, queued cancelled explicitly), the
  entrypoint-routed invocation builder, and timeout-vs-failure log
  classification. Fake command runners (`true`/`false`/`sh -c` assertions)
  stand in for the real entrypoint, which is absent outside the image.

## Running checks locally

Requires Go (see `go.mod`) and `golangci-lint` v2. If this repo is checked out
inside a parent `go.work` that doesn't include it, run module-local commands
with `GOWORK=off`:

```sh
GOWORK=off go build ./...
GOWORK=off go test -race ./...
GOWORK=off golangci-lint run          # config synced from cplieger/ci
GOWORK=off golangci-lint fmt          # gofumpt (extra-rules) + gci ordering
```

`golangci-lint run` reports unformatted files as issues, so run
`golangci-lint fmt` before pushing. Build the image with:

```sh
docker build -t docker-renovate-scheduler .
```

## Commits and PRs

This repo uses [Conventional Commits](https://www.conventionalcommits.org/)
parsed by git-cliff to generate release notes, so the subject becomes a
changelog line: `feat:` (Added), `fix:` (Fixed), `sec:` (Security),
`chore(deps):` (Dependencies). Keep changes focused; open the PR against `main`
and make sure `ci / validate` is green before merging.

## Conduct & security

By participating you agree to the
[Code of Conduct](https://github.com/cplieger/.github/blob/main/CODE_OF_CONDUCT.md).
Report vulnerabilities via the
[security policy](https://github.com/cplieger/.github/blob/main/SECURITY.md),
never in a public issue.

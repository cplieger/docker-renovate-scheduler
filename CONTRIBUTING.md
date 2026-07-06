# Contributing to docker-renovate-scheduler

A small Go scheduler that wraps the official
[Renovate](https://github.com/renovatebot/renovate) bot image and runs it on a
schedule (or on an external trigger) as a resident container. This guide covers
what the
[org-wide defaults](https://github.com/cplieger/.github/blob/main/CONTRIBUTING.md)
don't: why this image is **not** distroless, the package layout, the guardrails
to preserve, and how to run the checks locally.

## Why this image wraps `renovate/renovate` (not distroless)

The [README's "Not distroless" section](README.md#not-distroless--on-purpose)
covers _why_ the final stage is `FROM renovate/renovate:<version>` (Renovate
installs toolchains at runtime via containerbase, so there is no static form to
drop on `scratch`). The contributor-facing consequence: every run must inherit
the containerbase environment (PATH, `CONTAINERBASE_*`, `USER_*`) that the
upstream `renovate-entrypoint.sh` sets up, so the scheduler **execs renovate
through that entrypoint** rather than invoking the `renovate` binary directly
(see `renovateInvocation` in `renovate.go`). Do not "simplify" this to a bare
`exec renovate`; tool installation breaks without the entrypoint's env.

The Dockerfile strips the bundled `docker` CLI from that base
(`/opt/containerbase/tools/docker` plus its shim and PATH symlink); the README
carries the user-facing rationale. That removal was verified safe against
Renovate's exec layer (`lib/util/exec/`): every `docker` invocation is gated on
`binarySource === 'docker'`, and the docker datasource resolves digests over
HTTP, not via the CLI.

## Layout

Go module `github.com/cplieger/docker-renovate-scheduler`; binary
`docker-renovate-scheduler`. Flat package (mirrors `docker-rsync-scheduler`):

- `main.go` — subcommand dispatch (`daemon` / `run` / `health`) and the
  composition roots. `runBuiltin` drives the `time.Ticker` loop; `runExternal`
  idles until SIGTERM (external-trigger mode); the `run` subcommand (`runRun`)
  performs a single pass. `runBuiltin` and `runRun` both funnel through
  `runRenovatePass`; `runExternal` runs no pass itself -- it only drains an
  externally-triggered `run` on shutdown. Shutdown is driven by
  `signal.NotifyContext`.
- `config.go` — env loading. `loadInterval` parses `SCHED_INTERVAL` with the
  `off` / `disabled` / `0` / `0s` sentinels (→ external mode); `loadRunTimeout`
  parses `SCHED_TIMEOUT` (default 1h); plus `setupLogger`, `getEnv`, and the
  base-dir verification.
- `renovate.go` — `renovateInvocation` builds the command that routes through
  `/usr/local/sbin/renovate-entrypoint.sh`; `runRenovatePass` is the
  flock-guarded coalescing loop (queue-on-overlap → rerun-on-completion,
  bounded by `maxCoalescedReruns`) and `runRenovateOnce` is a single pass.
  `defaultCommandRunner` carries the gosec `#nosec G702` annotation on the
  `exec.CommandContext` call.
- `lock.go` — `flock(2)` overlap guard (proven, from the rsync sibling) so a
  built-in tick never overlaps an external `run` invocation, plus the
  single-slot rerun-coalescing flag primitives (`markRerunPending` /
  `rerunPending` / `clearRerunPending`).
- `health.go` — thin wrapper over `github.com/cplieger/health` (file marker).

## Env-var convention (don't collide with Renovate)

Scheduler knobs use the **`SCHED_*`** prefix (`SCHED_INTERVAL`,
`SCHED_TIMEOUT`), never `RENOVATE_*` — the latter is Renovate's own config
namespace and the bot would try to interpret our knobs as config. `LOG_LEVEL`
is intentionally shared. Renovate itself is configured the normal way
(`config.js` / `RENOVATE_*` env / each repo's `renovate.json`); the scheduler
never parses or rewrites Renovate config.

## Guardrails (don't weaken)

- Runs as the renovate image's non-root user (UID `12021`); `RENOVATE_BASE_DIR`
  is `/data` (writable). No network listener, no exposed ports. Overriding the
  UID is an operator choice with a caveat — a custom UID has no writable home, so
  each language cache must be redirected to a writable volume and forwarded via
  `RENOVATE_CUSTOM_ENV_VARIABLES` (see "Running as a non-default user" in the
  README); don't try to paper over that inside the image.
- Every renovate run is serialized by the `flock` in `lock.go` — the built-in
  ticker and an external `docker exec … run` can both fire, and the lock is what
  stops them overlapping. A trigger that loses the lock sets a single-slot rerun
  flag instead of being dropped, and the holder reruns once on completion if
  it's set (bounded by `maxCoalescedReruns`, and a failed pass stops the loop).
  Keep both the lock and the coalescing.
- `exec.CommandContext` arg lists only — never build a `sh -c` string from env.
- The run timeout (`SCHED_TIMEOUT`) is propagated via context so a wedged
  renovate run is killed, not left running into the next tick.
- The bundled `docker` CLI is removed in the Dockerfile (`binarySource=install`
  never invokes it). Don't re-add it or switch to `binarySource=docker`; if a
  feature ever seems to need it, re-check Renovate's exec layer first (every
  `docker` call is gated on `binarySource === 'docker'`, which is deprecated
  upstream).

## Scheduling modes

- **Built-in** (default): `SCHED_INTERVAL=<Go duration>` (e.g. `1h`) → a
  `time.Ticker` loop, first run at start.
- **External**: `SCHED_INTERVAL=off` (or `disabled` / `0` / `0s`) → the
  container idles and an external scheduler triggers
  `docker exec renovate docker-renovate-scheduler run` (e.g. Ofelia on a fixed
  cadence, or a webhook-driven action for ad-hoc runs).

## Conventions and gotchas

- Logs are slog logfmt to stderr (`key=value`); always use key/value pairs,
  never a formatted string (the `sloglint` linter enforces it).
- `main()` orchestration and the `exec.CommandContext` call to renovate are
  intentionally not unit-tested (process-level I/O, validated by container logs
  and Grafana alerting). New logic in `config.go` / `renovate.go` / `lock.go`
  is expected to come with tests.
- Tests are table-driven + property-based
  ([rapid](https://github.com/flyingmutant/rapid)) and live beside the code
  (`*_test.go`). They cover the interval sentinels, timeout parsing, the
  entrypoint-routed invocation builder, the flock mutual-exclusion, and the
  rerun coalescing (queue-on-overlap, bounded reruns, no rerun on failure).

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
[security policy](https://github.com/cplieger/.github/blob/main/SECURITY.md) —
never in a public issue.

# docker-renovate-scheduler

[![Image Size](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/cplieger/docker-renovate-scheduler/badges/size.json)](https://github.com/cplieger/docker-renovate-scheduler/pkgs/container/docker-renovate-scheduler)
![Platforms](https://img.shields.io/badge/platforms-amd64%20%7C%20arm64-blue)
![base: renovate/renovate](https://img.shields.io/badge/base-renovate%2Frenovate-1A1F6C)
[![Test coverage](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/cplieger/docker-renovate-scheduler/badges/coverage.json)](https://github.com/cplieger/docker-renovate-scheduler/actions/workflows/coverage.yml)
[![Mutation](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/cplieger/docker-renovate-scheduler/badges/mutation.json)](https://github.com/cplieger/docker-renovate-scheduler/issues?q=label%3Agremlins-tracker)
[![OpenSSF Best Practices](https://www.bestpractices.dev/projects/13208/badge)](https://www.bestpractices.dev/projects/13208)
[![OpenSSF Scorecard](https://api.scorecard.dev/projects/github.com/cplieger/docker-renovate-scheduler/badge)](https://scorecard.dev/viewer/?uri=github.com/cplieger/docker-renovate-scheduler)
[![SBOM](https://img.shields.io/badge/SBOM-SPDX-1D4ED8)](https://github.com/cplieger/docker-renovate-scheduler/releases)

<!-- hub-overview BEGIN -->
Run [Renovate](https://github.com/renovatebot/renovate) as a resident, always-on container instead of a one-shot job, driven by a built-in interval scheduler **or** an external trigger. A tiny Go wrapper around the official `renovate/renovate` image; structured logs, no metrics, no open ports.

## Why this exists

The Renovate CLI is designed to run once and exit; the usual self-hosted patterns schedule it with `cron` or a Kubernetes `CronJob`. If your stack schedules every other workload with always-on containers and external triggers (Ofelia, a webhook, a central orchestrator), an ephemeral `docker run` per cycle is the odd one out. This image keeps Renovate **resident** and lets you drive it the same way as everything else, while reusing the upstream image's runtime.

It deliberately has **no built-in HTTP/webhook server**; triggering is delegated to whatever already schedules your containers (`docker exec`, Ofelia, a webhook-driven action on a release, …).

### Not distroless, on purpose

Renovate is a Node.js application that shells out to `git` and, for lockfile maintenance, to language package managers; its default `binarySource=install` installs those toolchains at runtime via [containerbase](https://github.com/containerbase/base). There is no static, dependency-free form to drop on `scratch`. So this image **builds on the official `renovate/renovate` image** (the default image, which Renovate recommends for most users and which installs tools at runtime) and adds only the scheduler binary on top.

One deliberate trim: the bundled `docker` CLI is removed. Renovate invokes it only under `binarySource=docker`, which is [deprecated upstream](https://github.com/renovatebot/renovate/discussions/40742) and **not supported by this image**; dropping the unused ~42 MB binary cuts attack surface and the CVE noise image scanners report against it.

## What it does

- Runs `renovate` on a **built-in interval** (`RUN_INTERVAL=6h`): one run at startup when no successful run completed within the interval (the last-run record on `/data` survives restarts), then every interval. Or set `RUN_INTERVAL=off` and trigger each run **externally** (`docker exec … run`).
- **The daemon owns every run.** It executes Renovate as its own child process, whichever trigger asked. The `run` subcommand is a thin client that submits the request over an in-container unix socket, forwarding its repo arguments **and its environment** (a `docker exec -e RENOVATE_X=… … run` override reaches Renovate intact), and exits with that run's true result.
- **One run at a time, every request served.** Requests queue in order behind an in-flight run; each gets its own run and its own exit code. See [One run at a time](#one-run-at-a-time-queueing).
- File-marker healthcheck via [`github.com/cplieger/health`](https://github.com/cplieger/health): unhealthy when the last run failed, recovers on the next clean run.
- Streams Renovate's own structured logs straight through to the container's stdout/stderr (set `LOG_FORMAT=json`) for collection by Alloy/Promtail/Loki, **in both scheduling modes**. The scheduler neither captures nor parses Renovate's output; it emits only its own lifecycle lines, with UTC timestamps regardless of the container's `TZ`.
<!-- hub-overview END -->

## Configuration reference

Renovate reads **its entire configuration from its own** `RENOVATE_*` environment variables, a `config.js`, or a config file (see [`config.js.example`](config.js.example)); this scheduler does not wrap or re-expose any of it. The scheduler itself is configured by the variables below, all kept **outside** the `RENOVATE_*` namespace so Renovate cannot mistake them for config options:

| Variable | Description | Default |
| --- | --- | --- |
| `RUN_INTERVAL` | Built-in scheduler cadence as a Go duration (`6h`, `1h`, `30m`). A run at startup only when no successful run completed within the interval, then every interval. Set to `off` (aliases `disabled`, `0`) to disable the built-in scheduler and trigger runs externally (see [Scheduling modes](#scheduling-modes)). Falls back to `6h` on an unset, negative, or unparseable (non-sentinel) value. | `6h` |
| `RUN_TIMEOUT` | Whole-run timeout for a single `renovate` invocation, as a Go duration. This is the outer bound on the process; Renovate's own `RENOVATE_EXECUTION_TIMEOUT` is a separate per-child limit. | `1h` |
| `LOG_LEVEL` | `debug`, `info`, `warn`, and `error` are honoured by both the scheduler and Renovate. Renovate also accepts `trace` and `fatal`, which the scheduler reports as unrecognized and replaces with `info` for its own lines only. Any other value, including the long form `warning` and slog offset forms such as `warn+1`, makes Renovate refuse to start, which in built-in mode means the healthcheck never goes healthy. | `info` |

Everything else is Renovate's own configuration. The essentials for a self-hosted bot:

- `RENOVATE_TOKEN`: platform token for the bot account (and `RENOVATE_GITHUB_COM_TOKEN` for changelog fetching when running against non-github.com platforms or to avoid rate limits).
- `RENOVATE_AUTODISCOVER=true` **or** `RENOVATE_REPOSITORIES`: which repositories to process.
- `RENOVATE_PERSIST_REPO_DATA=true` and `RENOVATE_REPOSITORY_CACHE=enabled`: recommended for a resident container so runs `git fetch` instead of `git clone` and reuse extraction/datasource caches across runs (the payoff of staying always-on; persist `/data`).

## Running as a non-default user (rootless)

> **Recommended: don't override the user.** Run the image as-is; it works with no extra configuration.

By default the container runs as the base image's non-root user, UID `12021`, which has a writable home and a working [containerbase](https://github.com/containerbase/base): Renovate installs toolchains on demand and regenerates lockfiles out of the box.

If you override the user (Compose `user:`) to match host volume ownership (e.g. a `1000:1000` rootless UID), that UID has **no home directory** (`HOME=/`), so every tool cache that defaults under `$HOME` becomes unwritable and two things break, and neither failure surfaces here:

- containerbase's on-demand tool installs fail (`binarySource=install` can't write `/opt/containerbase`); and
- lockfile/artifact regeneration fails: `go mod tidy` can't refresh `go.sum`, `npm install` can't refresh `package-lock.json`. The dependency PR is still raised, but manifest-only (`go.mod` / `package.json`), and then fails the consuming repo's CI (`missing go.sum entry`, or `npm ci` reporting the lock out of sync).

Renovate reports that second failure on the PR itself, as a red `renovate/artifacts` status check; its own log line for it is at `debug`, and this image documents `LOG_LEVEL=info`, so the container stream does not carry it.

The scheduler **logs a startup warning** when it runs as a non-default UID and `RENOVATE_CUSTOM_ENV_VARIABLES` names no cache or toolchain-path variable, so the misconfiguration surfaces at boot instead of as a broken PR days later. The check is name-based: it verifies you engaged the mitigation (a cache variable is named), not that its value is correct; an empty or mistyped path is your configuration to verify.

If you must run as a custom UID, use the tools baked into the image and route every cache to a writable, mounted volume:

```yaml
    user: "1000:1000"                      # your rootless UID
    environment:
      RENOVATE_BINARY_SOURCE: "global"     # use the baked tools; skip the on-demand installer
      GOPATH: "/data/go"
      GOCACHE: "/data/.cache/go-build"      # Go
      npm_config_cache: "/data/.npm"        # Node / npm
      # Renovate forwards only an allowlist to artifact subprocesses
      # (GOPATH yes; GOCACHE / npm_config_cache no), so forward them explicitly:
      RENOVATE_CUSTOM_ENV_VARIABLES: '{"GOPATH":"/data/go","GOCACHE":"/data/.cache/go-build","npm_config_cache":"/data/.npm"}'
    volumes:
      - ./data:/data                        # chown ./data to your UID on the host
```

Add one cache entry per language manager Renovate updates (the pattern extends to `pip`, `cargo`, etc.), and `chown` the `/data` volume to your UID. If that is more than you want to manage, run as the default `12021`.

## Memory and the package cache

> **Recommended for a resident container: set `RENOVATE_X_SQLITE_PACKAGE_CACHE=true`,** and size `mem_limit` at 3g or more.

**Node sizes its heap from the container memory limit, not from host RAM.** It reads the cgroup limit and caps V8's old space at roughly half of it. So `mem_limit` is the effective heap knob, however much memory the host has. Measured inside the container:

| `mem_limit` | V8 heap ceiling |
| --- | --- |
| 2 GiB | 1120 MB |
| 3 GiB | 1728 MB |

When a run crosses that ceiling, node aborts itself with `FATAL ERROR: Ineffective mark-compacts near heap limit`, and the scheduler reports `exit status 134`. The **kernel OOM killer never fires**. The cgroup's `memory.events` counters stay at zero and `docker inspect` reports `OOMKilled=false`, so every signal a memory limit usually leaves is absent. The scheduler names this cause on the failure line as `likely_cause` and `fix`, so the exit code is not your only evidence.

**The default file package cache raises the cost of every run as it grows.** Renovate collects that cache after each run, and the collection reads the whole content directory into memory in one allocation. Its cost thus tracks accumulated cache size, not your repository count. Measured on a resident deployment scanning 55 repositories hourly: in three months the content directory reached 3.1 million files (44 GB) plus a 1.3 GB index, and the collection allocated 800 MB in one block. Past the heap ceiling it can no longer complete. Nothing is reclaimed after that point, so the cache only grows and the failure never recovers on its own.

`RENOVATE_X_SQLITE_PACKAGE_CACHE=true` ([Renovate's experimental variables](https://docs.renovatebot.com/self-hosted-experimental/)) replaces that backend with SQLite. Its cleanup is one indexed `DELETE`, with no directory walk and no orphaned content. On the same deployment the cache went from 45 GB to 4.7 MB. Runs went from failing at 131-180 s to finishing in 52-77 s, faster than before the cache grew. A content directory of that size also keeps hundreds of MB of kernel directory-entry cache charged to the cgroup, which node cannot see when it picks its ceiling; that charge goes away with the directory walk.

**The dependency work finishes before the crash.** The collection runs after every repository is processed, so Renovate raises its pull requests and then dies. Updates land while the run reports failure, so a non-zero exit here is not evidence that nothing happened.

## Scheduling modes

### Built-in scheduler (default)

Set `RUN_INTERVAL` to a Go duration. The container runs once at startup when that run is due, and then every interval. Zero external dependencies.

The startup run is due when no successful scheduled run completed within the interval. The daemon records each scheduled run and its outcome in a small file, `.docker-renovate-scheduler-last-run`, in `RENOVATE_BASE_DIR` (default `/data`); with `/data` persisted the record survives a container recreate (an image update, a config change), so recreates do not cause redundant back-to-back runs. The record also carries the schedule's phase: the next run lands one `RUN_INTERVAL` after the previous run, not one interval after boot, so a restart neither adds a run nor delays the cadence. Only scheduled runs write the record; triggered runs never do. A failed last run does not count: the startup run fires again, so a fixed configuration (a corrected `RENOVATE_TOKEN`, say) shows its effect at the next recreate instead of one interval later. Without a persisted `/data` the record never survives, and the container runs at every startup. To force a run at any time: `docker exec <container> docker-renovate-scheduler run`.

```yaml
services:
  renovate:
    image: ghcr.io/cplieger/docker-renovate-scheduler:latest
    container_name: renovate
    restart: unless-stopped
    environment:
      RUN_INTERVAL: "6h"
      LOG_LEVEL: "info"
      LOG_FORMAT: "json"
      RENOVATE_PLATFORM: "github"
      RENOVATE_AUTODISCOVER: "true"
      RENOVATE_TOKEN: "<bot-token>"
      RENOVATE_PERSIST_REPO_DATA: "true"
      RENOVATE_REPOSITORY_CACHE: "enabled"
    volumes:
      - ./data:/data            # persist clones + caches (see "Volumes" for chown)
```

### External scheduler

Set `RUN_INTERVAL=off`. The container stays running but idle; trigger each run out-of-band:

```bash
docker exec renovate docker-renovate-scheduler run            # all configured repos
docker exec renovate docker-renovate-scheduler run owner/repo # just one (positional args go straight to Renovate)
```

The `run` command submits the request to the daemon and blocks until that run completes, exiting 0 on success and 1 on failure (the run's own result, even when it waited its turn behind an in-flight pass). If you interrupt that wait, a run the daemon already accepted continues there; an interrupt before acceptance leaves the outcome unknown to the client. Either way the client exits 1 with a warning. Exit 1 there means the outcome is unknown to the client, not that the run failed. Because the daemon executes the run, its full Renovate output lands on the **container's** log stream in this mode too; the trigger's log (an Ofelia job log, a webhook action's output) shows only the `run` command's lifecycle lines (`triggered run accepted` / `started` / `complete`). Read per-run detail from `docker logs` / Loki; read the outcome from the exit code.

Environment overrides ride along: `docker exec -e RENOVATE_AUTODISCOVER=false renovate docker-renovate-scheduler run owner/repo` forwards the exec's environment with the request, and the daemon starts that run's Renovate child with that environment.

Example with [Ofelia](https://github.com/mcuadros/ofelia):

```yaml
    environment:
      RUN_INTERVAL: "off"     # disable the built-in loop; Ofelia drives it
    labels:
      ofelia.enabled: "true"
      ofelia.job-exec.renovate-run.schedule: "@every 6h"
      ofelia.job-exec.renovate-run.command: "docker-renovate-scheduler run"
      ofelia.job-exec.renovate-run.user: "12021"   # MUST match the container's user; see below
      ofelia.job-exec.renovate-run.no-overlap: "true"
```

> **Run the trigger as the same user the container runs as.** The daemon's
> trigger socket lives in `/tmp`, owner-only (`0600`), owned by whoever the
> container runs as: the image's default `12021`, or whatever you set via
> Compose `user:`. A bare `docker exec` inherits the container's user
> automatically, but Ofelia's `job-exec` does **not**: it runs as the image's
> default user unless you set `user:` explicitly. A mismatched trigger user
> fails immediately and loudly at connect (`cannot reach the scheduler
> daemon … permission denied`). So set Ofelia's `user:` to match your Compose
> `user:`, e.g. `"1000"` if you run the container rootless as `1000:1000`,
> or leave the default `12021` if you don't override the user.

The `docker exec` trigger needs no entrypoint prefix: the daemon routes each Renovate child through the image entrypoint internally, so every run gets the full containerbase environment.

#### One run at a time (queueing)

Two Renovate processes never run against the same base directory: the daemon executes requests **strictly one at a time, in arrival order**. A trigger that lands while a run is in flight is not dropped and not merged; it waits its turn, runs **exactly what it asked** (its repos, its environment), and its `run` command exits with that run's own result. A burst of triggers (e.g. `release` webhooks firing an external action) is served back-to-back after the in-flight pass; runs are idempotent, so a burst costs only time.

The queue is bounded (16 pending); a trigger arriving on a full queue is rejected immediately with exit 1 and a clear reason. Ofelia's `no-overlap` still prevents redundant _triggers_ from stacking up on the scheduler side.

## Graceful shutdown

On `SIGTERM`/`SIGINT` (a `docker stop`, or a redeploy that recreates the container) the scheduler does not abandon an in-flight run; every run is the daemon's own child, so shutdown is ordinary draining:

- The **in-flight run** completes with its real outcome (bounded by its own `RUN_TIMEOUT`); its waiting trigger still receives the true exit code.
- **Queued requests are cancelled explicitly**: each waiting `run` command receives a "scheduler shutting down" result and exits 1, so the trigger reports a failed job instead of hanging or being silently dropped. No new requests are accepted.

Docker terminates the container once the process exits **or** `stop_grace_period` elapses, whichever comes first. Set `stop_grace_period` long enough to cover your **slowest** run; a cold first run (empty `./data` + on-demand tool installs) can take as long as the 10m healthcheck `start_period`. A shorter grace `SIGKILL`s the run before the drain completes:

```yaml
services:
  renovate:
    stop_grace_period: 10m  # >= your slowest run
```

The drain is internally capped at `RUN_TIMEOUT` (a run can't outlast its own timeout); `stop_grace_period` is the real outer bound.

## Subcommands

| Command | Purpose |
| --- | --- |
| `daemon` (default) | Owns every Renovate run, serves the trigger socket, and drives the built-in interval when `RUN_INTERVAL` is a duration. |
| `run [repo …]` | Submit one run to the daemon and wait for it; exit 0/1 is the run's own result. The external-trigger entry point; extra args pass through to Renovate as repository slugs, and the exec's environment is forwarded to the run. |
| `health` | The Docker healthcheck probe (stats the marker file). |

## Volumes

| Mount | Description |
| --- | --- |
| `/data` | `RENOVATE_BASE_DIR`: repository clones, caches, dynamically installed tools, and the scheduler's last-run record (see [Scheduling modes](#scheduling-modes)). Persist it. For an `./data` bind mount, create the directory first and run `chown 12021:0 ./data` (a fresh auto-created root-owned dir fails the non-root daemon's boot write check). |
| `/usr/src/app/config.js` | Optional: a Renovate `config.js` if you prefer it over `RENOVATE_*` env vars. |

## Alerting

docker-renovate-scheduler has no metrics endpoint; its operational state is in its logs. The scheduler emits its own lifecycle lines as structured `slog` logfmt to the container log (`level=INFO msg="renovate run complete"` on success; `level=ERROR msg="renovate run failed"` or `msg="renovate run timed out"` on failure). Ship the container's logs to Loki (Grafana Alloy's Docker log discovery does this with no configuration) and evaluate these with [Loki's ruler](https://grafana.com/docs/loki/latest/alert/); firing alerts deliver through your Alertmanager exactly like Prometheus metric alerts.

These rules work in **both scheduling modes**: every run, interval-fired or externally triggered, executes inside the daemon, so its lifecycle lines and Renovate's own output always reach the container log. In external-trigger mode your trigger additionally sees each run's exit code, so trigger-side job alerting works as a second, independent signal.

```yaml
groups:
  - name: docker-renovate-scheduler
    rules:
      - alert: RenovateRunFailed
        expr: |
          sum by (container) (count_over_time(
            {container="renovate"} |= `level=ERROR` [15m]
          )) > 0
        for: 0m
        labels:
          severity: warning
        annotations:
          summary: "renovate: a scheduled run failed"
          description: >
            The scheduler logged an error: a run that exited non-zero
            (`renovate run failed`), a run that hit RUN_TIMEOUT
            (`renovate run timed out`), a base-directory error, or a
            containment halt (`halting run admission: renovate run process
            group survived the kill sweep`; a run's
            process tree could not be confirmed dead, so the daemon stops
            admitting runs and exits non-zero; the container restart reaps
            the surviving tree). No dependency PRs are raised until the
            next clean run. Check the container logs, RENOVATE_TOKEN, and
            platform reachability. A graceful shutdown drains the in-flight
            run and logs no error, so a redeploy does not trip this.
      - alert: RenovateNoRecentRun
        expr: |
          absent_over_time({container="renovate"} |= `renovate run complete` [13h])
        for: 30m
        labels:
          severity: warning
        annotations:
          summary: "renovate has not completed a run in 13h"
          description: >
            The scheduler logs `renovate run complete` after every run that
            completes, in both modes (built-in: at startup when due, then
            every RUN_INTERVAL, default 6h; external: per trigger). One case
            suppresses it on purpose: a run that exits zero whose process
            tree cannot be confirmed dead halts admission and logs at ERROR,
            which `RenovateRunFailed` catches. Otherwise, none in 13h while
            the container is up means the schedule is wedged or the triggers
            stopped arriving, and no dependency PRs are being raised.
            Restart the container (or check the trigger source). The 13h
            window covers the longest legal quiet stretch plus margin: the
            schedule keeps its phase across restarts (the record on /data
            carries it), so completion lines sit at most one RUN_INTERVAL
            plus the 1h RUN_TIMEOUT apart (7h at the 6h default). Adjust it
            to your cadence; it must exceed RUN_INTERVAL + RUN_TIMEOUT.
```

Thresholds and the `severity` label are starting points; adjust the deadman window to your `RUN_INTERVAL` and the `container` selector (or `job` / `service`, depending on your log collector) to your deployment, and route by whatever labels your Alertmanager uses.

One case makes `RenovateRunFailed` misleading on its own, so read the failure line before acting on it. A run that exhausts node's heap dies after its repositories are processed, so dependency updates are landing normally while the alert fires. The scheduler tags that failure with `likely_cause` and `fix` attributes; [Memory and the package cache](#memory-and-the-package-cache) has the remedy.

## Healthcheck

`docker-renovate-scheduler health` checks a marker file the daemon sets after each run. In **built-in** mode the container starts unhealthy and flips to healthy after the first successful run (size `healthcheck.start_period` for the time a first run may take); when a fresh successful run's record survives on `/data`, the startup run is skipped and the container starts healthy instead (see [Scheduling modes](#scheduling-modes)). A failed run flips it unhealthy, and it recovers on the next clean run. Built-in mode additionally treats a stale marker as unhealthy: if no run has refreshed it within `2*RUN_INTERVAL + RUN_TIMEOUT`, the probe fails, so a wedged interval loop surfaces as an unhealthy container instead of a silently idle one. In **external** mode the container starts healthy (idle, nothing has failed), each triggered run updates the marker, and no staleness deadline applies (an idle container between sparse triggers stays healthy).

The image bakes a 10m `start_period`, sized to a cold first run that installs its toolchains on demand. Docker reports the container healthy as soon as a probe succeeds inside that window, so a warm `/data` is not penalised. The trade runs the other way: a boot that is genuinely broken reports `starting` for up to the start period before it turns unhealthy. To detect a dead boot sooner, set a timing-only `healthcheck:` block in your own compose file (`test`, `interval`, `timeout`, and `retries` are inherited from the image).

## Security

No network listener, no HTTP server, no exposed ports: triggering happens over an **in-container unix socket** in `/tmp`, owner-only (`0600`), so trigger authority is scoped to the container's own user (the same boundary `docker exec` already enforces; a mismatched trigger user fails loudly at connect). The unused `docker` CLI is stripped from the base image, removing that container-execution surface (see [Not distroless, on purpose](#not-distroless-on-purpose)). Runs as the base image's non-root user (UID `12021`) by default, or whatever you set via Compose `user:`; the socket and health marker are owned by that user, so external run triggers must execute as it (see [Scheduling modes](#scheduling-modes)). The daemon executes Renovate via the image entrypoint with an explicit argument slice (no shell); a triggered run's forwarded environment crosses only that same-user socket, no broader boundary than the exec that carried it. Renovate's token is never logged by the scheduler. The base image is Renovate's own (AGPL-3.0); the scheduler wrapper is Apache-2.0.

## Dependencies

All dependencies are updated automatically via [Renovate](https://github.com/renovatebot/renovate) and pinned by digest or version for reproducibility.

| Dependency | Source |
| --- | --- |
| renovate/renovate | [Docker Hub](https://hub.docker.com/r/renovate/renovate) (the runtime base) |
| golang | [Go](https://hub.docker.com/_/golang) (builder stage only) |
| [`github.com/cplieger/atomicfile`](https://github.com/cplieger/atomicfile) | base-directory write probe |
| [`github.com/cplieger/envx`](https://github.com/cplieger/envx) | environment variable parsing |
| [`github.com/cplieger/health`](https://github.com/cplieger/health) | file-marker healthcheck |
| [`github.com/cplieger/scheduler`](https://github.com/cplieger/scheduler) | interval parsing, run loop, graceful command runner, unix-socket trigger broker |
| [`github.com/cplieger/slogx`](https://github.com/cplieger/slogx) | slog setup (UTC logfmt) |

## Credits

This image packages [Renovate](https://github.com/renovatebot/renovate) by [Mend.io](https://www.mend.io/) (AGPL-3.0). All credit for the dependency-update engine goes to its upstream maintainers; this project only adds a scheduling wrapper.

## Contributing

Issues and PRs are welcome. See [CONTRIBUTING.md](CONTRIBUTING.md) for the
conventions and how to run the checks locally.

## Disclaimer

This project is built with care and follows security best practices, but it is intended for personal / self-hosted use. No guarantees of fitness for production environments. Use at your own risk.

This project was built with AI-assisted tooling using [Claude](https://claude.com), [GPT](https://openai.com), and [Kiro](https://kiro.dev). The human maintainer defines architecture, supervises implementation, and makes all final decisions.

## License

Apache-2.0. See [LICENSE](LICENSE).

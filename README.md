# docker-renovate-scheduler

[![CI](https://github.com/cplieger/docker-renovate-scheduler/actions/workflows/ci.yaml/badge.svg)](https://github.com/cplieger/docker-renovate-scheduler/actions/workflows/ci.yaml)
[![GitHub release](https://img.shields.io/github/v/release/cplieger/docker-renovate-scheduler)](https://github.com/cplieger/docker-renovate-scheduler/releases)
[![Image Size](https://ghcr-badge.egpl.dev/cplieger/docker-renovate-scheduler/size)](https://github.com/cplieger/docker-renovate-scheduler/pkgs/container/docker-renovate-scheduler)
![Platforms](https://img.shields.io/badge/platforms-amd64%20%7C%20arm64-blue)
![base: renovate/renovate](https://img.shields.io/badge/base-renovate%2Frenovate-1A1F6C)
[![OpenSSF Scorecard](https://api.scorecard.dev/projects/github.com/cplieger/docker-renovate-scheduler/badge)](https://scorecard.dev/viewer/?uri=github.com/cplieger/docker-renovate-scheduler)

Run [Renovate](https://github.com/renovatebot/renovate) as a resident, always-on container — driven by a built-in interval scheduler **or** an external trigger — instead of a one-shot job. A tiny Go wrapper around the official `renovate/renovate` image; structured logs, no metrics, no open ports.

## Why this exists

The Renovate CLI is designed to run once and exit; the usual self-hosted patterns schedule it with `cron` or a Kubernetes `CronJob`. If your stack schedules every other workload with always-on containers and external triggers (Ofelia, a webhook, a central orchestrator), an ephemeral `docker run` per cycle is the odd one out. This image keeps Renovate **resident** and lets you drive it the same way as everything else, while reusing the upstream image's carefully-assembled runtime.

It deliberately has **no built-in HTTP/webhook server** — triggering is delegated to whatever already schedules your containers (`docker exec`, Ofelia, a Komodo action on a release webhook, …).

### Not distroless — on purpose

Unlike a single-static-binary tool, Renovate is a Node.js application that shells out to `git` and, for lockfile maintenance, to language package managers; its default `binarySource=install` installs those toolchains at runtime via [containerbase](https://github.com/containerbase/base). There is no static, dependency-free form to drop on `scratch`. So this image **builds on the official `renovate/renovate` image** (the "full" image the Renovate docs recommend when runtime tool installation is needed) and adds only the scheduler binary on top. The Go wrapper itself is a static, CGO-free binary.

## What it does

- Runs `renovate` on a **built-in interval** (`SCHED_INTERVAL=6h`) — one run at startup for immediate freshness, then every interval — **or** stays idle and runs on an **external trigger** (`SCHED_INTERVAL=off` + `docker exec … run`).
- Routes every Renovate invocation through the image's own entrypoint so the containerbase environment (`binarySource=install`) is set up — even for runs triggered by a bare `docker exec`, which bypasses the image `ENTRYPOINT`.
- Overlap-guards runs with an advisory `flock`, so a scheduled run and a manually/externally triggered run never execute two Renovate processes against the same base directory at once. A trigger that arrives while a run is in flight isn't dropped — it queues a single coalesced rerun ("max 1 wait") that fires as soon as the current run finishes.
- File-marker healthcheck via [`github.com/cplieger/health`](https://github.com/cplieger/health): unhealthy when the last run failed, recovers on the next clean run.
- Streams Renovate's own structured logs straight through to stdout/stderr (set `LOG_FORMAT=json`) for collection by Alloy/Promtail/Loki. The scheduler neither captures nor parses Renovate's output; it emits only its own lifecycle lines.

## Configuration

Renovate reads **its entire configuration from its own** `RENOVATE_*` environment variables, a `config.js`, or a config file (see [`config.js.example`](config.js.example)) — this scheduler does not wrap or re-expose any of it. The scheduler itself is configured by the variables below, all kept **outside** the `RENOVATE_*` namespace so Renovate cannot mistake them for config options:

| Variable | Description | Default |
|----------|-------------|---------|
| `SCHED_INTERVAL` | Built-in scheduler cadence as a Go duration (`6h`, `1h`, `30m`). First run at startup, then every interval. Set to `off` (aliases `disabled`, `0`) to disable the built-in scheduler and trigger runs externally — see [Scheduling modes](#scheduling-modes). Falls back to `6h` on an unset or unparseable (non-sentinel) value. | `6h` |
| `SCHED_TIMEOUT` | Whole-run timeout for a single `renovate` invocation, as a Go duration. This is the outer bound on the process; Renovate's own `RENOVATE_EXECUTION_TIMEOUT` is a separate per-child limit. | `1h` |
| `LOG_LEVEL` | `debug`, `info`, `warn`, or `error` (honoured by both the scheduler and Renovate). | `info` |

Everything else is Renovate's own configuration. The essentials for a self-hosted bot:

- `RENOVATE_TOKEN` — platform token for the bot account (and `RENOVATE_GITHUB_COM_TOKEN` for changelog fetching when running against non-github.com platforms or to avoid rate limits).
- `RENOVATE_AUTODISCOVER=true` **or** `RENOVATE_REPOSITORIES` — which repositories to process.
- `RENOVATE_PERSIST_REPO_DATA=true` and `RENOVATE_REPOSITORY_CACHE=enabled` — recommended for a resident container so runs `git fetch` instead of `git clone` and reuse extraction/datasource caches across runs (the payoff of staying always-on; persist `/data`).

## Scheduling modes

### Built-in scheduler (default)

Set `SCHED_INTERVAL` to a Go duration. The container runs once at startup and then every interval. Zero external dependencies.

```yaml
services:
  renovate:
    image: ghcr.io/cplieger/docker-renovate-scheduler:latest
    container_name: renovate
    restart: unless-stopped
    environment:
      SCHED_INTERVAL: "6h"
      LOG_LEVEL: "info"
      LOG_FORMAT: "json"
      RENOVATE_PLATFORM: "github"
      RENOVATE_AUTODISCOVER: "true"
      RENOVATE_TOKEN: "<bot-token>"
      RENOVATE_PERSIST_REPO_DATA: "true"
      RENOVATE_REPOSITORY_CACHE: "enabled"
    volumes:
      - ./data:/data            # RENOVATE_BASE_DIR — persist clones + caches
```

### External scheduler

Set `SCHED_INTERVAL=off`. The container stays running but idle; trigger each run out-of-band:

```bash
docker exec renovate docker-renovate-scheduler run            # all configured repos
docker exec renovate docker-renovate-scheduler run owner/repo # just one (positional args go straight to Renovate)
```

The run exits 0 on success, 1 on failure, and updates the same health marker the long-running container reports. Example with [Ofelia](https://github.com/mcuadros/ofelia):

```yaml
    environment:
      SCHED_INTERVAL: "off"     # disable the built-in loop; Ofelia drives it
    labels:
      ofelia.enabled: "true"
      ofelia.job-exec.renovate-run.schedule: "@every 6h"
      ofelia.job-exec.renovate-run.command: "docker-renovate-scheduler run"
      ofelia.job-exec.renovate-run.user: "12021"   # MUST match the container's user — see below
      ofelia.job-exec.renovate-run.no-overlap: "true"
```

> **Run the trigger as the same user the container runs as.** The run-lock and
> health marker live in `/tmp`, owned by whoever the container runs as — the
> image's default `12021`, or whatever you set via Compose `user:`. A bare
> `docker exec` (and a Komodo `execute_terminal`) inherits the container's user
> automatically, but Ofelia's `job-exec` does **not**: it runs as the image's
> default user unless you set `user:` explicitly. If the trigger's user differs
> from the container's, every run fails with
> `cannot acquire run lock … permission denied`. So set Ofelia's `user:` to
> match your Compose `user:` — e.g. `"568"` if you run the container rootless
> as `568:568`, or leave the default `12021` if you don't override the user.

The `docker exec` trigger is clean — no entrypoint prefix needed. The scheduler routes Renovate through the image entrypoint internally, so a bare exec still gets the full containerbase environment.

#### Overlap & coalescing

A trigger that races an in-flight run does **not** run twice and is **not** lost. The loser sets a single-slot "rerun pending" flag — any number of overlapping triggers collapse into one ("max 1 wait") — and when the active run finishes it immediately reruns once to pick up the queued work, then settles. This matters for release-driven triggering (e.g. a burst of `release` webhooks firing a Komodo action): without coalescing, a trigger that lands mid-run would otherwise wait for the next interval. Reruns are bounded by a small internal cap so a relentless trigger source can't pin the lock, and a failed run stops the loop rather than hammering a broken Renovate. Ofelia's `no-overlap` still prevents redundant _triggers_ from queuing on the scheduler side.

## Subcommands

| Command | Purpose |
|---------|---------|
| `daemon` (default) | PID 1; dispatches built-in vs external based on `SCHED_INTERVAL`. |
| `run [repo …]` | One Renovate run, then exit (exit 0 on success, 1 on failure). The external-trigger entry point; extra args are passed through to Renovate as repository slugs. |
| `health` | The Docker healthcheck probe (stats the marker file). |

## Volumes

| Mount | Description |
|-------|-------------|
| `/data` | `RENOVATE_BASE_DIR` — repository clones, caches, and dynamically installed tools. Persist it (the image creates it owned by the image's non-root user). |
| `/usr/src/app/config.js` | Optional — a Renovate `config.js` if you prefer it over `RENOVATE_*` env vars. |

## Healthcheck

`docker-renovate-scheduler health` checks a marker file set after each run. In **built-in** mode the container starts unhealthy and flips to healthy after the first successful run (size `healthcheck.start_period` for the time a first run may take); a failed run flips it unhealthy, and it recovers on the next clean run. In **external** mode it starts healthy (idle, nothing has failed) and each triggered `run` updates the marker.

```dockerfile
HEALTHCHECK --interval=60s --timeout=5s --retries=3 --start-period=30s \
    CMD ["/usr/local/bin/docker-renovate-scheduler", "health"]
```

## Security

No network listener, no HTTP server, no exposed ports. Runs as the base image's non-root user (UID `12021`) by default, or whatever you set via Compose `user:` (e.g. `568:568` to match a rootless homelab UID); the `/tmp` run-lock and health marker are owned by that user, so external run triggers must execute as it (see [Scheduling modes](#scheduling-modes)). The scheduler executes Renovate via the image entrypoint with an explicit argument slice (no shell). Renovate's token is never logged by the scheduler. The base image is Renovate's own (AGPL-3.0); the scheduler wrapper is GPL-3.0.

## Dependencies

All dependencies are updated automatically via [Renovate](https://github.com/renovatebot/renovate) and pinned by digest or version for reproducibility.

| Dependency | Source |
|------------|--------|
| renovate/renovate | [Docker Hub](https://hub.docker.com/r/renovate/renovate) (the runtime base) |
| golang | [Go](https://hub.docker.com/_/golang) (builder stage only) |
| [`github.com/cplieger/health`](https://github.com/cplieger/health) | file-marker healthcheck |

## Credits

This image packages [Renovate](https://github.com/renovatebot/renovate) by [Mend.io](https://www.mend.io/) (AGPL-3.0). All credit for the dependency-update engine goes to its upstream maintainers; this project only adds a scheduling wrapper.

## Disclaimer

Built with care and following security best practices, but intended for **homelab use**. No guarantees of fitness for production. Use at your own risk.

This project was built with AI-assisted tooling using [Claude Opus](https://www.anthropic.com/claude) and [Kiro](https://kiro.dev). The human maintainer defines architecture, supervises implementation, and makes all final decisions.

## License

This project is licensed under the [GNU General Public License v3.0](LICENSE).

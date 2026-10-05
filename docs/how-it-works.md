# How docker-renovate-scheduler works

This page explains how the image is built, how runs are started and queued, what happens when the container stops and when it reports unhealthy. It is for readers who want to know why the container behaved as it did. The settings are in [Configuration](configuration.md).

## Built on Renovate's own image

Renovate is a Node.js application that runs `git` and, to update lockfiles, the package managers of each language. With its default `binarySource=install`, it installs those tools at run time through [containerbase](https://github.com/containerbase/base), so it cannot run from an empty base image. So this image starts from the official `renovate/renovate` image, the default variant Renovate recommends for most users. It adds the scheduler binary and a Go toolchain installed for every user.

The image removes the `docker` command-line tool that the base image carries. Renovate uses it only with `binarySource=docker`, which is [deprecated upstream](https://github.com/renovatebot/renovate/discussions/40742) and not supported by this image. Removing the unused 42 MB binary also removes the security findings that image scanners report against it.

## One process runs every Renovate run

The container's main process, the scheduler, starts every Renovate run as its own child process, whichever mode or request asked for it. The `run` command is a small client. It sends its repository names and its whole environment to the scheduler over a socket at `/tmp/docker-renovate-scheduler.sock`, then waits and exits with that run's result.

| Command | What it does |
| --- | --- |
| `daemon` | The default. Runs every Renovate run, listens on the socket and keeps the built-in schedule when `RUN_INTERVAL` is a duration |
| `run [repo ...]` | Asks for one run, waits, and exits 0 or 1 with its result. The request carries the environment and any repository names for Renovate |
| `health` | The healthcheck. It reads the health file |

Because the scheduler runs every run, Renovate's output and the scheduler's own lines reach the container log in both modes. The scheduler neither captures nor reads Renovate's output. Each run starts through the image's entrypoint, so it gets Renovate's tool environment even when the request came from a `docker exec`. The scheduled runs and every `run` request share one queue, served one at a time in arrival order. [Your own scheduler](configuration.md#your-own-scheduler) has the queue's rules.

## The schedule

With the built-in schedule, the scheduler records each scheduled run and its result in `.docker-renovate-scheduler-last-run` on `/data`. At start it runs Renovate when no successful scheduled run finished within one `RUN_INTERVAL`, and otherwise waits until one interval after the last run. A failed last run is due again at start. [The built-in schedule](configuration.md#the-built-in-schedule) has the details.

## Shutdown

When the container receives `SIGTERM` or `SIGINT`, from a `docker stop` or a recreate, the scheduler stops taking requests and lets the current run finish:

- The run in progress ends with its real result, within its own `RUN_TIMEOUT`. A `run` command waiting for it still receives that result.
- Each waiting request is cancelled. Its `run` command receives `cancelled: scheduler shutting down` and exits 1, so your scheduler reports a failed job instead of waiting forever.

Docker stops the container when the process exits or when `stop_grace_period` runs out, whichever comes first. Set `stop_grace_period` to cover your slowest run. A first run with an empty `data` folder is slower while Renovate installs its tools, so measure that one. A shorter grace period kills the run, and the scheduler with it, before the run finishes. The example `compose.yaml` uses 10 minutes as a starting point:

```yaml
services:
  renovate:
    stop_grace_period: 10m  # at least your slowest run
```

The wait never exceeds `RUN_TIMEOUT`, because a run cannot outlast its own timeout. `stop_grace_period` is the outer limit.

When a run leaves a process behind that the scheduler cannot stop, the scheduler logs `halting run admission` at ERROR, refuses further runs and exits non-zero. This happens whatever the run's result. The container restart then ends what was left running.

## Health

The healthcheck runs `docker-renovate-scheduler health`, which reads a file the scheduler updates after each run.

With the built-in schedule, the container starts unhealthy and turns healthy after the first successful run. When the record on `/data` shows a successful run within the last interval, the startup run is skipped and the container starts healthy. A failed run makes it unhealthy, and the next good run makes it healthy again. A health file that no run has updated within `2 x RUN_INTERVAL + RUN_TIMEOUT` also counts as unhealthy, so a stopped schedule shows as an unhealthy container instead of a quiet one. With the defaults that is 13 hours.

With your own scheduler, the container starts healthy, because nothing has failed yet. Each run updates the file, and no deadline applies, so a container that waits a long time between runs stays healthy.

A Renovate that fails before it reaches its first repository, for example after a broken install or a missing module in the base image, counts as a failed run. The scheduler logs `renovate run failed`, the `run` command exits 1 and the container turns unhealthy. A run that processes some repositories and fails on others is reported as failed too. During shutdown the container reports unhealthy.

The image sets a 10-minute start period, for a first run that installs its tools. Docker reports the container healthy as soon as one check passes inside that window, so a run with a warm `data` folder is not held back. A start that is truly broken shows `starting` for up to 10 minutes before it turns unhealthy. To see that sooner, add a `healthcheck:` block to your compose file that sets only a shorter `start_period`. The image keeps its own test, interval, timeout and retries.

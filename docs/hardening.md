# Security

This page covers what the container accepts, where it writes and what the image contains. It is for readers who want to know what the scheduler exposes before they run it.

## What the container accepts

The image opens no port and runs no web server unless `METRICS_LISTEN_ADDR` is set. That listener serves only `GET /metrics` and `GET /healthz`, answers any other path with 404 and any other method with 405, and has no login, so keep it on your network. It cuts off a client that takes over 10 seconds to send its request or 30 seconds to read the answer.

The only way to start a run from outside the scheduler is a `docker exec` of the `run` command, which talks to the scheduler through a socket at `/tmp/docker-renovate-scheduler.sock`. Only the container's own user can open that socket, and only from inside the container, which is the same boundary `docker exec` already enforces. A `run` from another user fails at once with `permission denied`.

The container runs as Renovate's non-root user, UID 12021, or as the user Compose `user:` sets. The socket belongs to that user, so `docker exec` runs must use it too. [Running as another user](configuration.md#running-as-another-user) explains what another user needs.

With [GitHub App authentication](configuration.md#github-app-authentication), the private key stays in its read-only file. The scheduler reads it once at start. Each installation token lives one hour, goes only into that run's environment, and is never logged. The signed request token is never logged either. When GitHub's error message repeats it, the scheduler replaces it with `[redacted]`.

The key file stays readable for as long as the container runs, by every process running as the container's user. That includes the commands Renovate runs after an update, if you allow any with `RENOVATE_ALLOWED_COMMANDS`. A personal token in `RENOVATE_TOKEN` has the same exposure, through the Renovate process's environment. The difference is that a key does not expire. If you think the key leaked, delete it on the App's settings page and generate a new one.

A `run` command forwards its whole environment to the scheduler, which can include `RENOVATE_TOKEN`. That environment crosses only the same-user socket, no wider boundary than the `docker exec` that carried it, and the scheduler never logs it. The scheduler starts Renovate through the image's entrypoint with a list of arguments, with no shell.

The image removes the `docker` command-line tool from Renovate's base image, so a Renovate run cannot start containers. Renovate's `binarySource=docker` mode needs that tool and is not supported here.

## Where the container writes

| Folder | What it holds |
| --- | --- |
| `/data` | `RENOVATE_BASE_DIR` in this image: Renovate's clones and caches, and the scheduler's run records |
| `/tmp` | the trigger socket `/tmp/docker-renovate-scheduler.sock`. The scheduler stops at start when it cannot create it |
| `/opt/containerbase` and the home folder | the tools Renovate installs at run time, and their caches |
| `/dev/shm` | the health file `docker-renovate-scheduler.healthy` |

A read-only root filesystem is not supported, because Renovate installs the tools a repository needs into `/opt/containerbase` during a run. Health needs no folder of its own: `/dev/shm` is a writable tmpfs Docker gives every container by default, and `/healthz` answers from memory. If `/dev/shm` cannot be written, as under `ipc: none`, the healthcheck passes and the startup log says so, while `/healthz` on the metrics listener still gives the true answer.

## What the image contains

| Component | Source |
| --- | --- |
| renovate/renovate, the runtime base | [Docker Hub](https://hub.docker.com/r/renovate/renovate) |
| golang, the builder stage only | [Docker Hub](https://hub.docker.com/_/golang) |
| [`github.com/cplieger/go/filesafe`](https://github.com/cplieger/go/tree/main/filesafe) | the write check on the base directory |
| [`github.com/cplieger/go/envx`](https://github.com/cplieger/go/tree/main/envx) | reading environment variables |
| [`github.com/cplieger/go/health/v2`](https://github.com/cplieger/go/tree/main/health) | the health verdict, its health file for the healthcheck, and the `/healthz` answer |
| [`github.com/cplieger/go/httpserver`](https://github.com/cplieger/go/tree/main/httpserver) | the optional metrics listener's routes, timeouts, panic recovery, access log and shutdown |
| [`github.com/cplieger/go/measure`](https://github.com/cplieger/go/tree/main/measure) | the optional metrics page |
| [`github.com/cplieger/forgeapi`](https://github.com/cplieger/forgeapi) | the GitHub App token request: signing, retries, and redacting and capping GitHub's error text before it is logged |
| [`github.com/cplieger/go/scheduler`](https://github.com/cplieger/go/tree/main/scheduler) | interval parsing, the run loop, the command runner and the socket queue |
| [`github.com/cplieger/go/slogx`](https://github.com/cplieger/go/tree/main/slogx) | the logging setup, logfmt with UTC times |

[Renovate](https://github.com/renovatebot/renovate) keeps these up to date, and each is pinned by digest or version. The image also carries a Go toolchain installed for every user. Another user still needs the writable cache settings in [Running as another user](configuration.md#running-as-another-user) before Renovate can update Go modules.

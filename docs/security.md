# Security

This page covers what the container accepts and what the image contains. It is for readers who want to know what the scheduler exposes before they run it.

## What the container accepts

The image opens no ports and runs no web server. The only way to start a run from outside the scheduler is a `docker exec` of the `run` command, which talks to the scheduler through a socket at `/tmp/docker-renovate-scheduler.sock`. Only the container's own user can open that socket, and only from inside the container, which is the same boundary `docker exec` already enforces. A `run` from another user fails at once with `permission denied`.

The container runs as Renovate's non-root user, UID 12021, or as the user Compose `user:` sets. The socket and the health file belong to that user, so `docker exec` runs must use it too. [Running as another user](configuration.md#running-as-another-user) explains what another user needs.

A `run` command forwards its whole environment to the scheduler, which can include `RENOVATE_TOKEN`. That environment crosses only the same-user socket, no wider boundary than the `docker exec` that carried it, and the scheduler never logs it. The scheduler starts Renovate through the image's entrypoint with a list of arguments, with no shell.

The image removes the `docker` command-line tool from Renovate's base image, so a Renovate run cannot start containers. Renovate's `binarySource=docker` mode needs that tool and is not supported here.

## What the image contains

| Component | Source |
| --- | --- |
| renovate/renovate, the runtime base | [Docker Hub](https://hub.docker.com/r/renovate/renovate) |
| golang, the builder stage only | [Docker Hub](https://hub.docker.com/_/golang) |
| [`github.com/cplieger/atomicfile`](https://github.com/cplieger/atomicfile) | the write check on the base directory |
| [`github.com/cplieger/envx`](https://github.com/cplieger/envx) | reading environment variables |
| [`github.com/cplieger/health`](https://github.com/cplieger/health) | the file-based healthcheck |
| [`github.com/cplieger/scheduler`](https://github.com/cplieger/scheduler) | interval parsing, the run loop, the command runner and the socket queue |
| [`github.com/cplieger/slogx`](https://github.com/cplieger/slogx) | the logging setup, logfmt with UTC times |

[Renovate](https://github.com/renovatebot/renovate) keeps these up to date, and each is pinned by digest or version. The image also carries a Go toolchain installed for every user. Another user still needs the writable cache settings in [Running as another user](configuration.md#running-as-another-user) before Renovate can update Go modules.

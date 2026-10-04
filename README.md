# docker-renovate-scheduler

[![Image Size](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/cplieger/docker-renovate-scheduler/badges/size.json)](https://github.com/cplieger/docker-renovate-scheduler/pkgs/container/docker-renovate-scheduler) [![Platforms](https://img.shields.io/badge/platforms-amd64%20%7C%20arm64-blue)](https://github.com/cplieger/docker-renovate-scheduler/pkgs/container/docker-renovate-scheduler) [![base: renovate/renovate](https://img.shields.io/badge/base-renovate%2Frenovate-1A1F6C)](https://github.com/cplieger/docker-renovate-scheduler/blob/main/Dockerfile) [![Mutation](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/cplieger/docker-renovate-scheduler/badges/mutation.json)](https://github.com/cplieger/docker-renovate-scheduler/issues?q=label%3Agremlins-tracker) [![SBOM](https://img.shields.io/badge/SBOM-SPDX-1D4ED8)](https://github.com/cplieger/docker-renovate-scheduler/releases)

<!-- hub-overview BEGIN -->
docker-renovate-scheduler keeps your self-hosted [Renovate](https://github.com/renovatebot/renovate) bot in one always-on container that runs it on a schedule or when your own scheduler asks. It opens no ports.

## What it does

docker-renovate-scheduler keeps dependency pull requests coming from your Renovate bot, with no cron job to write:

- Runs Renovate every 6 hours by default and keeps that rhythm when the container restarts or updates.
- Also starts a run on request, from `docker exec` or your own scheduler, for all repositories or only named ones.
- Runs one pass at a time and queues the others, each with its own result.
- Keeps clones and caches on `/data` between runs.
- Marks itself unhealthy when a run fails, until the next good run.

## Who it is for

docker-renovate-scheduler is built for people who self-host Renovate on a Docker host and want it to run like their other always-on containers, not from cron. It is Renovate's official image plus a scheduler, and passes your `RENOVATE_*` settings on unchanged. You need a bot account and its token.

Other ways to run Renovate suit a different setup:

- Consider the [Mend Renovate App](https://github.com/apps/renovate) if you would rather host nothing. You install it on GitHub for the repositories you pick.
- Consider [Mend Renovate Community Edition](https://github.com/mend/renovate-ce-ee) if you want runs that answer webhooks, with merged PRs handled ahead of scheduled jobs.
- Consider [renovatebot/github-action](https://github.com/renovatebot/github-action) if you want Renovate to run on GitHub Actions runners.

docker-renovate-scheduler is free software under the Apache-2.0 license.
<!-- hub-overview END -->

## Quick start

The image is on GitHub Container Registry and Docker Hub, for `amd64` and `arm64`. This is the [`compose.yaml`](compose.yaml) in this repository.

```yaml
services:
  renovate:
    image: ghcr.io/cplieger/docker-renovate-scheduler:latest
    container_name: renovate
    restart: unless-stopped
    stop_grace_period: 10m  # lets a run in progress finish when the container stops

    environment:
      RUN_INTERVAL: "6h"  # time between runs, or "off" to wait for your own scheduler
      LOG_FORMAT: "json"  # Renovate's own setting, for JSON logs a log collector can read
      # Every RENOVATE_* setting goes to Renovate unchanged.
      RENOVATE_PLATFORM: "github"
      RENOVATE_AUTODISCOVER: "true"  # or list the repositories in RENOVATE_REPOSITORIES
      RENOVATE_TOKEN: "${RENOVATE_TOKEN}"  # the bot account's token, set in .env
      RENOVATE_GITHUB_COM_TOKEN: "${RENOVATE_GITHUB_COM_TOKEN}"  # a github.com token for changelogs and rate limits, set in .env
      RENOVATE_PERSIST_REPO_DATA: "true"  # later runs fetch the repositories instead of cloning them again
      RENOVATE_REPOSITORY_CACHE: "enabled"
      RENOVATE_X_SQLITE_PACKAGE_CACHE: "true"  # keeps the package cache from growing until runs fail

    volumes:
      # Create ./data and run "sudo chown 12021:0 data" before the first start,
      # or the container restarts in a loop.
      - "./data:/data"
```

1. In the folder that holds `compose.yaml`, create the data folder and give it to the container's user with `mkdir data && sudo chown 12021:0 data`.
2. Create a file named `.env` in the same folder with the bot account's token:

   ```sh
   RENOVATE_TOKEN=your-bot-token
   ```

3. If your repositories are not on github.com, set `RENOVATE_PLATFORM` and the settings your platform needs, as [Renovate's authentication docs](https://docs.renovatebot.com/getting-started/running/#authentication) describe.
4. In that case, also add a read-only github.com token to `.env` as `RENOVATE_GITHUB_COM_TOKEN`. Renovate uses it for changelogs and to stay under GitHub's rate limit.
5. Run `docker compose up -d`.

Run `docker logs renovate`. You should see `container started`, then `renovate run complete` once the first run ends, which can take several minutes. If you see `base directory preflight failed`, the `data` folder does not belong to user 12021, so repeat step 1.

## Configuration reference

The scheduler reads three settings, once at start, so recreate the container after a change. Everything else is Renovate's own configuration, as `RENOVATE_*` variables or a `config.js` like [`config.js.example`](config.js.example), documented in Renovate's [self-hosted configuration](https://docs.renovatebot.com/self-hosted-configuration/).

| Variable | Description | Default |
| --- | --- | --- |
| `RUN_INTERVAL` | Time between runs, such as `1h` or `30m`. `off` waits for your own scheduler to start each run | `6h` |
| `RUN_TIMEOUT` | Longest time one run may take before it is stopped and fails | `1h` |
| `LOG_LEVEL` | `debug`, `info`, `warn` or `error`, for the scheduler and Renovate. `trace` and `fatal` work for Renovate only. Any other value stops Renovate | `info` |

| Mount | Description |
| --- | --- |
| `/data` | Clones, caches, installed tools and the record of the last run. The container stops at start when its user cannot write here |
| `/usr/src/app/config.js` | Optional Renovate `config.js`, if you prefer a file to `RENOVATE_*` variables |

To start a run yourself, run `docker exec renovate docker-renovate-scheduler run`, or add repository names after `run` to process only those.

Keep the container's default user. Without the extra cache settings, another user cannot write Renovate's tool caches, and its PRs arrive without updated lockfiles. [Configuration](docs/configuration.md) has the settings that make another user work, both scheduling modes, an Ofelia example and the memory a run needs.

## Security

The image opens no ports and runs no web server. The `run` command talks to the scheduler through a Unix socket in `/tmp`. Only the container's own user can open it, and only from inside the container. The container runs as Renovate's non-root user, UID 12021.

The scheduler never logs your platform token. A `run` command sends its environment, which can include the token, to the scheduler through that socket only. Renovate starts from a list of arguments, with no shell. The image removes the `docker` command-line tool from Renovate's base image, so Renovate's `binarySource=docker` mode is not supported. [Security](docs/hardening.md) lists what the image contains.

## Troubleshooting

The healthcheck reads a file the scheduler updates after each run. Unhealthy means the last run failed, and the next good run makes it healthy again. With the built-in schedule, it also turns unhealthy when no run has finished for 13 hours with the defaults, which is twice `RUN_INTERVAL` plus `RUN_TIMEOUT`. The image allows 10 minutes at start for a slow first run.

- The container restarts with `base directory preflight failed`. The `data` folder does not belong to user 12021. Repeat step 1 of the quick start.
- A run fails with `exit status 134` and a `likely_cause` field. Node most likely ran out of heap memory, often after Renovate raised its PRs. Set `mem_limit` to 3g or more and keep `RENOVATE_X_SQLITE_PACKAGE_CACHE=true`, as [Memory and the package cache](docs/configuration.md#memory-and-the-package-cache) explains.
- A `docker exec` run fails with `cannot reach the scheduler daemon`. The command ran as a user other than the container's. Run it as that user, and set Ofelia's `user` label to match.
- Renovate's PRs fail CI on a stale `go.sum` or `package-lock.json`. The container runs as another user. See [Running as another user](docs/configuration.md#running-as-another-user).
- A run is cut off when the container is recreated. Raise `stop_grace_period`, 10 minutes in the example, above your slowest run, as [Shutdown](docs/how-it-works.md#shutdown) explains.

## Monitoring

docker-renovate-scheduler writes logfmt lines with UTC times to its container log, and Renovate's own output follows on the same log in both scheduling modes. It has no metrics endpoint. [Monitoring and alerts](docs/monitoring.md) lists the log lines and two Loki alert rules, one for a failed run and one for no finished run in 13 hours.

## Documentation

- [Configuration](docs/configuration.md) covers both scheduling modes, Ofelia, another user and the memory a run needs.
- [How it works](docs/how-it-works.md) explains the queue, the schedule, shutdown and the health rules.
- [Monitoring and alerts](docs/monitoring.md) lists the log lines and the alert rules.
- [Security](docs/hardening.md) covers what the container accepts and what the image contains.

## Credits

This image packages [Renovate](https://github.com/renovatebot/renovate) by [Mend.io](https://www.mend.io/) (AGPL-3.0). All credit for the dependency-update engine goes to its maintainers. This project adds the scheduler and a Go toolchain that every user can run.

## Contributing

Issues and pull requests are welcome. See [CONTRIBUTING.md](CONTRIBUTING.md).

## Disclaimer

This project is built with care and follows security best practices, but it is intended for personal / self-hosted use. No guarantees of fitness for production environments. Use at your own risk.

This project was built with AI-assisted tooling using [Claude](https://claude.com), [GPT](https://openai.com), and [Kiro](https://kiro.dev). The human maintainer defines architecture, supervises implementation, and makes all final decisions.

## License

Apache-2.0. See [LICENSE](LICENSE). The image carries the license text of every bundled component under `/usr/share/licenses/`.

The runtime base is the official `renovate/renovate` image, which packages [Renovate](https://github.com/renovatebot/renovate) under AGPL-3.0. The version and digest that base is pinned to are in the Dockerfile's `FROM` line, and Renovate's license and the corresponding source for that version are in the upstream repository at the matching release tag (`https://github.com/renovatebot/renovate/releases/tag/<version>`). The build applies no patches to Renovate, so this repository's Dockerfile is the complete record of what it adds to that image (the scheduler binary and a Go toolchain) and what it removes from it (the unused `docker` CLI).

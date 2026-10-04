# Configuration

This page covers the scheduler's three settings in full, the two scheduling modes, running the container as another user and the memory a Renovate run needs. It is for readers who go past the quick start. Renovate's own settings are documented in Renovate's [self-hosted configuration](https://docs.renovatebot.com/self-hosted-configuration/).

## Settings

The scheduler reads three environment variables at start. They sit outside the `RENOVATE_*` names, so Renovate never reads them as its own options.

- `RUN_INTERVAL`, default `6h`, is the time between runs as a Go duration, such as `6h`, `1h` or `30m`. `off`, `disabled` and `0` turn the built-in schedule off, and runs then start only when your own scheduler asks. An unset, negative or unreadable value means `6h`.
- `RUN_TIMEOUT`, default `1h`, is the longest one Renovate run may take, as a Go duration. A run past it is stopped and fails. A zero or negative value means `1h`. Renovate's own `RENOVATE_EXECUTION_TIMEOUT` is a separate limit for each command Renovate starts.
- `LOG_LEVEL`, default `info`, is read by the scheduler and by Renovate. Both honor `debug`, `info`, `warn` and `error`. Renovate also accepts `trace` and `fatal`, and the scheduler then logs that it does not recognize the value and uses `info` for its own lines. Any other value, including `warning` and forms such as `warn+1`, stops Renovate from starting. With the built-in schedule the container then never turns healthy.

## Renovate's own settings

Renovate reads its whole configuration from its own `RENOVATE_*` variables, a `config.js` or another config file, and the scheduler passes them on unchanged. [`config.js.example`](../config.js.example) is a starting point for a file, mounted at `/usr/src/app/config.js`. Renovate needs a bot account token and either autodiscovery or a repository list:

- `RENOVATE_TOKEN`, the platform token of the bot account.
- `RENOVATE_AUTODISCOVER=true`, or `RENOVATE_REPOSITORIES` with the list of repositories to process.

These settings suit this always-on container:

- `RENOVATE_GITHUB_COM_TOKEN`, a github.com token Renovate uses to fetch changelogs when your platform is not github.com, and to stay under GitHub's rate limit.
- `RENOVATE_PERSIST_REPO_DATA=true` and `RENOVATE_REPOSITORY_CACHE=enabled`. Later runs then fetch each repository instead of cloning it, and reuse Renovate's caches. Keep `/data` on a volume for this to work.
- `RENOVATE_X_SQLITE_PACKAGE_CACHE=true`, which keeps the package cache bounded. [Memory and the package cache](#memory-and-the-package-cache) explains why.

## The built-in schedule

Set `RUN_INTERVAL` to a duration. The container runs Renovate at start when a run is due, then once every interval, with nothing else to set up.

A run is due at start when no successful scheduled run finished within the last interval. The scheduler writes the time and the result of each scheduled run to `.docker-renovate-scheduler-last-run` in `RENOVATE_BASE_DIR`, which is `/data` in this image. With `/data` on a volume, that record survives a recreate for an image update or a config change, so a recreate adds no run. The next run comes one `RUN_INTERVAL` after the previous one, not one interval after the start, so a restart does not delay it either.

A failed last run is due again at start. A fixed setting, such as a corrected `RENOVATE_TOKEN`, then shows its effect at the next recreate instead of one interval later. Runs started with `docker exec` never write the record. Without a volume on `/data`, the record is lost and the container runs at every start.

To start a run at any time, run `docker exec renovate docker-renovate-scheduler run`.

## Your own scheduler

Set `RUN_INTERVAL` to `off`. The container stays up and idle, and each run starts from a `docker exec`:

```bash
docker exec renovate docker-renovate-scheduler run             # every configured repository
docker exec renovate docker-renovate-scheduler run owner/repo  # one repository, passed on to Renovate
```

The `run` command hands the request to the scheduler and waits until that run ends. It exits 0 when the run succeeded and 1 when it failed, also when the run waited behind another one. Interrupting the wait ends the command with exit 1 and a warning. A run the scheduler had already accepted still runs to its end, so exit 1 there means the command did not see the result, not that the run failed.

The run's full Renovate output goes to the container log. Your scheduler's own log shows only the command's short lines, `triggered run accepted`, `triggered run started`, then `triggered run complete` or `triggered run failed` with a `reason`. Read the detail of a run from `docker logs` or your log store, and its result from the exit code.

The environment of the `docker exec` goes with the request. `docker exec -e RENOVATE_AUTODISCOVER=false renovate docker-renovate-scheduler run owner/repo` runs Renovate for that repository with that setting. The command needs no entrypoint prefix, because the scheduler starts every run through the image's own entrypoint and so with Renovate's full tool environment.

Run the command as the user the container runs as, `12021` unless Compose `user:` sets another. The scheduler's socket in `/tmp` accepts only that user. A `docker exec` uses that user by default. A run from another user fails at once with `cannot reach the scheduler daemon` and `permission denied`.

This example runs Renovate every 6 hours from [Ofelia](https://github.com/mcuadros/ofelia). Set its `user` label to the container's user:

```yaml
    environment:
      RUN_INTERVAL: "off"  # Ofelia starts each run
    labels:
      ofelia.enabled: "true"
      ofelia.job-exec.renovate-run.schedule: "@every 6h"
      ofelia.job-exec.renovate-run.command: "docker-renovate-scheduler run"
      ofelia.job-exec.renovate-run.user: "12021"  # the user the container runs as
      ofelia.job-exec.renovate-run.no-overlap: "true"
```

Requests run strictly one at a time, in the order they arrive. A request that arrives during a run is neither dropped nor merged. It waits, runs with its own repositories and environment, and its `run` command exits with its own result. Renovate runs are safe to repeat, so a burst of requests costs only time. Up to 16 requests can wait. A request that arrives when 16 are waiting is refused at once, with exit 1 and the reason. Ofelia's `no-overlap` keeps its own runs from piling up in that queue.

## Running as another user

Keep the container's default user. It is Renovate's non-root user, UID 12021, with a writable home, and Renovate then installs the tools it needs and updates lockfiles with no extra settings.

A Compose `user:` line that sets another user, such as `1000:1000` to match a folder you own, has no home directory, because `HOME` is `/`. Every tool cache that lives under the home folder is then read-only, and two things break, neither of them visibly in the container log:

- Renovate cannot install tools on demand, because it cannot write to `/opt/containerbase`.
- Lockfiles are not updated. `go mod tidy` cannot refresh `go.sum` and `npm install` cannot refresh `package-lock.json`. Renovate still opens the PR, with only `go.mod` or `package.json` changed, and the repository's CI then fails with `missing go.sum entry` or an `npm ci` lockfile error.

Renovate reports the second failure on the PR itself, as a red `renovate/artifacts` status check. Its own log line for it is at `debug`, so at `LOG_LEVEL=info` the container log does not show it.

The scheduler logs a warning at start when it runs as another user and `RENOVATE_CUSTOM_ENV_VARIABLES` names no cache or tool-path variable. The check looks at the variable names only. It tells you that you set up the fix, not that the paths are right.

To run as another user, use the tools built into the image and send every cache to a writable folder on the volume:

```yaml
    user: "1000:1000"  # your user
    environment:
      RENOVATE_BINARY_SOURCE: "global"  # use the tools in the image, install nothing at run time
      GOPATH: "/data/go"
      GOCACHE: "/data/.cache/go-build"  # Go
      npm_config_cache: "/data/.npm"  # Node and npm
      # Renovate passes only some variables to the commands it runs. GOPATH is one of them,
      # GOCACHE and npm_config_cache are not, so name them here:
      RENOVATE_CUSTOM_ENV_VARIABLES: '{"GOPATH":"/data/go","GOCACHE":"/data/.cache/go-build","npm_config_cache":"/data/.npm"}'
    volumes:
      - "./data:/data"  # give ./data to your user on the host
```

Add one cache line for each package manager Renovate updates in your repositories, such as pip or cargo, and give the `data` folder to your user with `sudo chown 1000:1000 data`. If that is more than you want to manage, keep the default user.

## Memory and the package cache

For an always-on container, set `RENOVATE_X_SQLITE_PACKAGE_CACHE=true` and give the container a `mem_limit` of 3g or more.

Node sizes its heap from the container's memory limit, not from the host's memory. It reads the limit and caps its main heap at about half of it, so `mem_limit` sets the heap however much memory the host has. Measured inside the container:

| `mem_limit` | Node heap ceiling |
| --- | --- |
| 2 GiB | 1120 MB |
| 3 GiB | 1728 MB |

A run that reaches that ceiling ends with `FATAL ERROR: Ineffective mark-compacts near heap limit` from node, and the scheduler reports `exit status 134`. The kernel's out-of-memory killer never acts. The container's `memory.events` counters stay at zero and `docker inspect` shows `OOMKilled=false`, so none of the usual signs of a memory limit appear. The scheduler adds `likely_cause` and `fix` fields to that failure line, so the exit code is not your only clue.

Renovate's default package cache, a folder of files, makes every run cost more as it grows. Renovate cleans that cache after each run, and the cleanup reads the whole folder into memory at once, so its cost follows the size of the cache, not the number of repositories.

On one always-on container scanning 55 repositories every hour, the folder reached 3.1 million files and 44 GB in three months, plus a 1.3 GB index. The cleanup then took 800 MB of memory in one block. Past the heap ceiling the cleanup cannot finish, nothing is removed, and the cache only grows, so the failure never ends by itself.

`RENOVATE_X_SQLITE_PACKAGE_CACHE=true`, one of [Renovate's experimental settings](https://docs.renovatebot.com/self-hosted-experimental/), stores the cache in SQLite instead. Its cleanup is one indexed `DELETE`, with no walk through a folder and no files left behind. On the same container the cache went from 45 GB to 4.7 MB, and runs that had failed after 131 to 180 seconds finished in 52 to 77 seconds, faster than before the cache grew. A folder that size also keeps hundreds of MB of the kernel's file-name cache charged to the container's memory, which node cannot see when it picks its ceiling. That charge goes away with the folder.

The cleanup runs after every repository is processed, so Renovate opens its pull requests before the crash. Updates arrive while the run reports a failure, so a failed run here does not mean nothing happened.

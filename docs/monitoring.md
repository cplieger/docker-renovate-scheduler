# Monitoring and alerts

This page lists the log lines docker-renovate-scheduler writes and two alert rules for them. It is for readers who send container logs to Loki or a similar log store.

## What it logs

docker-renovate-scheduler has no metrics endpoint. Its state is in its container log. The scheduler writes its own lines as logfmt with UTC times, whatever the container's `TZ`, and Renovate's output follows on the same log. Set `LOG_FORMAT=json` for Renovate to write JSON. Every run, scheduled or started with `docker exec`, runs inside the scheduler, so both reach the container log in both scheduling modes.

| Message | Level | Fields worth reading |
| --- | --- | --- |
| `container started` | INFO | `mode`, `interval`, `timeout`, `base_dir`, `startup_run` |
| `github auth mode` | INFO | `mode` (`app` or `token`), and `app_id` and `installation_id` for an App |
| `run timeout capped to the installation token's life` | INFO | `run_timeout`, `effective`. App mode only |
| `github app configuration invalid` | ERROR | `error`, `hint`. The container stops |
| `github app installation resolved` | INFO | `installation_id`, `account`. Logged once when `GITHUB_APP_INSTALLATION_ID` is unset |
| `github app installation token issued` | INFO | `trigger`, `installation_id`, `expires_at` |
| `github app token request failed` | ERROR | `trigger`, `error`. The run fails and Renovate does not start |
| `renovate run skipped: github app token unavailable` | WARN | `trigger`, `error`. GitHub did not answer the token request; tolerated for 2h, see [Repository errors](#repository-errors) |
| `startup run skipped: the last scheduled run succeeded within the interval` | INFO | `last_success`, `interval` |
| `renovate run starting` | INFO | `trigger`, `repos`, `timeout` |
| `renovate run complete` | INFO | `trigger`, `duration_ms` |
| `renovate run failed` | ERROR | `trigger`, `duration_ms`, `error`, and `likely_cause` and `fix` after a heap exhaustion |
| `renovate run ended in a repository error` | WARN | `trigger`, `duration_ms`, `error`, `likely_cause`, `fix`. See [Repository errors](#repository-errors) |
| `renovate runs keep failing` | ERROR | `trigger`, `repos`, `failing_for`. Repository errors or token outages have lasted 2h; this run fails |
| `renovate run timed out` | ERROR | `trigger`, `duration_ms`, `timeout` |
| `base directory preflight failed` | ERROR | `path`, `error`, `hint` |
| `halting run admission: renovate run process group survived the kill sweep` | ERROR | `trigger` |
| `trigger request rejected` | WARN | `repos`, `reason` |
| `run cancelled by shutdown` | WARN | `stage`, `trigger`, `repos` |

`trigger` is `startup` or `interval` for a scheduled run. A `docker exec` run's own command writes `triggered run accepted`, `triggered run started`, then `triggered run complete`, `triggered run tolerated a failure` (WARN, exit 0) or `triggered run failed` to the log of whatever ran it, and exits with the run's result.

## Alerting

These rules are for Loki's ruler. Save the block below as a file in Loki's rules folder, as [Loading an app's alert rules](https://github.com/cplieger/docs/blob/main/docs/monitoring.md#loading-an-apps-alert-rules) shows. The rules work with the built-in schedule and with your own scheduler, because every run logs to the container log. With your own scheduler, its job result is a second, independent signal, because the `run` command exits with the run's result.

```yaml
groups:
  - name: renovate
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
          summary: "renovate: the scheduler logged an error"
          description: >
            A run failed or timed out, repository errors or token outages
            lasted 2h, a GitHub App token request failed, the base-directory
            preflight failed, or a leftover process halted run admission.
            Check the container logs, RENOVATE_TOKEN or the GitHub App
            settings, and whether the platform is reachable.
      - alert: RenovateNoRecentRun
        expr: |
          absent_over_time({container="renovate"} |= `renovate run complete` [14h])
        for: 30m
        labels:
          severity: warning
        annotations:
          summary: "renovate has not completed a run in 14h"
          description: >
            No `renovate run complete` line in 14h. Runs may be failing, the
            container or log pipeline may have stopped, the container may have
            been renamed, or your scheduler may have stopped sending requests.
            Rule those out, then restart the container.
```

When a run leaves behind a process that cannot be stopped, the scheduler logs `halting run admission`, stops taking runs and exits. The container restart then ends the leftover process. A normal shutdown lets the current run finish, so a redeploy fires `RenovateRunFailed` only if that run then fails.

`RenovateRunFailed` can fire while updates are landing. A run that runs out of node's heap fails after its repositories are processed, so its pull requests are already open. That failure line carries `likely_cause` and `fix`, and [Memory and the package cache](configuration.md#memory-and-the-package-cache) has the remedy.

`RenovateNoRecentRun` watches a heartbeat. The scheduler logs `renovate run complete` after every run that succeeds, in both modes. With the built-in schedule that is at start when a run is due, then every `RUN_INTERVAL`, 6h by default. With your own scheduler it is once per request. A run that exits zero but leaves a process behind logs `halting run admission` at ERROR instead, which `RenovateRunFailed` catches.

Set the `RenovateNoRecentRun` window to at least twice the sum of your `RUN_INTERVAL` and `RUN_TIMEOUT`, which is 14h with the defaults. A run that tolerates a [repository error](#repository-errors) logs no `renovate run complete`, so one such run doubles the gap. With your own scheduler, set it to at least twice the sum of that scheduler's cadence and `RUN_TIMEOUT`. A restart keeps the schedule's rhythm, so it adds nothing to that gap.

Thresholds and the `severity` label are starting points. Change the `container` selector to the label your log collector sets, such as `job` or `service`. Route by whatever labels your Alertmanager uses.

## Repository errors

The scheduler runs Renovate with `RENOVATE_EXIT_CODE_FOR_ERRORS=true` unless you set it, so a run in which a repository ends in an error exits with a code that names the first such error. Renovate still processes the other repositories, so their updates land:

| Exit code | The repository ended in |
| --- | --- |
| 3 | a Renovate system error, such as a full disk |
| 4 | a platform error, such as a rate limit or credentials the platform rejected |
| 5 | a configuration error |
| 6 | a temporary error |
| 7 | an external host error, a lockfile error or missing credentials, such as a platform outage or a clone or push the platform refused |
| 8 | an unknown error |

A platform outage causes most of these, so one is not yet a failure. The scheduler logs `renovate run ended in a repository error` at WARN with that `likely_cause`, leaves health as it was, and the `run` command exits 0. A GitHub App token request that GitHub does not answer is treated the same way, logged as `renovate run skipped: github app token unavailable`. A rejected key or a missing installation still fails at once. So does an outage that stops Renovate before its first repository, during its own start-up calls to the platform: Renovate exits 1 then, like any other Renovate failure.

Once such failures have lasted 2 hours with no good run in between, the scheduler logs `renovate runs keep failing` at ERROR. That run then fails: health turns unhealthy, the `run` command exits 1 and `RenovateRunFailed` fires. Each failing run after it fails too, until a good one. At an hourly cadence the third failing run is the first to fail, or the second when the built-in schedule's first two failing runs take an hour or more together. With the 6-hour default it is the second. Full runs and runs scoped to repositories keep separate clocks, so frequent good scoped runs cannot reset the full run's clock: its ERROR line and exit code still report the fault. Health follows the last run, though, so a good scoped run turns it healthy again. A restart starts the clock again.

Renovate's `Repository finished` line names the repository and its `result`, such as `external-host-error`. Set `RENOVATE_EXIT_CODE_FOR_ERRORS=false` on the container to keep Renovate's own behaviour, as [Renovate's own settings](configuration.md#renovates-own-settings) explains.

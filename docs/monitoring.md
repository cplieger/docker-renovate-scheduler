# Monitoring and alerts

This page lists the log lines docker-renovate-scheduler writes, its optional metrics and the alert rules for both. It is for readers who send container logs to Loki or scrape metrics with Prometheus.

## What it logs

Its state is in its container log, and, with `METRICS_LISTEN_ADDR` set, on a Prometheus metrics page at `/metrics`. With the setting unset, the image opens no port. The scheduler writes its own lines as logfmt with UTC times, whatever the container's `TZ`, and Renovate's output follows on the same log. Set `LOG_FORMAT=json` for Renovate to write JSON. Every run, scheduled or started with `docker exec`, runs inside the scheduler, so both reach the container log in both scheduling modes.

| Message | Level | Fields worth reading |
| --- | --- | --- |
| `container started` | INFO | `mode`, `interval`, `timeout`, `base_dir`, `startup_run` |
| `github auth mode` | INFO | `mode` (`app` or `token`), and `app_id` and `installation_id` for an App |
| `run timeout capped to the installation token's life` | INFO | `run_timeout`, `effective`. App mode only |
| `github app configuration invalid` | ERROR | `error`, `hint`. The container stops |
| `github app installation resolved` | INFO | `installation_id`, `account`. Logged once when `GITHUB_APP_INSTALLATION_ID` is unset |
| `github app installation token issued` | INFO | `trigger`, `installation_id`, `expires_at` |
| `github app token request failed` | ERROR | `trigger`, `error`. The run fails and Renovate does not start |
| `renovate run skipped: github app token unavailable` | WARN, or ERROR once such failures have lasted 2h | `trigger`, `error`, `failing_for`. GitHub did not answer the token request, see [Repository errors](#repository-errors) |
| `startup run skipped: the last scheduled run succeeded within the interval` | INFO | `last_success`, `interval` |
| `renovate run starting` | INFO | `trigger`, `repos`, `timeout` |
| `renovate run complete` | INFO | `trigger`, `duration_ms` |
| `renovate run failed` | ERROR | `trigger`, `duration_ms`, `error`, and `likely_cause` and `fix` after a heap exhaustion |
| `renovate run ended in a repository error` | WARN, or ERROR once such failures have lasted 2h | `trigger`, `duration_ms`, `error`, `likely_cause`, `fix`, `failing_for`. See [Repository errors](#repository-errors) |
| `renovate runs recovered` | INFO | `trigger`, `scope`, `failing_for`. The first good run after repository errors or token outages |
| `renovate run timed out` | ERROR | `trigger`, `duration_ms`, `timeout` |
| `base directory preflight failed` | ERROR | `path`, `error`, `hint` |
| `halting run admission: renovate run process group survived the kill sweep` | ERROR | `trigger` |
| `trigger request rejected` | WARN | `repos`, `reason` |
| `run cancelled by shutdown` | WARN | `stage`, `trigger`, `repos` |

`trigger` is `startup` or `interval` for a scheduled run. A `docker exec` run's own command writes `triggered run accepted`, `triggered run started`, then `triggered run complete`, `triggered run tolerated a failure` (WARN, exit 0) or `triggered run failed` to the log of whatever ran it, and exits with the run's result.

## Alerting

`alerts/logql.yaml` holds the rules for Loki's ruler, and `alerts/promql.yaml` the same alerts for Prometheus over the metrics page, scraped as job `renovate`. Load one of the two files, not both, as [Loading an app's alert rules](https://github.com/cplieger/docs/blob/main/docs/monitoring.md#loading-an-apps-alert-rules) shows. If you copied the two rules this page used to embed, delete them first: both would fire for the same fault.

| Alert | Fires when | Severity |
| --- | --- | --- |
| `RenovateRunFailed` | the latest run of a scope, full or scoped, failed; it resolves at that scope's next good run | warning |
| `RenovateNoRecentRun` | no complete full run in 14h | warning |
| `RenovateDaemonStopped` | the scheduler logged `daemon stopped` and exited; in `alerts/logql.yaml` only | warning |
| `RenovateDown` | no successful scrape for 5m, which also covers a stopped scheduler; in `alerts/promql.yaml` only | warning |

The rules read the `run finished` record that every completed run writes once at INFO, and the `daemon stopped` line, so keep `LOG_LEVEL` at `info`. Renovate reads the same setting.

| Record | Level | Fields |
| --- | --- | --- |
| `run finished` | INFO | `trigger`; `scope` (`full`, or `scoped` for a run that named repositories); `result` (`complete`, `failed`, `aborted`, `timed_out`, `start_failed`, `repository_error`, `token_unavailable`, `token_failed`, `preflight_failed` or `contained`); `ok` (1 or 0, as the `run` command reports it, so a tolerated failure is 1); `tolerated` (1 or 0); `exit_code` (Renovate's, or -1 when Renovate did not run or a signal ended it); `duration_ms` |
| `daemon stopped` | ERROR | `error`. The scheduler exits after it |

A run cancelled by shutdown, or refused after a halt, did not complete and writes no record. A normal shutdown lets the current run finish, so a redeploy fires `RenovateRunFailed` only if that run then fails.

Set the 14h windows to at least twice the sum of your `RUN_INTERVAL` and `RUN_TIMEOUT`. With your own scheduler, use that scheduler's cadence. A tolerated run is not a complete run, so one such run doubles the gap `RenovateNoRecentRun` sees.

Thresholds and the `severity` label are starting points. Change the `container` selector to the label your log collector sets, such as `job` or `service`.

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

Each of these lines carries `failing_for`, how long such failures have lasted. Once that reaches 2 hours with no good run in between, the same line is written at ERROR instead, and that run fails: health turns unhealthy, the `run` command exits 1 and `RenovateRunFailed` fires. Each failing run after it fails too, until a good one. At an hourly cadence the third failing run is the first to fail, or the second when the built-in schedule's first two failing runs take an hour or more together. With the 6-hour default it is the second. Full runs and runs scoped to repositories keep separate clocks, so frequent good scoped runs cannot reset the full run's clock: its ERROR line and exit code still report the fault. Health follows the last run, though, so a good scoped run turns it healthy again. A restart starts the clock again. The first good run after such failures logs `renovate runs recovered` with how long they lasted.

Renovate's `Repository finished` line names the repository and its `result`, such as `external-host-error`. Set `RENOVATE_EXIT_CODE_FOR_ERRORS=false` on the container to keep Renovate's own behaviour, as [Renovate's own settings](configuration.md#renovates-own-settings) explains.

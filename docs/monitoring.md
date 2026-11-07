# Monitoring and alerts

This page covers what docker-renovate-scheduler reports, how to get alerts from it and who tells you when it stops. docker-renovate-scheduler writes one `run finished` record per run to its log and can serve a Prometheus metrics page. The same alerts ship twice, in [`alerts/promql.yaml`](../alerts/promql.yaml) for a metric store and in [`alerts/logql.yaml`](../alerts/logql.yaml) for Loki. Load one of the two.

## What you get

| You run | You get | You set up |
| --- | --- | --- |
| Prometheus, Mimir or VictoriaMetrics, with Alertmanager | Three alerts, `RenovateDown` included, and graphs of each run | `METRICS_LISTEN_ADDR`, a scrape job named `renovate`, and `alerts/promql.yaml` |
| Loki and Alertmanager, no metric store | Three alerts, `RenovateDaemonStopped` in place of `RenovateDown` | `alerts/logql.yaml`, with `LOG_LEVEL` left at `info` |
| Grafana-managed alerting only | The same alerts, as Grafana rules | One of the two files imported into Grafana, with its data source |
| Docker or Unraid only | The container log and Docker's health status, which notify nobody | `METRICS_LISTEN_ADDR` and an uptime prober on `/healthz`, or something that watches Docker health |

## Metrics

Set `METRICS_LISTEN_ADDR` to an address such as `:9470`, and publish that port. The container then serves Prometheus metrics at `/metrics` and a health check at `/healthz` on it. Left unset, the image opens no port. The page has no login, so keep the port on your network. An address the container cannot listen on stops it at start with `cannot listen on METRICS_LISTEN_ADDR`.

Every series starts with `renovate_scheduler_`. `scope` is `full` for a run over every repository and `scoped` for a run that named repositories, and a scope's run series appear at its first completed run. After a restart, the counters start again from 0, and the full scope's last success comes back from `/data`.

| Series | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `runs_total` | counter | `scope`, `result` | Completed runs by how they ended |
| `last_run_ok` | gauge | `scope` | 1 when the scope's last completed run passed, a tolerated failure included, else 0 |
| `last_run_result` | gauge | `scope`, `result` | 1 for how the scope's last completed run ended, 0 for the other results |
| `last_run_exit_code` | gauge | `scope` | Renovate's exit code in that run, -1 when Renovate did not run or a signal ended it |
| `last_run_timestamp_seconds` | gauge | `scope` | Unix time that run ended |
| `last_run_duration_seconds` | gauge | `scope` | How long that run took |
| `last_success_timestamp_seconds` | gauge | `scope` | Unix time the scope's last complete run ended |
| `failing_since_timestamp_seconds` | gauge | `scope` | Unix time the current series of tolerated failures began, 0 when runs are not failing |
| `trigger_rejected_total` | counter | `reason` | Run requests refused, `full` for a full queue and `closing` during shutdown |
| `healthy` | gauge | | 1 while the healthcheck would pass, else 0 |
| `info` | gauge | `mode`, `auth` | Always 1, with labels that name the scheduling mode (`built-in` or `external`) and the GitHub auth (`token` or `app`) |

`result` is one of `complete`, `failed`, `aborted`, `timed_out`, `start_failed`, `repository_error`, `token_unavailable`, `token_failed`, `preflight_failed` and `contained`. The page also carries the Go runtime and process families (`go_*`, `process_*`) under the names Go's official Prometheus client uses, so stock Go dashboards work.

No dashboard ships. These queries are a start in Grafana's Metrics Drilldown:

- Runs per day by result: `increase(renovate_scheduler_runs_total[1d])`
- Run time of full runs: `renovate_scheduler_last_run_duration_seconds{scope="full"}`
- Hours since the last complete full run: `(time() - renovate_scheduler_last_success_timestamp_seconds{scope="full"}) / 3600`

## Logs

The scheduler writes its own lines as logfmt with UTC times, whatever the container's `TZ`, and Renovate's output follows on the same log. Set `LOG_FORMAT=json` for Renovate to write JSON. Every run, scheduled or started with `docker exec`, runs inside the scheduler, so both reach the container log in both scheduling modes.

### The records the log rules read

The log alert rules read two records and no other line. They need `LOG_LEVEL` at `info` or `debug`, and Renovate reads the same setting, so quieting Renovate with `warn` also silences the rules. The metric rules work at any level.

Each completed run writes one `run finished` record at INFO. A run cancelled by shutdown, or refused after a halt, did not complete and writes none.

| Field | Meaning |
| --- | --- |
| `trigger` | `startup` or `interval` for a scheduled run, `external` for a `run` command |
| `scope` | `full`, or `scoped` for a run that named repositories |
| `result` | How the run ended, one of the `result` values above |
| `ok` | 1 or 0, as the `run` command reports it, so a tolerated failure is 1 |
| `tolerated` | 1 when the run failed in a way an outage causes and the failure is still tolerated, else 0 |
| `exit_code` | Renovate's exit code, or -1 when Renovate did not run or a signal ended it |
| `duration_ms` | How long the run took |

When the scheduler stops with an error, it writes one `daemon stopped` record at ERROR and exits. Its `error` field names the cause, such as invalid GitHub App settings, an unwritable base directory, an address `METRICS_LISTEN_ADDR` cannot use, or a process that survived a run. A normal shutdown writes none.

### Other lines

These lines are for reading a run by hand. No alert rule reads them.

| Message | Level | Fields worth reading |
| --- | --- | --- |
| `container started` | INFO | `mode`, `interval`, `timeout`, `base_dir`, `startup_run` |
| `metrics listener started` | INFO | `address` |
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
| `cannot listen on METRICS_LISTEN_ADDR` | ERROR | `value`, `error`. The container stops |
| `halting run admission: renovate run process group survived the kill sweep` | ERROR | `trigger` |
| `trigger request rejected` | WARN | `repos`, `reason` |
| `run cancelled by shutdown` | WARN | `stage`, `trigger`, `repos` |

A `docker exec` run's own command writes `triggered run accepted`, `triggered run started`, then `triggered run complete`, `triggered run tolerated a failure` (WARN, exit 0) or `triggered run failed` to the log of whatever ran it, and exits with the run's result.

## Alerting

Load `alerts/promql.yaml` into Prometheus or the Mimir ruler, or `alerts/logql.yaml` into Loki's ruler, not both. They hold the same alerts under the same names, so both loaded would notify twice. `RenovateDown` exists only in `alerts/promql.yaml`, and covers a stopped scheduler too. `RenovateDaemonStopped` exists only in `alerts/logql.yaml`, where it covers a scheduler that stopped with an error. [Loading metric alert rules](https://github.com/cplieger/docs/blob/main/docs/monitoring.md#loading-metric-alert-rules) and [Loading an app's alert rules](https://github.com/cplieger/docs/blob/main/docs/monitoring.md#loading-an-apps-alert-rules) show how. Both files work with the built-in schedule and with your own scheduler, because every run happens in the scheduler.

| Alert | Fires when | Severity |
| --- | --- | --- |
| `RenovateDown` | Prometheus has not scraped the metrics page for 5 minutes | warning |
| `RenovateRunFailed` | the latest run of a scope, full or scoped, failed | warning |
| `RenovateNoRecentRun` | no complete full run in 14 hours | warning |
| `RenovateDaemonStopped` | the scheduler logged `daemon stopped` and exited | warning |

Thresholds and the `severity` labels are starting points. Set the 14-hour window to at least twice the sum of your `RUN_INTERVAL` and `RUN_TIMEOUT`. With your own scheduler, use twice its cadence plus `RUN_TIMEOUT`. In `alerts/promql.yaml` it is written as `50400` seconds. Change the `container` selector to the label your log collector sets, and keep the scrape job name `renovate` that `RenovateDown` and `RenovateNoRecentRun` read.

### Notes on each alert

`RenovateRunFailed` fires while the latest run of a scope failed and clears with the next good run of that scope. Full runs and scoped runs are watched apart, so a good scoped run does not clear a failed full run. A tolerated failure counts as good, as [Repository errors](#repository-errors) explains. The record's `result` field, or the `renovate_scheduler_last_run_result` series, names the cause, and the run's ERROR line has the detail. A normal shutdown lets the current run finish, so a redeploy fires it only if that run then fails. In `alerts/promql.yaml` it also clears at a restart, because a scope's series come back only with its next run.

`RenovateNoRecentRun` fires when no complete full run has finished within the window. Runs scoped to repositories do not count, so frequent scoped runs cannot hide a full pass that stopped. A tolerated run is not a complete run, so one such run doubles the gap the rule sees. It fires on silence, so it also catches a stopped schedule, a scheduler of your own that stopped calling `run`, and, in `alerts/logql.yaml`, logs that stopped reaching Loki. In `alerts/promql.yaml`, a container with no complete full run on record counts from its start.

`RenovateDaemonStopped` fires once per stop, at the `daemon stopped` record. A stop with no record, such as a killed container, leaves it quiet, and `RenovateNoRecentRun` reports that later. `RenovateDown` covers both cases in `alerts/promql.yaml`.

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

Each of these lines carries `failing_for`, how long such failures have lasted. Once that reaches 2 hours with no good run in between, the same line is written at ERROR instead, and that run fails: health turns unhealthy, the `run` command exits 1 and `RenovateRunFailed` fires. Each failing run after it fails too, until a good one. The first failing run of a series is always a warning, however long it ran. At an hourly cadence the third failing run is the first to fail, or the second when the built-in schedule's first two failing runs take an hour or more together. With the 6-hour default it is the second. Full runs and runs scoped to repositories keep separate clocks, so frequent good scoped runs cannot reset the full run's clock: its ERROR line and exit code still report the fault. Health follows the last run, though, so a good scoped run turns it healthy again. A restart starts the clock again. The first good run after such failures logs `renovate runs recovered` with how long they lasted.

Renovate's `Repository finished` line names the repository and its `result`, such as `external-host-error`. Set `RENOVATE_EXIT_CODE_FOR_ERRORS=false` on the container to keep Renovate's own behaviour, as [Renovate's own settings](configuration.md#renovates-own-settings) explains.

## Knowing the app is alive

| Route | Method | Port | Login | Answers |
| --- | --- | --- | --- | --- |
| `/healthz` | `GET`, `HEAD` | `METRICS_LISTEN_ADDR`, unset by default | none | `200 {"status":"ok"}`, or `503 {"status":"unhealthy","reasons":[...]}` |
| `/metrics` | `GET` | the same port | none, so keep the port on your network | Prometheus text format |

**What healthy means.** The last completed run succeeded, or failed in a way that is still tolerated, and the container is not shutting down. With the built-in schedule, the container is also unhealthy from start until a due startup run succeeds. It is unhealthy too when no run has finished for twice `RUN_INTERVAL` plus `RUN_TIMEOUT`, 13 hours at the defaults. `/healthz` names which of these failed in `reasons`. GitHub being unreachable makes the container unhealthy only once the failures it causes have lasted 2 hours. [How it works](how-it-works.md#health) explains how the container decides.

**Docker health.** The image checks itself every 30 seconds with `docker-renovate-scheduler health`, which reads the health file the scheduler updates after each run. Docker marks the container unhealthy after 3 failures, and allows 10 minutes after a start. Docker's health status notifies nobody on its own, and Docker does not restart an unhealthy container.

**Who is told.**

| You run | A dead app | A wedged app |
| --- | --- | --- |
| Prometheus with this app's rules | `RenovateDown` | `RenovateNoRecentRun`, and `/healthz` turns 503 |
| Loki only | `RenovateDaemonStopped` when it stopped with an error, else `RenovateNoRecentRun` | `RenovateNoRecentRun` |
| Docker or Unraid only | an uptime prober on `/healthz`, or something that watches Docker health | the same |

**Probing from outside.** The app opens no port unless `METRICS_LISTEN_ADDR` is set, so nothing outside the container can probe it until then. Watch Docker health, or set `METRICS_LISTEN_ADDR` and probe its `/healthz`. Check `GET http://<host>:<port>/healthz` and pass on status 200 only, without matching the body. Use a 5-second timeout, check every 60 seconds, alert after 3 failures, and wait 10 minutes after a first start.

**With your own scheduler**, no deadline applies, because the container cannot know your schedule. Alert on your scheduler's own job result: the `run` command exits 1 when the run fails.

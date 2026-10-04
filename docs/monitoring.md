# Monitoring and alerts

This page lists the log lines docker-renovate-scheduler writes and two alert rules for them. It is for readers who send container logs to Loki or a similar log store.

## What it logs

docker-renovate-scheduler has no metrics endpoint. Its state is in its container log. The scheduler writes its own lines as logfmt with UTC times, whatever the container's `TZ`, and Renovate's output follows on the same log. Set `LOG_FORMAT=json` for Renovate to write JSON. Every run, scheduled or started with `docker exec`, runs inside the scheduler, so both reach the container log in both scheduling modes.

| Message | Level | Fields worth reading |
| --- | --- | --- |
| `container started` | INFO | `mode`, `interval`, `timeout`, `base_dir`, `startup_run` |
| `startup run skipped: the last scheduled run succeeded within the interval` | INFO | `last_success`, `interval` |
| `renovate run starting` | INFO | `trigger`, `repos`, `timeout` |
| `renovate run complete` | INFO | `trigger`, `duration_ms` |
| `renovate run failed` | ERROR | `trigger`, `duration_ms`, `error`, and `likely_cause` and `fix` after a heap exhaustion |
| `renovate run timed out` | ERROR | `trigger`, `duration_ms`, `timeout` |
| `base directory preflight failed` | ERROR | `path`, `error`, `hint` |
| `halting run admission: renovate run process group survived the kill sweep` | ERROR | `trigger` |
| `trigger request rejected` | WARN | `repos`, `reason` |
| `run cancelled by shutdown` | WARN | `stage`, `trigger`, `repos` |

`trigger` is `startup` or `interval` for a scheduled run. A `docker exec` run's own command writes `triggered run accepted`, `triggered run started`, then `triggered run complete` or `triggered run failed` to the log of whatever ran it, and exits with the run's result.

## Alerting

Ship the container's logs to Loki and evaluate these rules with [Loki's ruler](https://grafana.com/docs/loki/latest/alert/). Grafana Alloy's Docker log discovery ships them with no extra configuration. Firing alerts go through your Alertmanager like any Prometheus alert. The rules work with the built-in schedule and with your own scheduler, because every run logs to the container log. With your own scheduler, its job result is a second, independent signal, because the `run` command exits with the run's result.

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
            The scheduler logged an error. A run exited non-zero
            (`renovate run failed`), hit RUN_TIMEOUT (`renovate run timed out`),
            failed its base-directory preflight, or left a process behind that
            could not be stopped (`halting run admission`). In that last case
            the scheduler stops taking runs and exits, and the container
            restart ends the leftover process. Check the container logs,
            RENOVATE_TOKEN and whether the platform is reachable. A normal
            shutdown lets the current run finish, so a redeploy does not fire
            this rule unless that run then fails.
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
            succeeds, in both modes. With the built-in schedule that is at
            start when a run is due, then every RUN_INTERVAL, 6h by default.
            With your own scheduler it is once per request. A run that exits
            zero but leaves a process behind logs `halting run admission` at
            ERROR instead, which RenovateRunFailed catches. Otherwise, no
            successful completion line in 13h means the expected heartbeat is
            missing. A run may be failing, the container or log pipeline may
            have stopped, the container name may have changed, or your
            scheduler may have stopped sending requests. Rule out each of
            those, then restart the container. The window must exceed
            RUN_INTERVAL plus RUN_TIMEOUT, 7h with the defaults, because a
            restart keeps the schedule's rhythm.
```

Thresholds and the `severity` label are starting points. Set the `RenovateNoRecentRun` window above your `RUN_INTERVAL` plus `RUN_TIMEOUT`, or above your own scheduler's cadence plus `RUN_TIMEOUT`. Change the `container` selector to the label your log collector sets, such as `job` or `service`. Route by whatever labels your Alertmanager uses.

`RenovateRunFailed` can fire while updates are landing. A run that runs out of node's heap fails after its repositories are processed, so its pull requests are already open. That failure line carries `likely_cause` and `fix`, and [Memory and the package cache](configuration.md#memory-and-the-package-cache) has the remedy.

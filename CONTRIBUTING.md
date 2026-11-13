# Contributing to docker-renovate-scheduler

The [shared rules](https://github.com/cplieger/.github/blob/main/CONTRIBUTING.md) for commits, releases, synced files and checks apply here.

## Rules

- Log at ERROR only a failure the operator must act on. Use WARN for anything the scheduler recovers from.
- The rules in `alerts/logql.yaml` read only the `run finished` and `daemon stopped` records. Change their fields together with the rules and `alert_rules_test.go`. A rule with a label filter parses with the bare `| logfmt`, never a field list such as `| logfmt scope, result`: Loki misreads label filters after a field list in an aggregating query ([grafana/loki#11334](https://github.com/grafana/loki/issues/11334)), and `alert_rules_test.go` refuses that shape. The rules in `alerts/promql.yaml` read the series in `metrics.go`, and `alert_parity_test.go` fails when one is gone.
- If you change `defaultInterval` or `defaultRunTimeout` in `config.go`, update the matching 6h or 1h default in `README.md` and `docs/`. Update the 13-hour health window there, and the 14-hour alert window in both `alerts/` files and `docs/monitoring.md`, with its minimum, too.
- If you change the lease in `health.go`, update `reasonStuck` beside it and the 13-hour health window in `README.md` and `docs/`. The run deadline on the metrics page uses the same lease, so update its description in `docs/monitoring.md` too.
- If you change `failureGrace` in `daemon.go`, update the 2-hour window in `toleratedReason`, `escalatedReason`, the `fix` text in `runner.go`, `README.md` and `docs/`. Update the failing-run counts in `docs/monitoring.md` too.

# Contributing to docker-renovate-scheduler

The [shared rules](https://github.com/cplieger/.github/blob/main/CONTRIBUTING.md) for commits, releases, synced files and checks apply here.

## Rules

- `RenovateRunFailed` fires on any `level=ERROR` line in the container log. Log at ERROR only a failure the operator must act on. Use WARN for anything the scheduler recovers from.
- If you change `defaultInterval` or `defaultRunTimeout` in `config.go`, update the matching 6h or 1h default in `README.md` and `docs/`. Update the 13-hour health window, the 14-hour alert window and the alert window's minimum there too.
- If you change the lease in `health.go`, update the 13-hour health window in `README.md` and `docs/`.
- If you change `failureGrace` in `daemon.go`, update the 2-hour window in `toleratedReason`, `escalatedReason`, the `fix` text in `runner.go`, `README.md` and `docs/`. Update the failing-run counts in `docs/monitoring.md` too.

#!/bin/sh
# Runtime image smoke test for docker-renovate-scheduler. Invoked by the central
# CI docker job:
#   sh tests/image-smoke.sh <image-ref>
#
# Starts the assembled image in external-trigger mode and waits for the
# container's own HEALTHCHECK to report "healthy". This proves the scheduler
# binary runs in the real renovate/renovate base image, the base ENTRYPOINT
# (renovate-entrypoint.sh -> containerbase docker-entrypoint.sh) sets up the
# environment and exec-launches the daemon (the Dockerfile CMD), the baked
# RENOVATE_BASE_DIR (/data, created and owned by the image's default UID 12021)
# is writable, and the file-marker health probe works.
#
# Why external mode: SCHED_INTERVAL=off routes run() -> runExternal() (main.go),
# which sets the health marker healthy on boot and then idles -- no Renovate
# pass runs, so no RENOVATE_TOKEN, no repositories, and no network are needed to
# reach "healthy". The default built-in mode (6h) would instead execute a real
# Renovate pass first (which needs a token + network to install tools and scan
# repos) and never go healthy in CI.
set -eu

IMG="${1:?usage: image-smoke.sh <image-ref>}"
NAME="smoke-renovate-scheduler-$$"
# The image's baked HEALTHCHECK is --interval=60s --start-period=30s (Dockerfile),
# so the container's first probe lands ~60s in. External mode is healthy from
# boot, so that first probe passes; 150s covers the 30s start-period plus two
# 60s intervals in case the first probe is slightly delayed on a cold boot.
TIMEOUT=150

# shellcheck disable=SC2317,SC2329  # invoked indirectly via trap
cleanup() {
  code=$?
  # Dump container logs only on failure (a passing run stays quiet).
  if [ "$code" -ne 0 ]; then
    printf '%s\n' "--- container logs (tail) ---" >&2
    docker logs "$NAME" 2>&1 | tail -40 >&2 || true
  fi
  docker rm -f "$NAME" >/dev/null 2>&1 || true
}
trap cleanup EXIT

# SCHED_INTERVAL=off selects external-trigger mode. Do NOT override the command:
# the baked CMD (docker-renovate-scheduler daemon) must run through the inherited
# base ENTRYPOINT so containerbase sets up the environment before the daemon.
docker run -d --name "$NAME" -e SCHED_INTERVAL=off "$IMG" >/dev/null

i=0
status=starting
while [ "$i" -lt "$TIMEOUT" ]; do
  # Fail fast on an early exit: poll .State.Running before the health status so
  # a crash-boot is caught by its exit code (more debuggable than "unhealthy")
  # and the verdict never depends on what health a stopped container reports.
  if [ "$(docker inspect --format '{{ .State.Running }}' "$NAME" 2>/dev/null || echo missing)" != "true" ]; then
    ec=$(docker inspect --format '{{ .State.ExitCode }}' "$NAME" 2>/dev/null || echo '?')
    printf 'FAIL: renovate-scheduler container exited early (exit code %s)\n' "$ec" >&2
    exit 1
  fi
  status=$(docker inspect --format '{{ if .State.Health }}{{ .State.Health.Status }}{{ else }}no-healthcheck{{ end }}' "$NAME" 2>/dev/null || echo gone)
  case "$status" in
    healthy)
      printf 'renovate-scheduler image smoke: ok (healthy after %ss)\n' "$i"
      exit 0
      ;;
    unhealthy)
      printf 'FAIL: renovate-scheduler reported unhealthy\n' >&2
      exit 1
      ;;
    no-healthcheck)
      printf 'FAIL: image has no HEALTHCHECK to assert against\n' >&2
      exit 1
      ;;
    gone)
      printf 'FAIL: renovate-scheduler container is gone\n' >&2
      exit 1
      ;;
  esac
  i=$((i + 1))
  sleep 1
done
printf 'FAIL: renovate-scheduler did not become healthy within %ss (last status: %s)\n' "$TIMEOUT" "$status" >&2
exit 1

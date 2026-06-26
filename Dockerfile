# check=error=true
FROM golang:1.26-trixie@sha256:76a29248dedcd75870e95cbd90cc8cb356db082404ac7d3a5803f276c3ba79c9 AS go-builder
ENV GOTOOLCHAIN=auto

WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download
COPY *.go ./
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /docker-renovate-scheduler .

# Wrap the official Renovate image. It cannot be distroless: Renovate is a
# Node.js app that shells out to git and (for lockfile maintenance) language
# package managers, and its default binarySource=install installs those tools
# at runtime via containerbase. The upstream image assembles all of that; we
# add only the scheduler binary on top.
FROM renovate/renovate:43.244.0@sha256:e7154e5006718959a1bf3ccb11e4bfbf2b3e9859bcc31a8f9bbd87b145a0a959

# Become root for the image customizations below -- strip the bundled docker CLI,
# install the scheduler binary, create /data, and pre-install Go. The final USER
# reverts to the non-root 12021 before the runtime CMD.
USER root

# Strip the docker CLI that containerbase bakes into the renovate base image
# (a ~42 MB binary under /opt/containerbase/tools/docker/<ver>/bin/docker, plus
# its shim at /opt/containerbase/bin/docker, the /usr/local/bin/docker PATH
# symlink, the containerbase lib dir, and the version marker). Renovate only
# invokes the docker CLI under binarySource=docker
# (verified against its exec layer: every docker call is gated on that mode);
# this scheduler runs binarySource=install, so the binary is never used.
# Removing it drops the Go-stdlib CVEs Trivy reports against that unused binary
# and trims attack surface. binarySource=docker is therefore unsupported here
# (and is deprecated upstream anyway).
#
# Let find DRIVE the removal instead of enumerating fixed paths: containerbase
# reshuffles these locations between releases (the v43.242.0 bump added
# lib/docker and the versions/ marker, which a hardcoded 3-path list missed and
# the assertion below then failed the build on). Deleting every entry named
# `docker` under the containerbase tree, plus the PATH symlink, is layout-stable
# and won't silently fall behind the next base-image bump.
RUN find /opt/containerbase -name docker -prune -exec rm -rf {} + \
    && rm -f /usr/local/bin/docker \
    && ! command -v docker \
    && [ -z "$(find /opt/containerbase -name docker 2>/dev/null)" ]

# DL4006: the version-assertion pipe (apt-cache policy | awk) needs a
# pipefail-aware shell so a failing apt-cache surfaces instead of being
# masked by awk's exit status. bash is present in the renovate/Ubuntu base.
SHELL ["/bin/bash", "-o", "pipefail", "-c"]

# Patch the MySQL client libraries the renovate base inherits from its Ubuntu
# layer (pulled in transitively by containerbase's default-libmysqlclient-dev
# build prerequisite). Renovate itself never connects to MySQL, but Trivy still
# flags the stale libs against this image (CVE-2026-46862 / CVE-2026-46863).
# Upgrade just those two packages to the patched noble-security build rather
# than removing them, so native MySQL-driver builds during lockfile maintenance
# keep working. Left unpinned on purpose: we always want the latest patched
# point release, and this becomes a no-op once the base image ships it.
# hadolint ignore=DL3008
RUN apt-get update \
    && apt-get install -y --only-upgrade --no-install-recommends \
        libmysqlclient21 libmysqlclient-dev \
    && for p in libmysqlclient21 libmysqlclient-dev; do \
         dpkg-query -W "$p" >/dev/null 2>&1 || continue; \
         inst="$(dpkg-query -W -f='${Version}' "$p")"; \
         cand="$(apt-cache policy "$p" | awk '/Candidate:/{print $2}')"; \
         [ "$inst" = "$cand" ] || { echo "FATAL: $p $inst != candidate $cand"; exit 1; }; \
       done \
    && rm -rf /var/lib/apt/lists/*

COPY --chmod=755 --from=go-builder /docker-renovate-scheduler /usr/local/bin/docker-renovate-scheduler

# Renovate stores repo clones and caches under RENOVATE_BASE_DIR. Persisting
# it on a volume lets runs git-fetch instead of git-clone and reuse the
# datasource/tool caches. Create it owned by the image's non-root user
# (UID 12021, group 0) so the container can write there even on a fresh mount.
RUN mkdir -p /data && chown 12021:0 /data && chmod 0775 /data
ENV RENOVATE_BASE_DIR=/data

# Pre-install Go so Renovate's gomod artifact step (go mod tidy / go get to
# refresh go.sum after a dependency bump) works even when the container is run
# as a non-default UID. A non-default runtime UID cannot write containerbase's
# tool dir (/opt/containerbase/tools, owned 12021:root) to install Go on
# demand -- so gomod artifact updates silently fail and every Go dependency PR
# lands with a stale go.sum. Installing Go here as root makes it
# world-executable for any runtime UID; GOTOOLCHAIN=auto then lets Go fetch a
# newer toolchain into the writable RENOVATE_BASE_DIR cache when a repo's
# go.mod requires a higher version than the baked one. (The homelab itself now
# runs the image default 12021:0, but this stays load-bearing for external
# rootless operators -- see the README "Running as a non-default user".)
# renovate: datasource=golang-version depName=go
ARG GOLANG_VERSION=1.26.4
RUN install-tool golang "${GOLANG_VERSION}"
ENV GOTOOLCHAIN=auto

USER 12021

# ENTRYPOINT is inherited from the base image (renovate-entrypoint.sh, which
# exec-chains to the containerbase docker-entrypoint.sh). It sets up the
# containerbase environment and then execs CMD — our scheduler daemon — so the
# daemon and the Renovate children it spawns get that environment. Runs
# triggered by a bare `docker exec` bypass ENTRYPOINT, so the scheduler also
# re-routes each Renovate invocation through the same entrypoint internally.
HEALTHCHECK --interval=60s --timeout=5s --retries=3 --start-period=30s \
    CMD ["/usr/local/bin/docker-renovate-scheduler", "health"]
CMD ["docker-renovate-scheduler", "daemon"]

# check=error=true
FROM golang:1.26-trixie@sha256:68b7145ec43d1820b9a56704554b53d1520aa2a15cb5233e374188a31b2a1bce AS go-builder
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
FROM renovate/renovate:43.252.8@sha256:41ca73841a59646cd649089fc65d1388200d53487f33e593dc9eb20f3777b948

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

# Apply all available Ubuntu security updates the renovate base inherits from
# its Ubuntu layer. The base lags the distro security mirror between upstream
# rebuilds, so Trivy flags stale OS packages (perl, tar, libxml2, libssh2,
# libmysqlclient, ...) against this image even though fixed builds exist. A
# broad apt upgrade patches them all at build time -- matching the vibekit and
# vibecli images -- instead of enumerating packages one CVE at a time, and
# becomes a no-op once the base ships the fixes. upgrade never removes
# packages, so native MySQL-driver builds during lockfile maintenance keep
# working.
RUN apt-get update && apt-get upgrade -y && rm -rf /var/lib/apt/lists/*

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
# go.mod requires a higher version than the baked one. (One consumer now
# runs the image default 12021:0, but this stays load-bearing for external
# rootless operators -- see the README "Running as a non-default user".)
# renovate: datasource=golang-version depName=go
ARG GOLANG_VERSION=1.26.4
RUN install-tool golang "${GOLANG_VERSION}"
ENV GOTOOLCHAIN=auto

USER 12021

# ENTRYPOINT is inherited from the base image (renovate-entrypoint.sh, which
# exec-chains to the containerbase docker-entrypoint.sh). It sets up the
# containerbase environment and then execs CMD, our scheduler daemon, so the
# daemon and the Renovate children it spawns get that environment. Runs
# triggered by a bare `docker exec` bypass ENTRYPOINT, so the scheduler also
# re-routes each Renovate invocation through the same entrypoint internally.
# The HEALTHCHECK bypasses the ENTRYPOINT above, so it calls the binary by
# absolute path (no containerbase PATH setup); the CMD below is passed through
# the ENTRYPOINT, which sets up PATH, so its bare name resolves.
HEALTHCHECK --interval=60s --timeout=5s --retries=3 --start-period=30s \
    CMD ["/usr/local/bin/docker-renovate-scheduler", "health"]
CMD ["docker-renovate-scheduler", "daemon"]

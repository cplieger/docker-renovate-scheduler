# check=error=true
FROM golang:1.27-trixie@sha256:22b64c486d44847387a2d9591bb705dc4b3a1227bb393d76a9d4ae176d046327 AS go-builder
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
FROM renovate/renovate:44.51.2@sha256:dd5a8ca92b2f3cbb8e3c8de35c63ae46494b074463c5e2488ed43e128b22f32e

# Become root for the image customizations below -- strip the bundled docker CLI,
# install the scheduler binary, create /data, and pre-install Go. The final USER
# reverts to the non-root 12021 before the runtime CMD.
USER root

# Strip the docker CLI containerbase bakes into the renovate base: renovate
# invokes it only under binarySource=docker (verified against its exec layer,
# every call gated on that mode) and this image runs binarySource=install, so
# the binary is unreachable and its Go-stdlib CVEs are noise. find DRIVES the
# removal because containerbase reshuffles these paths between releases (the
# v43.242.0 bump added lib/docker and the versions/ marker). The final test pins
# the base's own entrypoint (runner.go renovateEntrypoint) so a relocating base
# bump fails THIS build, not every run.
RUN find /opt/containerbase -name docker -prune -exec rm -rf {} + \
    && rm -f /usr/local/bin/docker \
    && ! command -v docker \
    && [ -z "$(find /opt/containerbase -name docker 2>/dev/null)" ] \
    && test -x /usr/local/sbin/renovate-entrypoint.sh

# Strip TypeScript's native compiler (lib/tsc, a 24 MB Go binary) from the
# base's pnpm store: Trivy reads its embedded module list and reports 10 HIGH on
# 44.50.1, none fixable here, and nothing at runtime can reach it -- it arrives
# through a types-only `typescript` peerDependency chain and no JS in the image
# requires typescript (full trace: docker-builds.md). Only the BINARY goes, so
# the .d.ts files and typescript's JS API stay. find drives the removal because
# the store path embeds the TypeScript version and moves on renovate bumps; the
# pre-check fails the build when a base bump stops shipping the binary.
RUN tsc_glob='*/@typescript/typescript-linux-*/lib/tsc' \
    && store=/usr/local/renovate/node_modules/.pnpm \
    && [ -n "$(find "$store" -type f -path "$tsc_glob")" ] \
    && find "$store" -type f -path "$tsc_glob" -delete \
    && [ -z "$(find "$store" -type f -path "$tsc_glob")" ]

# Apply the Ubuntu security updates the renovate base lags between upstream
# rebuilds, so Trivy stops flagging stale OS packages against this image.
# upgrade never removes packages, so native driver builds during lockfile
# maintenance keep working. PKG_REFRESH busts this layer's cache -- without it
# BuildKit restores the layer verbatim and the upgrade never runs again -- and
# the echo is load-bearing, because BuildKit keys a RUN on the build args it
# actually CONSUMES. Mechanism: docker-builds.md "PKG_REFRESH".
ARG PKG_REFRESH=static
RUN echo "OS package refresh: ${PKG_REFRESH}" \
    && apt-get update && apt-get upgrade -y && rm -rf /var/lib/apt/lists/*

# Renovate stores repo clones and caches under RENOVATE_BASE_DIR. Persisting
# it on a volume lets runs git-fetch instead of git-clone and reuse the
# datasource/tool caches. Create it owned by the image's non-root user
# (UID 12021, group 0) so the container can write there even on a fresh mount.
RUN mkdir -p /data && chown 12021:0 /data && chmod 0775 /data
ENV RENOVATE_BASE_DIR=/data

# Pre-install Go as root: a non-default runtime UID cannot write containerbase's
# tool dir (/opt/containerbase/tools, owned 12021:root), so Renovate's on-demand
# Go install fails and every Go dependency PR lands with a stale go.sum.
# Installing here makes it world-executable for any runtime UID; GOTOOLCHAIN=auto
# then lets Go fetch a newer toolchain into the writable RENOVATE_BASE_DIR cache
# when a repo's go.mod requires a higher version. See README "Running as a
# non-default user (rootless)".
# renovate: datasource=golang-version depName=go
ARG GOLANG_VERSION=1.27.0
RUN install-tool golang "${GOLANG_VERSION}"
ENV GOTOOLCHAIN=auto

COPY --chmod=755 --from=go-builder /docker-renovate-scheduler /usr/local/bin/docker-renovate-scheduler

USER 12021

# No ENTRYPOINT here: the base image's chain (renovate-entrypoint.sh into
# containerbase's docker-entrypoint.sh) ends `exec dumb-init -- CMD`, so PID 1
# is dumb-init and the daemon is its child. runner.go's own process group is
# written against that hop, and it routes each Renovate child back through this
# same entrypoint to re-establish containerbase per run.

# HEALTHCHECK bypasses the entrypoint, so it needs the absolute path; CMD goes
# through it and gets containerbase's PATH, so a bare name resolves there.
HEALTHCHECK --interval=30s --timeout=5s --retries=3 --start-period=10m \
    CMD ["/usr/local/bin/docker-renovate-scheduler", "health"]
CMD ["docker-renovate-scheduler", "daemon"]

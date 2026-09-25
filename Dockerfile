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
COPY LICENSE NOTICE ./
COPY scripts/collect-licenses.sh scripts/
RUN --mount=type=cache,target=/go/pkg/mod \
    sh scripts/collect-licenses.sh --name docker-renovate-scheduler .

FROM renovate/renovate:44.115.5@sha256:06ccc04b1a819d369fa0cdcbe43c216782f4cda6d77799ba0df1fd66a4ed0ec8

USER root

# Renovate reaches the docker CLI only under binarySource=docker; this image
# runs binarySource=install, so the binary is dead weight Trivy still scans.
# find drives the removal because containerbase moves the paths between releases.
# The last test pins the entrypoint runner.go routes every child through.
RUN [ -n "$(find /opt/containerbase -name docker)" ] \
    && find /opt/containerbase -name docker -prune -exec rm -rf {} + \
    && rm -f /usr/local/bin/docker \
    && ! command -v docker \
    && [ -z "$(find /opt/containerbase -name docker 2>/dev/null)" ] \
    && test -x /usr/local/sbin/renovate-entrypoint.sh

# TypeScript's native compiler arrives through a types-only peer dependency and
# nothing at runtime requires it (trace: docker-builds.md); only the binary
# goes, so module resolution is unchanged. The store path embeds the TypeScript
# version, hence find plus a pre-check that fails when the base stops shipping it.
RUN tsc_glob='*/@typescript/typescript-linux-*/lib/tsc' \
    && store=/usr/local/renovate/node_modules/.pnpm \
    && [ -n "$(find "$store" -type f -path "$tsc_glob")" ] \
    && find "$store" -type f -path "$tsc_glob" -delete \
    && [ -z "$(find "$store" -type f -path "$tsc_glob")" ]

# `renovate --version` exits 0 on a release whose module graph is broken
# (44.64.0 imported `tar` from a devDependency, renovatebot/renovate#45699);
# importing the registries fails the build instead, and also catches a strip
# above breaking module resolution.
RUN node --input-type=module -e \
    'const dir = "/usr/local/renovate/dist/modules"; \
     for (const m of ["datasource", "manager", "platform", "versioning"]) { \
       await import(dir + "/" + m + "/index.js"); \
     }'

# upgrade never removes packages, so native driver builds during lockfile
# maintenance keep working. PKG_REFRESH: docker-builds.md.
ARG PKG_REFRESH=static
RUN echo "OS package refresh: ${PKG_REFRESH}" \
    && apt-get update && apt-get upgrade -y && rm -rf /var/lib/apt/lists/*

# Owned by the base's non-root user so a fresh bind mount is writable.
RUN mkdir -p /data && chown 12021:0 /data && chmod 0775 /data
ENV RENOVATE_BASE_DIR=/data

# A non-default runtime UID cannot write /opt/containerbase/tools (12021:root),
# so Renovate's on-demand Go install fails and every Go PR ships a stale go.sum;
# installed as root, Go is usable by any UID (README "Running as a non-default user").
# renovate: datasource=golang-version depName=go
ARG GOLANG_VERSION=1.27.1
RUN install-tool golang "${GOLANG_VERSION}"

COPY --chmod=755 --from=go-builder /docker-renovate-scheduler /usr/local/bin/docker-renovate-scheduler
COPY --from=go-builder /out/usr/share/licenses /usr/share/licenses

USER 12021

# Renovate resolves its default config.js against the working directory; this
# is the mount path published in README.md.
WORKDIR /usr/src/app

# No ENTRYPOINT: the base's chain ends `exec dumb-init -- CMD`; runner.go's
# process-group handling is written against that hop, and it routes each
# Renovate child back through the same entrypoint.

# HEALTHCHECK bypasses the entrypoint, so it needs the absolute path.
HEALTHCHECK --interval=30s --timeout=5s --retries=3 --start-period=10m \
    CMD ["/usr/local/bin/docker-renovate-scheduler", "health"]
CMD ["docker-renovate-scheduler", "daemon"]

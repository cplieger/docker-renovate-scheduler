# check=error=true
FROM golang:1.26-trixie@sha256:0dcba0d95dbfb072e9917a106b9e07d7cc298097dc83e9307056ef1889de654d AS go-builder
ENV GOTOOLCHAIN=auto

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /docker-renovate-scheduler .

# Wrap the official Renovate image. It cannot be distroless: Renovate is a
# Node.js app that shells out to git and (for lockfile maintenance) language
# package managers, and its default binarySource=install installs those tools
# at runtime via containerbase. The upstream image assembles all of that; we
# add only the scheduler binary on top.
FROM renovate/renovate:43.220.0@sha256:459b7e28aa54fa2fa7db83df6a2d6ad26de416a054ee554e7833203adf3d9a47

# Renovate stores repo clones and caches under RENOVATE_BASE_DIR. Persisting
# it on a volume lets runs git-fetch instead of git-clone and reuse the
# datasource/tool caches. Create it owned by the image's non-root user
# (UID 12021, group 0) so the container can write there even on a fresh mount.
USER root
COPY --chmod=755 --from=go-builder /docker-renovate-scheduler /usr/local/bin/docker-renovate-scheduler
RUN mkdir -p /data && chown 12021:0 /data && chmod 0775 /data
ENV RENOVATE_BASE_DIR=/data
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

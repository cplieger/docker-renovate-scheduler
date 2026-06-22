# check=error=true
FROM golang:1.26-trixie@sha256:bbf22ddccb3205344f2755ea8fa4fe39f7a8b2b77b9f7b764ec2aad31406f6fc AS go-builder
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
FROM renovate/renovate:43.234.1@sha256:b2fc488a46ad741da9dbb9cbae32e904adfcaa00d00bb0e14cd70c3c65f33383

# Renovate stores repo clones and caches under RENOVATE_BASE_DIR. Persisting
# it on a volume lets runs git-fetch instead of git-clone and reuse the
# datasource/tool caches. Create it owned by the image's non-root user
# (UID 12021, group 0) so the container can write there even on a fresh mount.
USER root

# Strip the docker CLI that containerbase bakes into the renovate base image
# (a ~42 MB binary under /opt/containerbase/tools/docker, plus its shim and
# PATH symlink). Renovate only invokes the docker CLI under binarySource=docker
# (verified against its exec layer: every docker call is gated on that mode);
# this scheduler runs binarySource=install, so the binary is never used.
# Removing it drops the Go-stdlib CVEs Trivy reports against that unused binary
# and trims attack surface. binarySource=docker is therefore unsupported here
# (and is deprecated upstream anyway).
RUN rm -rf /opt/containerbase/tools/docker \
    /opt/containerbase/bin/docker \
    /usr/local/bin/docker

COPY --chmod=755 --from=go-builder /docker-renovate-scheduler /usr/local/bin/docker-renovate-scheduler
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

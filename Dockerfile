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
FROM renovate/renovate:44.51.1@sha256:72bc126bba6a85c7966fbeaaeec010bb16671c183c2d2b7a3fb94d2ba97fc92a

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
#
# The final assertion pins the base image's own entrypoint script: the
# scheduler routes every Renovate invocation through it (renovateEntrypoint in
# runner.go), so a base bump that relocates that private path must fail THIS
# build, not every run at runtime.
RUN find /opt/containerbase -name docker -prune -exec rm -rf {} + \
    && rm -f /usr/local/bin/docker \
    && ! command -v docker \
    && [ -z "$(find /opt/containerbase -name docker 2>/dev/null)" ] \
    && test -x /usr/local/sbin/renovate-entrypoint.sh

# Strip the native TypeScript compiler out of the base image's pnpm store, for
# the same reason as the docker CLI above: it is a 24 MB Go binary nothing here
# can reach, and Trivy reads its embedded module list and reports the Go stdlib
# and golang.org/x/text CVEs of whatever toolchain TypeScript was built with
# (10 HIGH on 44.50.1). None of them are fixable here -- we do not build it and
# renovate pins the version -- so they would otherwise sit open forever.
#
# It is unreachable at runtime, which is NOT obvious from renovate's manifest.
# `typescript` is a devDependency there, and the image installs `--prod`, so the
# expected conclusion is that it should not be present at all. It arrives
# through the production graph instead: the `openpgp` optional dependency needs
# peer `@openpgp/web-stream-tools`, which declares a types-only `typescript`
# peerDependency, which pulls typescript's per-platform binary. A types peer is
# consumed by a compiler, never by the library at runtime -- verified in the
# live v3.0.31 image, where the app root has no `node_modules/typescript`, no
# `node_modules/@typescript` and no `.bin/tsc`, and no JS anywhere in the image
# requires typescript, web-stream-tools' own sources included.
#
# Only the BINARY goes. The `.d.ts` files beside it and typescript's JS API stay
# where pnpm put them, so nothing about module resolution changes; this removes
# the CVE surface rather than the package.
#
# find DRIVES the removal and covers both arches (`typescript-linux-x64` and
# `-arm64`), because the store path embeds the TypeScript version and moves on
# renovate bumps -- a hardcoded path would rot into a silent no-op. The
# pre-check is what makes that failure loud: a base bump that stops shipping the
# binary fails THIS build instead of leaving a removal that protects nothing.
RUN tsc_glob='*/@typescript/typescript-linux-*/lib/tsc' \
    && store=/usr/local/renovate/node_modules/.pnpm \
    && [ -n "$(find "$store" -type f -path "$tsc_glob")" ] \
    && find "$store" -type f -path "$tsc_glob" -delete \
    && [ -z "$(find "$store" -type f -path "$tsc_glob")" ]

# Apply all available Ubuntu security updates the renovate base inherits from
# its Ubuntu layer. The base lags the distro security mirror between upstream
# rebuilds, so Trivy flags stale OS packages (perl, tar, libxml2, libssh2,
# libmysqlclient, ...) against this image even though fixed builds exist. A
# broad apt upgrade patches them all at build time -- matching the vibekit and
# web-terminal-kiro images -- instead of enumerating packages one CVE at a time, and
# becomes a no-op once the base ships the fixes. upgrade never removes
# packages, so native MySQL-driver builds during lockfile maintenance keep
# working.
# PKG_REFRESH busts the cache for this layer. Without it BuildKit restores the
# layer verbatim on every rebuild and the apt upgrade below never runs again, so
# the image keeps shipping whatever packages were current when the layer was
# first built — which defeats the whole point of the broad upgrade described
# above. The central release/CI/scan builds pass today's UTC date. The `echo` is
# load-bearing: BuildKit keys a RUN on the build args it actually CONSUMES, so a
# merely-declared ARG would change nothing.
ARG PKG_REFRESH=static
RUN echo "OS package refresh: ${PKG_REFRESH}" \
    && apt-get update && apt-get upgrade -y && rm -rf /var/lib/apt/lists/*

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
ARG GOLANG_VERSION=1.27.0
RUN install-tool golang "${GOLANG_VERSION}"
ENV GOTOOLCHAIN=auto

COPY --chmod=755 --from=go-builder /docker-renovate-scheduler /usr/local/bin/docker-renovate-scheduler

USER 12021

# ENTRYPOINT is inherited from the base image (renovate-entrypoint.sh, which
# exec-chains to the containerbase docker-entrypoint.sh). It sets up the
# containerbase environment and then execs CMD, our scheduler daemon. The
# daemon owns every Renovate run as a child process; a run triggered via
# `docker exec … run` executes with the CLIENT's forwarded environment (which
# never passed through this ENTRYPOINT), so the daemon routes each child
# through the same entrypoint internally to re-establish containerbase per
# run regardless of the environment it starts from.
# The HEALTHCHECK bypasses the ENTRYPOINT above, so it calls the binary by
# absolute path (no containerbase PATH setup); the CMD below is passed through
# the ENTRYPOINT, which sets up PATH, so its bare name resolves.
HEALTHCHECK --interval=60s --timeout=5s --retries=3 --start-period=30s \
    CMD ["/usr/local/bin/docker-renovate-scheduler", "health"]
CMD ["docker-renovate-scheduler", "daemon"]

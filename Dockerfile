# syntax=docker/dockerfile:1.6

# Build stage runs natively on the build host and cross-compiles to the target
# platform. glebarez/sqlite is pure Go (no CGO), so this is a plain
# cross-compile.
FROM --platform=$BUILDPLATFORM golang:1.27.3-alpine AS builder
# nodejs/npm: go:generate bundles web/src with esbuild, which resolves
# react/react-dom imports from web/node_modules — `npm ci` below installs them.
RUN apk add --no-cache esbuild make curl openssl bash git nodejs npm
WORKDIR /src
COPY . .
RUN npm ci --prefix web
# `make generate` (not `go generate` directly) — it also runs
# vendor-frontend-js/fonts, which download and checksum-verify React and the
# fonts into internal/frontend/static/ before go:embed reads the tree. Calling
# `go generate` here would bypass that entirely: the build would stay green
# and the binary would embed an empty vendor directory, so the shipped image
# 404s on every vendored asset with no error anywhere in the pipeline.
RUN make generate
RUN for f in internal/frontend/static/js/app.bundle.js \
             internal/frontend/static/js/vendor/*.js \
             internal/frontend/static/fonts/*.woff2; do \
      [ -s "$f" ] || { echo "vendored asset missing or empty: $f" >&2; exit 1; }; \
    done

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=docker
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/executor ./cmd/executor
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/worker ./cmd/worker

# Debian rather than Alpine, and the reason is Python.
#
# PyPI's binary wheels target manylinux, which means glibc. On musl, pip falls
# back to building from source for a large part of the ecosystem — numpy, pandas,
# cryptography, anything with a C extension — which needs a toolchain in the image
# and turns `pip install` from seconds into minutes, or into a compiler error an
# agent cannot do anything about. musllinux wheels exist and cover far less.
#
# The cost is a larger base (tens of megabytes against Alpine's five). Paid
# deliberately: an image where Python is nominally present and practically unusable
# is worse than a bigger one where it works.
FROM debian:bookworm-slim

# The tools an agent is likely to reach for, plus the ones the service needs.
#
# ca-certificates is not optional: OIDC discovery and the token endpoint are https
# calls to Keycloak, and without the trust store the server refuses to start with
# an opaque x509 error. Agents also fetch over https constantly.
#
# tini is here for one reason: the worker would otherwise be PID 1, and a PID 1
# that does not wait() leaks a zombie for every orphan reparented to it. An agent
# running `npm run dev &` and then hitting a timeout leaves two — the process group
# is killed correctly, but nobody collects the corpses, and nothing reaps them
# lazily either. Measured on a real deployment, still there minutes later.
#
# An in-process reaper was the alternative and is worse here: wait4(-1) races the
# wait() that os/exec already does for every tracked child, and losing that race
# turns a finished command into "waitid: no child processes". With tini as PID 1
# the worker keeps its own children and only genuine orphans reparent past it, so
# there is no race to lose. It also forwards SIGTERM, which the graceful shutdown
# depends on.
#
# Deliberately absent: a compiler toolchain. build-essential is most of a
# quarter-gigabyte, and with glibc wheels the common cases do not need it. Add it
# here if your agents' tasks turn out to.
RUN set -eux; \
    apt-get update; \
    DEBIAN_FRONTEND=noninteractive apt-get install --no-install-recommends -y \
        ca-certificates tzdata tini \
        python3 python3-venv python3-pip \
        git openssh-client curl \
        jq ripgrep less unzip xz-utils procps \
        iputils-ping traceroute dnsutils netcat-openbsd; \
    rm -rf /var/lib/apt/lists/*

# The UID is pinned rather than left to useradd: a Pod with runAsNonRoot set
# refuses to start on a non-numeric USER, because the kubelet cannot verify a name
# is not root. 65532 is the conventional "nonroot" id.
RUN set -eux; \
    groupadd -g 65532 executor; \
    useradd -u 65532 -g 65532 -m -d /home/executor -s /usr/sbin/nologin executor

# Passwd and group entries for the per-agent id range the worker hands out.
#
# Without them getpwuid() finds nothing, and git, ssh and npm all complain about
# an unknown user in ways that read as a sandbox bug rather than a missing line in
# a file. They have to be generated at build time because the worker's root
# filesystem is read-only at runtime, and the range is fixed in the image for the
# same reason — an operator choosing a different one needs a different image,
# which is the honest coupling rather than a hidden one.
#
# 20000–20999: below 65536, because a pod in a user namespace maps exactly that
# many ids and an id above it could not be mapped.
ARG AGENT_UID_FIRST=20000
ARG AGENT_UID_COUNT=1000
RUN set -eu; \
    end=$((AGENT_UID_FIRST + AGENT_UID_COUNT - 1)); \
    for uid in $(seq "$AGENT_UID_FIRST" "$end"); do \
      echo "agent$uid:x:$uid:$uid::/nonexistent:/usr/sbin/nologin" >> /etc/passwd; \
      echo "agent$uid:x:$uid:" >> /etc/group; \
    done
WORKDIR /app
COPY --from=builder --chown=executor:executor /out/executor ./
COPY --from=builder --chown=executor:executor /out/worker ./
COPY --chown=executor:executor --chmod=0755 ./docker-entrypoint.sh ./
# One image, two commands: /app/data holds the server's default sqlite DSN
# (data/executor.db), and /sandboxes is the worker's default root, which each
# agent gets a subdirectory of. Both are mount points in a real deployment — the
# worker's is an emptyDir, which is what makes its root filesystem read-only.
RUN mkdir -p /app/data && chown -R executor:executor /app
# /sandboxes stays root-owned, unlike /app.
#
# With per-agent ids the worker is container-root without CAP_DAC_OVERRIDE, so it
# is bound by file permissions like anyone else — and a root-owned parent is the
# one it can create agent directories in. What protects an agent's files is the
# 0700 on its own directory, not the mode of the directory above it.
#
# A worker running as 65532 instead (per-agent ids off) needs the mount itself made
# writable for that user: `fsGroup` in a Pod, a tmpfs mode in Compose. Both already
# do, and both override whatever the image says about this path anyway.
RUN mkdir -p /sandboxes && chmod 0755 /sandboxes
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/usr/bin/tini", "--", "./docker-entrypoint.sh"]
CMD ["executor"]

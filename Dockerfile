# Build the manager binary
# Base images are pinned by digest (Scorecard "pinned dependencies");
# Dependabot's docker ecosystem keeps version + digest current together.
FROM golang:1.26.5@sha256:3aff6657219a4d9c14e27fb1d8976c49c29fddb70ba835014f477e1c70636647 AS builder

# Automatic platform arguments provided by Docker BuildKit
ARG TARGETOS
ARG TARGETARCH

# Build metadata, injected into the binary via -ldflags (see cmd/buildinfo.go).
ARG VERSION=dev
ARG GIT_COMMIT=unknown
ARG GIT_DIRTY=0
ARG BUILD_DATE=unknown

# When non-empty, build a coverage-instrumented binary (Go 1.20+ integration
# coverage). Used only for e2e coverage collection; release images leave it unset.
ARG GOCOVER=

WORKDIR /workspaces

# Copy the Go Modules manifests
COPY go.mod go.sum ./
# cache deps before building and copying source so that we don't need to re-download as much
# and so that source changes don't invalidate our downloaded layer. The module
# cache is a BuildKit cache mount so it also survives across builds.
RUN --mount=type=cache,target=/go/pkg/mod go mod download

# Copy the go source
COPY cmd/ cmd/
COPY api/ api/
COPY internal/ internal/

# Build for the target platform. ${GOCOVER:+...} expands to the coverage flags
# only when GOCOVER is non-empty, instrumenting every package in the module.
#
# The Go build and module caches are BuildKit cache mounts, so they persist
# across builds instead of starting empty every time. A source change still
# recompiles (only the changed packages + dependents), but a rebuild no longer
# recompiles the whole module + all deps from scratch — the dominant cost of the
# e2e image-refresh chain, which rebuilds the controller several times per run
# (see docs/design/e2e-ci-runner-sharding-plan.md). Also speeds up local
# `task test-e2e` rebuilds. The cache is content-addressed and keyed by
# GOOS/GOARCH/flags, so cross-arch and coverage builds stay isolated.
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build \
    ${GOCOVER:+-cover -covermode=atomic -coverpkg=github.com/ConfigButler/gitops-reverser/...} \
    -ldflags "-X main.version=${VERSION} -X main.gitCommit=${GIT_COMMIT} -X main.gitDirty=${GIT_DIRTY} -X main.buildDate=${BUILD_DATE}" \
    -o manager ./cmd

# Bundle git and all its shared-library deps (musl libc, openssl, curl, etc.) so that
# the final distroless image can shell out to git for Azure DevOps repositories (which
# require multi_ack capability that go-git v5 does not implement).
FROM alpine:3.24@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b AS git-bundle
RUN set -eux; \
    apk add --no-cache git openssh-client; \
    mkdir -p /bundle/bin /bundle/lib /bundle/usr/lib /bundle/usr/libexec; \
    cp -L /usr/bin/git /bundle/bin/; \
    cp -L /usr/bin/ssh /bundle/bin/; \
    cp -rL /usr/libexec/git-core /bundle/usr/libexec/; \
    cp -L /lib/ld-musl*.so* /bundle/lib/; \
    for f in /usr/bin/git \
             /usr/bin/ssh \
             /usr/libexec/git-core/git-remote-http \
             /usr/libexec/git-core/git-remote-https; do \
        [ -f "$f" ] || continue; \
        ldd "$f" 2>/dev/null \
            | awk '/ => /{ print $3 }' \
            | while read -r so; do \
                [ -f "$so" ] || continue; \
                case "$so" in \
                    /lib/*)     cp -Ln "$so" /bundle/lib/     2>/dev/null || true ;; \
                    /usr/lib/*) cp -Ln "$so" /bundle/usr/lib/ 2>/dev/null || true ;; \
                esac; \
            done; \
    done

FROM alpine:3.24@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b AS sops-downloader
ARG TARGETARCH
# Keep current: the CI image-scan gate fails on fixable CRITICALs in this
# binary's compiled-in deps (old releases ship vulnerable grpc/stdlib builds).
ARG SOPS_VERSION=v3.13.3
RUN apk add --no-cache curl
RUN case "${TARGETARCH}" in \
    amd64)  SOPS_ARCH=amd64 ;; \
    arm64)  SOPS_ARCH=arm64 ;; \
    *) echo "unsupported TARGETARCH: ${TARGETARCH}" && exit 1 ;; \
    esac \
 && curl -fsSL -o /usr/local/bin/sops "https://github.com/getsops/sops/releases/download/${SOPS_VERSION}/sops-${SOPS_VERSION}.linux.${SOPS_ARCH}" \
 && chmod 0555 /usr/local/bin/sops

# Use distroless as minimal base image to package the manager binary
# Refer to https://github.com/GoogleContainerTools/distroless for more details
FROM gcr.io/distroless/static:debug@sha256:e741251ccc55dd6cec4a99ff21c0766df31891fabb4a50727104619a7e6ff4f2
WORKDIR /
COPY --from=builder /workspaces/manager .
COPY --from=sops-downloader /usr/local/bin/sops /usr/local/bin/sops
# git, ssh, and their musl-linked deps for the ADO system-git fallback.
# busybox provides /bin/sh so git can invoke GIT_SSH_COMMAND via shell parsing.
# Alpine's busybox is musl-linked and needs only the libs already copied below.
COPY --from=git-bundle /bin/busybox                 /bin/sh
COPY --from=git-bundle /bundle/bin/git              /usr/bin/git
COPY --from=git-bundle /bundle/bin/ssh              /usr/bin/ssh
COPY --from=git-bundle /bundle/usr/libexec/git-core /usr/libexec/git-core
COPY --from=git-bundle /bundle/lib/                 /lib/
COPY --from=git-bundle /bundle/usr/lib/             /usr/lib/
USER 65532:65532

ENTRYPOINT ["/manager"]

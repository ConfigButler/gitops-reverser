# Build the manager binary
# Base images are pinned by digest (Scorecard "pinned dependencies");
# Dependabot's docker ecosystem keeps version + digest current together.
FROM golang:1.27.1@sha256:512690a5660563b57d37ecc31129e7f136e831db2aed24a1dbeb8ad7380dc0fa AS builder

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

FROM golang:1.27.1@sha256:512690a5660563b57d37ecc31129e7f136e831db2aed24a1dbeb8ad7380dc0fa AS sops-builder

# Automatic platform arguments provided by Docker BuildKit
ARG TARGETOS
ARG TARGETARCH

# SOPS is BUILT here rather than downloaded from its GitHub release.
#
# A Go release binary carries an embedded SBOM — the toolchain version and every
# dependency version it was compiled with — and that is what image scanners read.
# So a prebuilt binary accumulates findings on a clock set by UPSTREAM's release
# cadence, not ours: v3.13.3 was frozen on Go 1.26.5 with x/crypto v0.54.0 and
# grpc v1.82.1, and every stdlib or dependency CVE disclosed since lands on it
# permanently. There is no newer release to bump to, and waiting for one is not a
# mitigation. Scanning the release artifact today reports 14 findings, none of
# them a defect in SOPS's own code.
#
# Building the same upstream tag with the toolchain above, from the pinned module
# in hack/sops-build (which also holds the dependencies past the versions SOPS
# requires), reports zero. The version and the dependency pins live in that
# module's go.mod so Dependabot tracks them; see hack/sops-build/pins.go for why
# each one is held forward.
#
# This does NOT weaken the supply chain. The download it replaces verified
# nothing — no checksum, no signature, just TLS to a URL. Building through the
# module graph verifies every source module against go.sum and the public Go
# checksum database.
WORKDIR /workspaces/sops-build

# Only the module files: the build resolves cmd/sops out of the module cache, and
# pins.go is behind a build tag that is never set, so neither is needed here.
COPY hack/sops-build/go.mod hack/sops-build/go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

# Static, like the manager: the runtime stage is distroless/static. -trimpath so
# the binary does not carry builder paths.
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build \
    -trimpath -ldflags "-s -w" \
    -o /out/sops github.com/getsops/sops/v3/cmd/sops \
    && chmod 0555 /out/sops

# Use distroless as minimal base image to package the manager binary
# Refer to https://github.com/GoogleContainerTools/distroless for more details
FROM gcr.io/distroless/static:debug@sha256:53cd815b916ffc1751285f307bdaa728f459224296e97af342e73e4cebeb41e8
WORKDIR /
COPY --from=builder /workspaces/manager .
COPY --from=sops-builder /out/sops /usr/local/bin/sops
USER 65532:65532

ENTRYPOINT ["/manager"]

# Build the manager binary
FROM golang:1.26.4 AS builder

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
# and so that source changes don't invalidate our downloaded layer
RUN go mod download

# Copy the go source
COPY cmd/ cmd/
COPY api/ api/
COPY internal/ internal/

# Build for the target platform. ${GOCOVER:+...} expands to the coverage flags
# only when GOCOVER is non-empty, instrumenting every package in the module.
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build \
    ${GOCOVER:+-cover -covermode=atomic -coverpkg=github.com/ConfigButler/gitops-reverser/...} \
    -ldflags "-X main.version=${VERSION} -X main.gitCommit=${GIT_COMMIT} -X main.gitDirty=${GIT_DIRTY} -X main.buildDate=${BUILD_DATE}" \
    -o manager ./cmd

FROM alpine:3.23 AS sops-downloader
ARG TARGETARCH
ARG SOPS_VERSION=v3.11.0
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
FROM gcr.io/distroless/static:debug
WORKDIR /
COPY --from=builder /workspaces/manager .
COPY --from=sops-downloader /usr/local/bin/sops /usr/local/bin/sops
USER 65532:65532

ENTRYPOINT ["/manager"]

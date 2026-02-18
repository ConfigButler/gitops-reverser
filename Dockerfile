# Build the manager binary
FROM golang:1.26.0 AS builder

# Automatic platform arguments provided by Docker BuildKit
ARG TARGETOS
ARG TARGETARCH

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

# Build for the target platform
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -o manager cmd/main.go

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

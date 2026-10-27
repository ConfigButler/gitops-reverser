# Mutation-capture lab image. Built by `task lab-build-image` and run on the lab's
# own cluster; it is never part of the product image or the Helm chart.
FROM golang:1.27.3 AS builder

ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspaces

COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ cmd/
COPY api/ api/
COPY internal/ internal/

RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build \
    -o mutation-capture-lab ./cmd/mutation-capture-lab

FROM gcr.io/distroless/static:debug
WORKDIR /
COPY --from=builder /workspaces/mutation-capture-lab .
USER 65532:65532

ENTRYPOINT ["/mutation-capture-lab"]

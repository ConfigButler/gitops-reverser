# Mutation-capture lab image. Built and deployed only by `task lab-e2e`; it is
# never part of the product image, the Helm chart, or the default CI lane.
FROM golang:1.26.5 AS builder

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

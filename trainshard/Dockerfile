# syntax=docker/dockerfile:1.4

FROM golang:1.25.9-alpine3.23 AS builder

ARG GOOS
ARG GOARCH
ARG VERSION=dev

ENV GOOS=${GOOS} \
    GOARCH=${GOARCH} \
    CGO_ENABLED=0 \
    GOCACHE=/root/.cache/go-build \
    GOMODCACHE=/go/pkg/mod

# the chain's own types come from the module next door, so the build starts a directory up
WORKDIR /src
COPY inference-chain ./inference-chain
COPY trainshard ./trainshard

WORKDIR /src/trainshard
RUN --mount=type=cache,id=trainshard-go-build,target=/root/.cache/go-build \
    --mount=type=cache,id=trainshard-go-mod,target=/go/pkg/mod \
    go build -mod=mod -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/trainshardd ./cmd/trainshardd

FROM alpine:3.21

RUN apk add --no-cache ca-certificates xfsprogs

COPY --from=builder /out/trainshardd /usr/local/bin/trainshardd

ENTRYPOINT ["/usr/local/bin/trainshardd"]

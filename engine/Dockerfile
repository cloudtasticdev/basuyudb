# syntax=docker/dockerfile:1.10
# BasuyuDB engine — pure-Go, zero-CGo, distroless (ADR-002).
# Multi-stage: build a static binary, ship it on distroless/static (no shell,
# no libc, minimal attack surface).

# ---- build stage ----
FROM golang:1.24 AS build
WORKDIR /src

# Module graph first for layer caching. third_party holds the local renameio
# shim referenced by a go.mod replace, so it must be present before download.
COPY go.mod go.sum ./
COPY third_party/ ./third_party/
RUN go mod download

COPY . .

# Static, stripped build. CGO disabled (ADR-002). SOURCE_DATE_EPOCH pins time.
ARG VERSION=0.1.0
ENV SOURCE_DATE_EPOCH=0
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/basuyudb ./cmd/basuyudb

# ---- runtime stage ----
FROM gcr.io/distroless/static-debian12:nonroot

LABEL org.opencontainers.image.title="BasuyuDB" \
      org.opencontainers.image.description="Distributed multi-model database: PostgreSQL wire, branch-per-PR, OTel JOIN, vector + FTS. Apache-2.0." \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.source="https://github.com/cloudtasticdev/basuyudb"

COPY --from=build /out/basuyudb /usr/local/bin/basuyudb

# PG wire, OTLP gRPC, OTLP HTTP, Raft transport, gossip, management/health.
EXPOSE 5432 4317 4318 63001 7946 8080

ENV BASUYUDB_DATA_DIR=/data/badger \
    BASUYUDB_RAFT_DIR=/data/raft \
    BASUYUDB_PG_ADDR=:5432 \
    BASUYUDB_OTLP_GRPC_ADDR=:4317

USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/basuyudb"]

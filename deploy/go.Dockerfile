# One image for every Go service: atc-node (sector, tower, registry, standby
# replicas), atc-gateway, atc-maekawa (the comparison baseline) and
# atc-intruder (the mTLS demo).
# Compose picks the binary with `command:`.

# ---- build stage: full Go toolchain, never shipped ----
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/atc-node ./cmd/node && \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/atc-gateway ./cmd/gateway && \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/atc-maekawa ./cmd/maekawa && \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/atc-intruder ./cmd/intruder

# ---- runtime stage: small Alpine image ----
# Alpine rather than distroless so the chaos scripts can run `tc netem`
# (iproute2) inside a node, and the "certs" service can run openssl
# (scripts/gen-certs.sh). Healthchecks use busybox wget.
FROM alpine:3.22
RUN apk add --no-cache iproute2 openssl
COPY --from=build /out/ /usr/local/bin/
EXPOSE 7000 8080 9090
CMD ["atc-node"]

# syntax=docker/dockerfile:1.7
#
# Production image for the monitor daemon.
#
# Two stages:
#   1. build — golang:1.26-alpine, cross-compiles a static linux/arm64 binary.
#              Identical toolchain settings to Dockerfile.build; kept inline
#              here so `docker compose up -d --build` is self-contained on the
#              Pi (no need to prebuild a separate builder image).
#   2. prod  — distroless/static, no shell, no package manager, no libc. Just
#              CA certs, tzdata, a /etc/passwd entry for the nonroot user,
#              and our ~6 MB static binary.

FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build

WORKDIR /src

COPY go.mod ./
RUN go mod download

COPY . .

ARG TARGETOS=linux
ARG TARGETARCH=arm64

RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/monitor ./cmd/monitor

# Lean production runtime — ~2 MB distroless base + ~6 MB static binary.
FROM gcr.io/distroless/static-debian12:nonroot AS runtime

LABEL org.opencontainers.image.source="https://github.com/tokko/volvo-tibber-sync"
LABEL org.opencontainers.image.description="Mirrors Volvo XC60 battery SoC into Tibber mock-car settings so Tibber smart charging sees real state."
LABEL org.opencontainers.image.licenses="MIT"

COPY --from=build /out/monitor /monitor

USER nonroot:nonroot
EXPOSE 8080
VOLUME ["/data"]

ENTRYPOINT ["/monitor"]

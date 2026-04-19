# syntax=docker/dockerfile:1.7

# Build stage — cross-compile a static linux/arm64 binary for Raspberry Pi 5.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build

WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .

ARG TARGETOS=linux
ARG TARGETARCH=arm64

RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/monitor ./cmd/monitor

# Runtime — distroless static, no shell, no libc.
FROM gcr.io/distroless/static-debian12:nonroot AS runtime

COPY --from=build /out/monitor /monitor

USER nonroot:nonroot
EXPOSE 8080
VOLUME ["/data"]

ENTRYPOINT ["/monitor"]

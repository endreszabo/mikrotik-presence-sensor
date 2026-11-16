# syntax=docker/dockerfile:1

FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/mikrotik-presence-sensor ./cmd/mikrotik-presence-sensor

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/mikrotik-presence-sensor /usr/local/bin/mikrotik-presence-sensor
USER nonroot:nonroot
# health (HTTP), syslog (UDP+TCP). Unprivileged ports: the image runs as non-root.
EXPOSE 8080/tcp 5514/udp 5514/tcp
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
  CMD ["/usr/local/bin/mikrotik-presence-sensor", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/mikrotik-presence-sensor"]

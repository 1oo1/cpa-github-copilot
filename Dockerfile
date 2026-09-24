# syntax=docker/dockerfile:1
# Build the embedded UI on the builder's native architecture (no Node/Bun at runtime).
FROM --platform=$BUILDPLATFORM oven/bun:1.3.14 AS web-build
WORKDIR /src/web
COPY web/package.json web/bun.lock ./
RUN bun install --frozen-lockfile
COPY web/ ./
RUN bun run build

# Cross-compile the pure-Go service; Buildx supplies TARGETOS/TARGETARCH.
FROM --platform=$BUILDPLATFORM golang:1.26-bookworm AS go-build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY internal/ ./internal/
COPY cmd/ ./cmd/
COPY web/embed.go ./web/embed.go
COPY --from=web-build /src/web/dist/index.html ./web/dist/index.html
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/copilot-proxy ./cmd/copilot-proxy

# CA roots are required for GitHub HTTPS; tzdata supports the optional TZ setting.
# BusyBox wget supplies the unauthenticated, local-only health probe.
FROM alpine:3.23 AS runtime
RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -g 10001 copilot \
    && adduser -D -H -u 10001 -G copilot copilot \
    && mkdir /data \
    && chown 10001:10001 /data \
    && chmod 0700 /data
COPY --from=go-build /out/copilot-proxy /usr/local/bin/copilot-proxy
COPY LICENSE /usr/share/licenses/copilot-proxy/LICENSE
COPY LICENSES/ /usr/share/licenses/copilot-proxy/third-party/
LABEL org.opencontainers.image.title="Copilot Proxy" \
      org.opencontainers.image.description="Standalone GitHub Copilot proxy with embedded management UI" \
      org.opencontainers.image.licenses="MIT"
USER 10001:10001
WORKDIR /data
EXPOSE 8317
STOPSIGNAL SIGTERM
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD wget --quiet --tries=1 --timeout=3 --output-document=/dev/null http://127.0.0.1:8317/healthz || exit 1
ENTRYPOINT ["/usr/local/bin/copilot-proxy"]
CMD ["--config", "/data/config.yaml"]

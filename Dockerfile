# syntax=docker/dockerfile:1

FROM node:24-alpine AS dash-builder
WORKDIR /src/dashboard
COPY dashboard/package.json dashboard/package-lock.json ./
RUN npm ci
COPY dashboard/ ./
RUN npm run build

FROM golang:1.27-alpine AS go-builder
ARG VERSION=""
ARG COMMIT=none
ARG BUILD_TIME=unknown
WORKDIR /src
COPY VERSION go.mod go.sum ./
RUN go mod download
COPY cmd/ ./cmd/
COPY internal/ ./internal/
COPY --from=dash-builder /src/internal/server/static ./internal/server/static
RUN VER="${VERSION:-v$(tr -d ' \t\r\n' < VERSION)}" \
    && BT="${BUILD_TIME}" \
    && [ "$BT" != "unknown" ] || BT="$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    && CGO_ENABLED=0 go build -trimpath \
      -ldflags="-s -w -X main.version=${VER} -X main.commit=${COMMIT} -X main.buildTime=${BT}" \
      -o /out/verloc ./cmd/verloc

FROM alpine:3.21
LABEL org.opencontainers.image.title="verloc" \
      org.opencontainers.image.description="Self-hosted, single-binary OpenAI-compatible LLM gateway with adaptive routing, SQLite analytics, and an embedded operations dashboard." \
      org.opencontainers.image.url="https://github.com/getverloc/verloc" \
      org.opencontainers.image.source="https://github.com/getverloc/verloc" \
      org.opencontainers.image.documentation="https://github.com/getverloc/verloc#readme" \
      org.opencontainers.image.vendor="getverloc" \
      org.opencontainers.image.licenses="MIT"

RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -g 10001 -S verloc \
    && adduser -u 10001 -S -G verloc -h /var/lib/verloc verloc \
    && mkdir -p /var/lib/verloc \
    && chown -R verloc:verloc /var/lib/verloc \
    && chmod 700 /var/lib/verloc

COPY --from=go-builder /out/verloc /usr/local/bin/verloc
COPY scripts/docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
RUN chmod 755 /usr/local/bin/verloc /usr/local/bin/docker-entrypoint.sh

USER verloc
WORKDIR /var/lib/verloc
VOLUME ["/var/lib/verloc"]
EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD wget -qO- http://127.0.0.1:8080/healthz >/dev/null 2>&1 || exit 1

ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]

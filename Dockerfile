# CogOS Kernel — Multi-stage OCI build
#
# Build:
#   docker build -t cogos-dev/cogos:dev .
#
# Run:
#   docker run -v /path/to/workspace:/workspace \
#              -p 6931:6931 cogos-dev/cogos:dev \
#              serve --workspace /workspace --port 6931

# ── Stage 1: Build ────────────────────────────────────────────────────────────
FROM golang:1.24-alpine AS builder

ARG BUILD_TIME=unknown

WORKDIR /build

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 go build \
    -ldflags="-s -w -X github.com/cogos-dev/cogos/internal/engine.BuildTime=${BUILD_TIME}" \
    -o /cogos ./cmd/cogos

# ── Stage 2: Runtime ──────────────────────────────────────────────────────────
FROM alpine:3.21

RUN apk add --no-cache \
    ca-certificates \
    git \
    curl

RUN addgroup -S cogos && adduser -S cogos -G cogos

WORKDIR /workspace

COPY --from=builder /cogos /usr/local/bin/cogos

RUN mkdir -p .cog/mem .cog/config .cog/ledger \
    && chown -R cogos:cogos /workspace

USER cogos

EXPOSE 6931

HEALTHCHECK --interval=30s --timeout=5s --retries=3 \
    CMD curl -f http://localhost:6931/health || exit 1

ENTRYPOINT ["cogos"]
CMD ["serve", "--port", "6931"]

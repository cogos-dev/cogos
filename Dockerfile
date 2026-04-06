# CogOS v3 Kernel — Multi-stage OCI build
#
# Build:
#   docker build -t cogos/kernel-v3:dev .
#
# Run:
#   docker run -v /path/to/cog-workspace:/path/to/cog-workspace \
#              -p 5200:5200 cogos/kernel-v3:dev \
#              serve --workspace /path/to/cog-workspace --port 5200
#
# Multi-platform:
#   docker buildx build --platform linux/amd64,linux/arm64 -t cogos/kernel-v3:dev .

# ── Stage 1: Build ────────────────────────────────────────────────────────────
FROM golang:1.24-alpine AS builder

ARG BUILD_TIME=unknown

WORKDIR /build

# Copy module files first for layer caching.
COPY go.mod go.sum ./

RUN go mod download

COPY . .

RUN CGO_ENABLED=0 go build \
    -ldflags="-s -w -X main.BuildTime=${BUILD_TIME}" \
    -o /cog-v3 .

# ── Stage 2: Runtime ──────────────────────────────────────────────────────────
FROM alpine:3.21

RUN apk add --no-cache \
    ca-certificates \
    git \
    curl

# Non-root user.
RUN addgroup -S cogos && adduser -S cogos -G cogos

WORKDIR /workspace

COPY --from=builder /cog-v3 /usr/local/bin/cog-v3

# Workspace volume mount point (host workspace is usually mounted elsewhere).
RUN mkdir -p .cog/mem .cog/config .cog/ledger \
    && chown -R cogos:cogos /workspace

USER cogos

# v3 serves on 5200; v2 on 5100 so both can coexist.
EXPOSE 5200

HEALTHCHECK --interval=30s --timeout=5s --retries=3 \
    CMD curl -f http://localhost:5200/health || exit 1

ENTRYPOINT ["cog-v3"]
CMD ["serve", "--port", "5200"]

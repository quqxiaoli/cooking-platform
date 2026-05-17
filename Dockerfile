# ── Stage 1: builder ──────────────────────────────────────────────────────────
FROM golang:1.25-alpine AS builder

WORKDIR /build
# Download dependencies separately for layer caching — invalidated only when go.mod changes.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags="-s -w" \
    -o /server \
    ./cmd/server

# ── Stage 2: runner ───────────────────────────────────────────────────────────
# Minimal alpine image; wget (busybox) is used by the Docker healthcheck.
FROM alpine:3.21

RUN apk add --no-cache ca-certificates tzdata
WORKDIR /app
COPY --from=builder /server ./server
# Embed all config files so CONFIG_PATH env var can select the right one.
COPY configs/ ./configs/

EXPOSE 8080
ENTRYPOINT ["./server"]

# ══════════════════════════════════════════════════════════════
# X-Media Server — Multi-stage Docker Build
# Target: < 30MB Alpine image, single binary (go:embed admin.html)
# ══════════════════════════════════════════════════════════════

FROM golang:1.22-alpine AS builder

RUN apk add --no-cache gcc musl-dev sqlite-dev ca-certificates git

WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Build statically linked binary with go:embed admin panel
RUN CGO_ENABLED=1 GOOS=linux \
    go build -a -installsuffix cgo \
    -ldflags="-s -w -linkmode external -extldflags '-static'" \
    -trimpath \
    -o /x-media-server \
    ./cmd/server

FROM alpine:3.19

RUN apk add --no-cache ca-certificates tzdata curl

WORKDIR /app
COPY --from=builder /x-media-server .

# No web/ folder needed — admin.html is embedded in binary
RUN chmod +x /x-media-server

EXPOSE 50051 35678

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD curl -sf http://localhost:35678/healthz || exit 1

ENTRYPOINT ["./x-media-server"]

# syntax=docker/dockerfile:1
# Multi-stage build for lilbro-whisper Go service (linux/amd64 only)

# ── Builder ────────────────────────────────────────────────────────────────────
FROM golang:1.25.11-alpine AS builder

RUN apk add --no-cache ca-certificates git

WORKDIR /src

# Download dependencies first (cached layer)
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_TIME=unknown

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
    -ldflags="-s -w \
      -X 'github.com/jedwards1230/lil-whisper/internal/version.Version=${VERSION}' \
      -X 'github.com/jedwards1230/lil-whisper/internal/version.Commit=${COMMIT}' \
      -X 'github.com/jedwards1230/lil-whisper/internal/version.BuildTime=${BUILD_TIME}'" \
    -o /lil-whisper \
    .

# ── Final ──────────────────────────────────────────────────────────────────────
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /lil-whisper /lil-whisper
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

# nonroot user (UID 65532) from distroless:nonroot base image
USER 65532:65532

EXPOSE 8081

ENTRYPOINT ["/lil-whisper"]
CMD ["mcp"]

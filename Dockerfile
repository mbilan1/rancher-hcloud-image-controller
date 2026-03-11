# Hcloud Image Controller — multi-stage build
#
# Stage 1: Build the Go binary
# Stage 2: Minimal runtime image (distroless)

FROM golang:1.24.0-alpine AS builder

ARG VERSION=dev

WORKDIR /build

COPY go.mod go.sum ./
COPY cmd/ cmd/
COPY internal/ internal/
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w -X main.version=${VERSION}" -o /hcloud-image-controller ./cmd/controller

# ── Runtime ──────────────────────────────────────────────────────────────────
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /hcloud-image-controller /hcloud-image-controller

USER nonroot:nonroot

ENTRYPOINT ["/hcloud-image-controller"]

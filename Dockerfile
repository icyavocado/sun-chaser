# ── Build stage ────────────────────────────────────────────────────────────
FROM golang:1.21-alpine AS builder

WORKDIR /build

# calcbright is a local module referenced via a replace directive in go.mod
# (replace github.com/icyavocado/calcbright => ../calcbright).
# Both directories must be present so the Go toolchain can resolve the path.
COPY calcbright/ ./calcbright/
COPY sun-chaser/ ./sun-chaser/

WORKDIR /build/sun-chaser

# modernc.org/sqlite is pure Go — CGO_ENABLED=0 produces a fully static binary.
RUN go mod download && \
    CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /sun-chaser .

# ── Runtime stage ──────────────────────────────────────────────────────────
FROM alpine:3.19

# ca-certificates  — HTTPS calls to OWM and Photon APIs
# tzdata           — timezone-aware solar position calculations
RUN apk add --no-cache ca-certificates tzdata

WORKDIR /app

COPY --from=builder /sun-chaser                   /app/sun-chaser
COPY --from=builder /build/sun-chaser/templates   /app/templates
COPY --from=builder /build/sun-chaser/static      /app/static

# Persistent volume mount point for the SQLite database
RUN mkdir -p /app/data

EXPOSE 8080

CMD ["/app/sun-chaser"]

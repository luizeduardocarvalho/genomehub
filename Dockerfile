# ── build ──────────────────────────────────────────────────────────────────
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# pure-Go deps (badger, blake3) → static binary, no libc needed at runtime
RUN CGO_ENABLED=0 go build -o /genomehub .

# ── runtime ────────────────────────────────────────────────────────────────
FROM alpine:3.20
# curl is only for the compose healthcheck / manual poking; ca-certs for future TLS
RUN apk add --no-cache curl ca-certificates
COPY --from=build /genomehub /usr/local/bin/genomehub
# nodes serve and download; data lives under /data (mounted store + catalog)
ENTRYPOINT ["genomehub"]

FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/app ./cmd/app

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata && \
    mkdir -p /etc/mikrotik-route-sync /var/log/mikrotik-route-sync /var/lib/mikrotik-route-sync && \
    adduser -D -H -s /bin/false appuser

COPY --from=build /out/app /usr/local/bin/app
COPY config.yaml /etc/mikrotik-route-sync/config.yaml
RUN chown -R appuser:appuser /etc/mikrotik-route-sync /var/log/mikrotik-route-sync /var/lib/mikrotik-route-sync

VOLUME ["/var/log/mikrotik-route-sync", "/var/lib/mikrotik-route-sync"]
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD wget --no-verbose --tries=1 --spider http://localhost:8080/api/v1/status || exit 1

USER appuser
ENTRYPOINT ["/usr/local/bin/app"]
CMD ["--config", "/etc/mikrotik-route-sync/config.yaml", "--cache", "/var/lib/mikrotik-route-sync/cache.db", "web"]
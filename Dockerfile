FROM golang:1.27.1-alpine AS builder
RUN apk add --no-cache ca-certificates git
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o /out/app ./cmd/app

FROM scratch
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /out/app /app
WORKDIR /data
USER 65534:65534
VOLUME ["/data", "/var/log/mikrotik-route-sync"]
EXPOSE 8080
ENTRYPOINT ["/app", "--config", "/data/config.yaml", "--cache", "/data/cache.db"]
CMD ["web"]

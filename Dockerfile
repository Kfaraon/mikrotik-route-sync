# Stage 1: Build
FROM golang:1.22-alpine AS builder

RUN apk add --no-cache git ca-certificates

WORKDIR /app

# Кэширование зависимостей
COPY go.mod go.sum ./
RUN go mod download

# Копирование исходников
COPY . .

# Статическая сборка
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-w -s" -o /mikrotik-route-sync ./cmd/app

# Stage 2: Runtime (scratch — минимальный образ)
FROM scratch

# Копирование CA-сертификатов для HTTPS
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

# Копирование бинарника
COPY --from=builder /mikrotik-route-sync /mikrotik-route-sync

# Создание непривилегированного пользователя
COPY --from=builder /etc/passwd /etc/passwd
USER nobody

# Рабочая директория
WORKDIR /app

# Точки монтирования
VOLUME ["/data", "/var/log/mikrotik-route-sync"]

# Порт веб-интерфейса
EXPOSE 8080

# Точка входа
ENTRYPOINT ["/mikrotik-route-sync"]
CMD ["web"]

# ==============================================================================
# Stage 1: Builder (Сборка статического бинарного файла)
# ==============================================================================
FROM golang:1.27.1-alpine AS builder

# Устанавливаем только необходимые для сборки инструменты
RUN apk add --no-cache git ca-certificates tzdata

WORKDIR /src

# Кэширование зависимостей (ускоряет повторные сборки)
COPY go.mod go.sum ./
RUN go mod download

# Копируем исходный код
COPY . .

# Собираем статический бинарный файл с оптимизациями и внедрением версии
# Примечание: пути -X должны соответствовать реальным путям в internal/version (будут созданы на след. шаге)
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
    -trimpath \
    -ldflags="-s -w \
    -X github.com/Kfaraon/mikrotik-route-sync/internal/version.Version=$(git describe --tags --always 2>/dev/null || echo 'dev') \
    -X github.com/Kfaraon/mikrotik-route-sync/internal/version.Commit=$(git rev-parse --short HEAD 2>/dev/null || echo 'unknown') \
    -X github.com/Kfaraon/mikrotik-route-sync/internal/version.BuildDate=$(date -u +'%Y-%m-%dT%H:%M:%SZ')" \
    -o /bin/mikrotik-route-sync \
    ./cmd/app

# ==============================================================================
# Stage 2: Runtime (Минимальный безопасный образ)
# ==============================================================================
FROM alpine:3.20 AS runtime

# Устанавливаем только runtime-зависимости: корневые сертификаты (для HTTPS) и часовые пояса
RUN apk add --no-cache ca-certificates tzdata && \
    # Создаем непривилегированного пользователя и группу с фиксированным UID/GID (65534 = nobody)
    addgroup -g 65534 appgroup && \
    adduser -D -H -u 65534 -G appgroup appuser

# Копируем только скомпилированный бинарный файл из builder-стадии
COPY --from=builder /bin/mikrotik-route-sync /usr/local/bin/mikrotik-route-sync

# Переключаемся на непривилегированного пользователя (Security by default)
USER 65534:65534

# Рабочая директория (должна совпадать с точкой монтирования volume для записи кэша/конфигов)
WORKDIR /data

# Порт веб-интерфейса и API
EXPOSE 8080

# Тома для сохранения состояния между перезапусками контейнера
VOLUME ["/data", "/var/log/mikrotik-route-sync"]

# Healthcheck: проверяет доступность встроенного веб-сервера через локальный wget
# /healthz должен возвращать 200 OK без аутентификации (согласно промпту)
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD wget --no-verbose --tries=1 --spider http://127.0.0.1:8080/healthz || exit 1

# Точка входа и команда по умолчанию
# "daemon" запускает всё: web, scheduler, bot (согласно разделу XIII промпта)
ENTRYPOINT ["/usr/local/bin/mikrotik-route-sync"]
CMD ["daemon", "--config", "/data/config.yaml"]

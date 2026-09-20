# ==============================================================================
# Stage 1: Builder (Сборка статического бинарного файла)
# ==============================================================================
# REGISTRY — зеркало Docker Hub. auth.docker.io недоступен из некоторых сетей,
# поэтому по умолчанию используется зеркальная копия official-образов.
# При сборке с обычным доступом к Docker Hub: docker build --build-arg REGISTRY=docker.io/library .
ARG REGISTRY=docker.m.daocloud.io/library

FROM ${REGISTRY}/golang:1.27.1-alpine AS builder

# Версия собирается на хосте и передаётся аргументами (git в образе не нужен)
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown

# Устанавливаем только необходимые для сборки инструменты
RUN apk add --no-cache ca-certificates tzdata

WORKDIR /src

# proxy.golang.org может быть недоступен — используем зеркало
ENV GOPROXY=https://goproxy.cn,direct

# Кэширование зависимостей (ускоряет повторные сборки)
COPY go.mod go.sum ./
RUN go mod download

# Копируем исходный код
COPY . .

# Собираем статический бинарный файл с оптимизациями и внедрением версии
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
    -trimpath \
    -ldflags="-s -w \
    -X github.com/Kfaraon/mikrotik-route-sync/internal/version.Version=${VERSION} \
    -X github.com/Kfaraon/mikrotik-route-sync/internal/version.Commit=${COMMIT} \
    -X github.com/Kfaraon/mikrotik-route-sync/internal/version.BuildDate=${BUILD_DATE}" \
    -o /bin/mikrotik-route-sync \
    ./cmd/app

# ==============================================================================
# Stage 2: Runtime (Минимальный безопасный образ)
# ==============================================================================
FROM ${REGISTRY}/alpine:3.20 AS runtime

# Устанавливаем только runtime-зависимости: корневые сертификаты (для HTTPS) и часовые пояса
# Отдельный пользователь не создаётся: в Alpine 3.20 UID/GID 65534 уже заняты
# системным nobody:nobody — используем его (USER ниже).
RUN apk add --no-cache ca-certificates tzdata && \
    mkdir -p /data /var/log/mikrotik-route-sync && \
    chown 65534:65534 /data /var/log/mikrotik-route-sync

# Копируем только скомпилированный бинарный файл из builder-стадии
COPY --from=builder /bin/mikrotik-route-sync /usr/local/bin/mikrotik-route-sync

# Рабочая директория (совпадает с точкой монтирования volume для кэша/конфигов)
WORKDIR /data

# Непривилегированный пользователь nobody (65534:65534)
USER 65534:65534

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

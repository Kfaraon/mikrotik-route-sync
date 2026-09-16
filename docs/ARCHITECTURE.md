# Архитектура MikroTik Route Sync

## Обзор

MikroTik Route Sync — это приложение на Go для автоматического управления маршрутами на MikroTik RouterOS v7. Система собирает IP-диапазоны сервисов из различных источников, валидирует их, агрегирует и синхронизирует с маршрутизатором через REST API.

## Архитектурные принципы

### 1. Изоляция по сервисам (Service Isolation)

Каждый сервис обрабатывается независимо:

- Маршруты помечаются комментарием `AUTO:<ServiceName>`
- Операции ограничены только маршрутами конкретного сервиса
- Ошибка в одном сервисе не влияет на другие
- REST-запросы фильтруются: `GET /rest/ip/route?comment=AUTO:instagram`

### 2. Fail-closed

При любой неопределённости или ошибке:

- Существующие маршруты не удаляются
- Синхронизация прерывается с ошибкой
- Отправляется уведомление в Telegram
- Пропуск логируется с уровнем `WARN`

### 3. Best-effort rollback

Перед применением изменений создаётся снимок:

- При сбое выполняется откат к исходному состоянию
- Rollback через REST API (DELETE добавленного, POST удалённого)
- Частичный rollback помечает сервис как `degraded`
- Отправляется алерт с высоким приоритетом

### 4. Safe-diff

Защита от массового удаления:

- Проверка соотношения `deleted / existing`
- Прерывание при превышении `max_delete_ratio` (по умолчанию 50%)
- Требование ручного подтверждения через `--force`
- Если `existing > 0`, а новый набор пуст — прерывать всегда

### 5. Idempotency

Повторный запуск с теми же данными:

- Не приводит к изменениям в RouterOS
- Сравнение по нормализованному CIDR (network + prefix length)
- Без учёта хостовых бит

### 6. Кэширование

Результаты ASN-resolution, WHOIS, RDAP кэшируются:

- Хранилище: bbolt
- TTL задаётся в конфиге (по умолчанию 24 часа)
- Периодическая очистка (по умолчанию каждый час)
- Кэш — оптимизация, не источник истины

### 7. Минимизация прав

- Отдельный пользователь API для RouterOS
- Права: `read`, `write`, `rest-api`, `api`, `policy`
- Ограничение по IP через firewall
- Web UI слушает только LAN/loopback
- Docker/LXC без `--privileged`

## Структура проекта

    cmd/
      app/              — CLI entrypoint (cobra)
    internal/
      aggregator/       — агрегация CIDR (Radix Tree, инварианты)
      audit/            — аудит изменений конфигурации
      bot/              — Telegram-бот (состояние, callback, auth)
      classifier/       — выбор метода сбора (CDN, ASN, dynamic)
      collectors/       — источники сетей (asn, cdn, dynamic, whois, static)
      config/           — конфигурация (viper + yaml, hot-reload)
      core/             — оркестратор синхронизации (syncer, diff)
      history/          — история синхронизаций (bbolt)
      logging/          — slog + rotation + redaction
      mikrotik/         — RouterOS REST API (client, transaction, rollback)
      notifier/         — уведомления (Telegram)
      resolver/         — DNS / ASN resolution (rdap, bgpview, ripestat)
      scheduler/        — расписания (cron + human-readable)
      storage/          — bbolt cache
      validator/        — фильтрация сетей (RFC, ASN-проверка)
      version/          — версия приложения
      web/              — Web UI / REST API / WebSocket
    pkg/
      cidrutil/         — утилиты CIDR (нормализация, вложенность)
      humanize/         — форматирование (длительность, размеры)
    docs/
      openapi.yaml      — OpenAPI 3.1 спецификация
      ARCHITECTURE.md   — описание архитектуры
      THREAT_MODEL.md   — модель угроз
      SECURITY.md       — безопасность
      CHANGES.md        — история изменений

## Поток данных

1. Пользователь или Планировщик инициирует синхронизацию
2. Classifier определяет метод сбора для сервиса
3. Collector собирает сырые CIDR из источника
4. Validator выполняет многоуровневую фильтрацию
5. Aggregator агрегирует через Radix Tree
6. Syncer вычисляет diff и выполняет safety-check
7. MikroTik Client применяет изменения через REST API
8. Notifier отправляет уведомление в Telegram

## Компоненты

### Collector

Собирает IP-диапазоны из источников:

- **ASN** — BGPView, RIPEstat, RDAP (основной метод для крупных сервисов)
- **CDN** — Cloudflare, AWS, Google (официальные статические списки)
- **Dynamic** — DNS-резолвинг + официальные JSON (Google, YouTube, Telegram)
- **WHOIS** — DNS → ASN → все префиксы (для мелких сайтов)
- **Static** — произвольные URL (antifilter, re:filter)

### Validator

Многоуровневая валидация:

1. **Синтаксис** — формат CIDR, нормализация
2. **Приватные диапазоны** — RFC 1918, IANA (10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16, CGNAT, loopback, link-local, TEST-NET, multicast, reserved)
3. **Принадлежность ASN** — проверка через RDAP
4. **Ширина префикса** — IPv4: min 8, IPv6: min 16
5. **Пользовательские фильтры** — exclude, include_only, max_prefixes
6. **Дедупликация** — удаление дубликатов и вложенных префиксов

### Aggregator

Агрегация CIDR через Radix Tree:

- Сложность: O(n·L), где L — длина префикса
- Объединение только sibling-префиксов
- Проверка инварианта: сумма адресов до = сумма после
- Проверка инварианта: каждый исходный префикс покрыт
- При нарушении — откат, использование неагрегированного списка

### Syncer

Оркестрация синхронизации:

- Вычисление diff (add / remove / unchanged)
- Safety-check (max_delete_ratio)
- Транзакционное применение (сначала POST, затем DELETE)
- Rollback при сбое
- Mutex на сервис (защита от наложения)
- Параллельность ограничена max_concurrent

### MikroTik Client

REST API клиент:

- HTTP/HTTPS (TLS 1.2+)
- Circuit Breaker (5 ошибок → Open на 60 сек)
- Retry с exponential backoff + jitter (3 попытки)
- Rate limiting (20 запросов/сек)
- Timeout на каждый запрос (30 сек)
- Verify SSL по умолчанию

### Scheduler

Управление расписаниями:

- Три уровня приоритета: сервис → группа → глобальное
- Форматы: cron (5 полей), human-readable (every 6h, daily at 03:00)
- Специальные: manual, disabled, inherit
- Hot-reload через SIGHUP
- Часовой пояс из конфига (IANA)
- Учёт DST-переходов

### Telegram Bot

Управление через Telegram:

- Команды: /start, /menu, /status, /sync, /schedule, /help
- Inline-кнопки с состоянием сценария
- Авторизация только по authorized_chat_ids
- Rate limit (1 сообщение/сек на chat_id)
- Уведомления: старт, прогресс, результат, ошибки, weekly report

### Web UI

Встроенный дашборд:

- Страницы: Dashboard, Services, Schedules, Settings, Logs, History
- Фреймворк: net/http + chi
- Шаблоны: html/template + go:embed
- CSS: Pico CSS (встроенный)
- JS: HTMX + vanilla JS
- WebSocket для live-обновлений
- Basic Auth + session cookie + CSRF

## Безопасность

### Конфигурация

- Права файла `0600`, директории `0700`
- Проверка прав при старте, отказ при нарушении
- Атомарная запись через tmpfile + fsync + rename
- Валидация перед применением
- Hot-reload с rollback при ошибке
- Маскирование секретов в CLI/Web (последние 4 символа)
- Аудит изменений (без записи значений)

### Сеть

- Web UI слушает только явно указанный адрес (по умолчанию 127.0.0.1:8080)
- Проверка allowed_cidrs для всех входящих
- Проверка Origin/Referer для изменяющих запросов
- TLS ≥ 1.2, modern cipher suites
- HSTS при HTTPS
- Rate limiting (login: 5/мин, API: 100/мин, WS: 10/IP)
- Security headers (CSP, X-Frame-Options: DENY, X-Content-Type-Options: nosniff)
- Ограничение размера запросов (≤1 МБ)

### MikroTik

- Отдельный пользователь API с минимальными правами
- Ограничение по IP через firewall на RouterOS
- Verify SSL по умолчанию (предупреждение при false)
- Rate limiting в клиенте
- Обязательная проверка ответа (HTTP 200 + отсутствие error)
- Circuit Breaker
- Retry с exponential backoff + jitter
- Никогда не логировать пароль и Authorization

### Внешние API

- Таймауты на все HTTP-запросы (15 сек)
- Ограничение размера ответа (50 МБ через io.LimitReader)
- Проверка Content-Type перед парсингом
- Ограничение редиректов (≤3)
- Проверка TLS-сертификата (без InsecureSkipVerify)
- Sanitization URL (только http/https)
- Защита от SSRF (блокировка private/loopback/link-local)
- Circuit Breaker для каждого источника

### Telegram

- Авторизация только по authorized_chat_ids
- Не отвечать неавторизованным (не раскрывать существование)
- Rate limit на команды
- Логирование попыток неавторизованного доступа с WARN
- Не передавать содержимое секретов

### Контейнер (Docker)

- Multi-stage build (builder: golang:1.27-alpine, runtime: scratch)
- Static binary (CGO_ENABLED=0, -trimpath, -ldflags="-s -w")
- Non-root пользователь (USER 65534:65534)
- Read-only root filesystem (кроме /var/log и /data)
- cap_drop: [ALL], security_opt: [no-new-privileges:true]
- Ограничение ресурсов (CPU, memory)
- Healthcheck через /healthz
- Volume для /data (bbolt cache) и /var/log
- Секреты через ENV или Docker secrets, не в образе

### Защита от утечек

- Redaction в логах: password, token, secret, api_key, authorization, cookie
- Redaction в ошибках, отправляемых в Telegram / API
- Redaction в CLI-выводе
- Ограничение verbose-логирования HTTP-заголовков

### Защита от supply chain

- go.sum зафиксирован
- govulncheck и gosec — обязательные шаги при релизе
- Зависимости — только популярные, с активной поддержкой
- Ежегодный аудит зависимостей

### Защита от небезопасных входных данных

- Валидация имени сервиса: `^[a-z0-9][a-z0-9_-]{0,63}$`
- Валидация доменов: RFC 1035 + IDN
- Валидация ASN: `^AS\d{1,10}$`
- Валидация CIDR: только корректный формат
- Валидация URL: только http/https
- Валидация cron: только через robfig/cron/v3
- Экранирование RouterOS-комментариев (без ", \, \n)

## Отказоустойчивость

### Retry

- Экспоненциальная задержка: base_delay * 2^attempt + jitter
- По умолчанию: base_delay=1s, max_delay=30s, max_attempts=3
- Retry только для идемпотентных операций и сетевых ошибок (5xx, timeout, connection refused)
- Не retry для 4xx (кроме 429)

### Circuit Breaker

- Состояния: Closed → Open → Half-Open → Closed
- Порог открытия: 5 ошибок подряд
- Таймаут Open: 60 сек
- Полу-открытое: 1 пробный запрос
- Отдельный breaker для каждого внешнего сервиса (MikroTik, BGPView, RDAP, Telegram)

### Восстановление после краха

- Все критические операции — с записью состояния в bbolt
- При старте — проверка незавершённых транзакций
- Попытка восстановления или логирование
- Graceful shutdown по SIGTERM/SIGINT: завершение текущих операций, закрытие соединений, flush логов

### Бэкапы

- Перед каждой синхронизацией — снимок текущих маршрутов сервиса в bbolt (bucket snapshots, TTL 7 дней)
- Экспорт маршрутов сервиса в JSON: `app backup <service>`
- Восстановление из снимка: `app restore <service> <snapshot_id>`

## Наблюдаемость

### Логирование

- Формат: JSON через log/slog
- Вывод: stdout + файл (одновременно)
- Ротация: gopkg.in/natefinch/lumberjack.v2
- Параметры: max_size_mb=10, max_files=5, max_total_mb=50, compress=true, level=info
- Структурированные поля: service, method, duration_ms, added, removed, unchanged, error, request_id, user
- Запрещено логировать: пароли, токены, Authorization, cookie

### Tracing (опционально)

- OpenTelemetry OTLP → Jaeger/Tempo (по флагу tracing.enabled)
- Propagate trace_id в логи и HTTP-заголовки
- Sampling 10% по умолчанию

### Health checks

- `/healthz` — процесс жив (без авторизации)
- `/readyz` — MikroTik доступен, bot подключён, cache открыт

## Масштабирование

### Ограничения ресурсов

- ≤128 МБ RAM
- ≤1 CPU
- Статический бинарник (без внешних зависимостей)
- LXC-контейнер на Proxmox (x86_64)

### Кэширование

- bbolt для ASN/WHOIS/RDAP результатов
- TTL 24 часа (настраивается)
- Периодическая очистка (каждый час)
- Кэш — оптимизация, не источник истины для удаления маршрутов

### Параллелизм

- Mutex на сервис — защита от наложения
- Ограничение общей параллельности через scheduler.max_concurrent
- Повторный запуск по тому же сервису пропускается с WARN
- Ошибка одного сервиса не останавливает остальные

## Расширение

### Добавление нового коллектора

1. Реализовать интерфейс `Collector` в `internal/collectors/`
2. Зарегистрировать в `Registry` (`internal/collectors/registry.go`)
3. Добавить в `Classifier` (`internal/classifier/classifier.go`)
4. Обновить конфигурацию (если нужны параметры)

### Добавление нового метода валидации

1. Добавить в `Validator.Validate()` (`internal/validator/validator.go`)
2. Настроить через конфигурацию (если нужны параметры)
3. Обновить документацию

### Интеграция с новыми системами

- REST API для внешних интеграций (см. `docs/openapi.yaml`)
- Webhook уведомления (через Notifier)
- OpenTelemetry для tracing (через Tracing)
- Prometheus метрики (опционально, через middleware)

## Зависимости

### Основные

- `github.com/spf13/cobra` — CLI
- `github.com/spf13/viper` + `gopkg.in/yaml.v3` — конфигурация
- `github.com/robfig/cron/v3` — планировщик
- `go.etcd.io/bbolt` — кэш/хранилище
- `github.com/go-telegram-bot-api/telegram-bot-api/v5` — Telegram
- `github.com/gorilla/websocket` — WebSocket
- `github.com/go-chi/chi/v5` — роутинг
- `gopkg.in/natefinch/lumberjack.v2` — ротация логов
- `github.com/sony/gobreaker` — Circuit Breaker
- `github.com/hashicorp/go-retryablehttp` — Retry
- `golang.org/x/time/rate` — Rate limiting

### Стандартная библиотека

- `net/http`, `net`, `encoding/json`, `crypto/tls`
- `log/slog`, `html/template`, `embed`
- `crypto/rand`, `crypto/subtle`

## Сборка и развёртывание

### Сборка

    # Статический бинарник
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w \
      -X github.com/Kfaraon/mikrotik-route-sync/internal/version.Version=1.0.0 \
      -X github.com/Kfaraon/mikrotik-route-sync/internal/version.Commit=$(git rev-parse --short HEAD) \
      -X github.com/Kfaraon/mikrotik-route-sync/internal/version.BuildDate=$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
      -o mikrotik-route-sync ./cmd/app

### Docker

    # Multi-stage build
    FROM golang:1.27-alpine AS builder
    WORKDIR /build
    COPY . .
    RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /app ./cmd/app

    FROM scratch
    COPY --from=builder /app /app
    USER 65534:65534
    ENTRYPOINT ["/app"]

### Docker Compose

    version: '3.8'
    services:
      mikrotik-route-sync:
        build: .
        container_name: mikrotik-route-sync
        restart: unless-stopped
        read_only: true
        tmpfs:
          - /tmp
        volumes:
          - ./data:/data
          - ./logs:/var/log
        environment:
          - MRS_MIKROTIK_PASSWORD_FILE=/run/secrets/mikrotik_password
          - MRS_TELEGRAM_BOT_TOKEN_FILE=/run/secrets/telegram_token
        secrets:
          - mikrotik_password
          - telegram_token
        deploy:
          resources:
            limits:
              cpus: '1.0'
              memory: 128M
        security_opt:
          - no-new-privileges:true
        cap_drop:
          - ALL

### Makefile

    .PHONY: build lint test docker release

    build:
        CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o bin/mikrotik-route-sync ./cmd/app

    lint:
        golangci-lint run
        gosec ./...

    test:
        go test -v -race -coverprofile=coverage.out ./...

    docker:
        docker build -t mikrotik-route-sync:latest .

    release:
        @echo "Building release..."
        $(MAKE) lint
        $(MAKE) test
        $(MAKE) build
        @echo "Release ready in bin/"

## Лицензия

MIT License. См. файл `LICENSE`.

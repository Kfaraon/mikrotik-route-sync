# mikrotik-route-sync

> Легковесный сервис на Go для автоматического управления маршрутами **MikroTik RouterOS v7** через REST API.

![Go](https://img.shields.io/badge/Go-1.27-blue)
![License](https://img.shields.io/badge/license-MIT-green)
![RouterOS](https://img.shields.io/badge/RouterOS-v7-red)

Пользователь указывает сервис, домен, IP-адрес или ASN. Приложение определяет подходящий источник сетей, собирает и валидирует CIDR, безопасно агрегирует их и синхронизирует **только маршруты выбранного сервиса**. Каждый управляемый маршрут помечается комментарием `AUTO:<service>`.

> **Главный принцип проекта:** один сервис — одна изолированная область синхронизации. Ошибка при обработке `youtube` не должна затрагивать маршруты `instagram`, `cloudflare` или любого другого сервиса.

---

## Содержание

- [Возможности](#возможности)
- [Как работает синхронизация](#как-работает-синхронизация)
- [Требования](#требования)
- [Быстрый старт](#быстрый-старт)
- [CLI](#cli)
- [Управление сервисами](#управление-сервисами)
- [Расписания](#расписания)
- [Конфигурация](#конфигурация)
- [Изоляция маршрутов](#изоляция-маршрутов)
- [Безопасность](#безопасность)
- [Отказоустойчивость](#отказоустойчивость)
- [Snapshots](#snapshots)
- [Telegram-бот](#telegram-бот)
- [Web UI](#web-ui)
- [REST API](#rest-api)
- [Логирование](#логирование)
- [Кэш](#кэш)
- [Docker](#docker)
- [Proxmox LXC](#proxmox-lxc)
- [Разработка](#разработка)
- [Лицензия](#лицензия)

---

## Возможности

- Автоматическая классификация источника: `asn`, `cdn`, `dynamic`, `whois`, `static_url`
- Определение ASN по домену / IP и работа с ASN, указанным напрямую
- Резолвинг: BGPView с fallback на RIPEstat и RDAP
- Официальные источники: Cloudflare, AWS CloudFront, Google, Fastly, Akamai
- DNS/dynamic-сбор для сервисов с меняющейся инфраструктурой
- Пользовательские списки через `static_url`
- Кэш ASN и префиксов в `bbolt`
- Фильтрация приватных, link-local, multicast и слишком широких сетей
- Безопасная CIDR-агрегация только настоящих sibling-префиксов
- Проверка сохранности множества IP-адресов после агрегации
- Инкрементальная diff-синхронизация RouterOS
- Точная изоляция по комментарию `AUTO:<service>`
- Fail-closed: при пустом / ошибочном результате существующие маршруты не удаляются
- Best-effort rollback при ошибке применения изменений
- Retry и Circuit Breaker для RouterOS REST API
- CLI на Cobra
- Трёхуровневые расписания: сервис → группа → глобальное
- Защита от параллельного запуска одного сервиса
- Ограничение общей параллельности через `max_concurrent`
- Telegram-бот с авторизацией и командами
- Встроенный Web UI и REST API
- WebSocket для live-обновлений
- Basic Auth, CSRF и ограничение Web UI по CIDR
- JSON-логирование через `log/slog` и ротация логов
- Статическая сборка `CGO_ENABLED=0`
- Docker (Alpine) и развёртывание в Proxmox LXC

---

## Как работает синхронизация

    service / domain / IP / ASN
                │
                ▼
         resolver + classifier
                │
                ▼
     ASN / CDN / dynamic / WHOIS / static_url
                │
                ▼
           collection
                │
                ▼
     validation + exclusions
                │
                ▼
       safe CIDR aggregation
                │
                ▼
     GET RouterOS routes with exact AUTO:<service>
                │
                ▼
           calculate diff
           ┌──────┴──────┐
           ▼             ▼
         delete          add
           └──────┬──────┘
                  ▼
          result / rollback

Если после сбора и валидации не осталось ни одной сети, приложение завершает синхронизацию с ошибкой и **не изменяет текущие маршруты MikroTik**.

---

## Требования

- **Go 1.27.1**
- MikroTik RouterOS v7 с доступным **REST API**
- Отдельный RouterOS-пользователь с минимально необходимыми правами
- Доступ приложения к DNS и настроенным внешним источникам (BGPView, RIPEstat, официальные CDN-списки)
- Для Docker / LXC — постоянное хранилище для конфигурации, кэша и логов

---

## Быстрый старт

### 1. Клонирование и конфигурация

    git clone https://github.com/Kfaraon/mikrotik-route-sync.git
    cd mikrotik-route-sync

    cp config.example.yaml config.yaml
    chmod 600 config.yaml

Минимально настройте подключение к MikroTik:

    mikrotik:
      host: 192.168.88.1
      port: 443
      username: api
      password: CHANGE_ME
      use_ssl: true
      verify_ssl: true
      gateway: wg-cz-vpn
      routing_table: main
      distance: 2
      comment_prefix: AUTO

> Для production рекомендуется использовать доверенный TLS-сертификат и `verify_ssl: true`.

### 2. Сборка

    go mod tidy
    go test ./...
    CGO_ENABLED=0 go build -trimpath -o app ./cmd/app

### 3. Проверка подключения

    ./app --config config.yaml test-mikrotik

### 4. Добавление первого сервиса

    ./app --config config.yaml add-service cloudflare

### 5. Dry-run (без применения изменений)

    ./app --config config.yaml sync --dry-run

### 6. Применение

    ./app --config config.yaml sync

### 7. Проверка в RouterOS

    /ip/route/print where comment="AUTO:cloudflare"

### 8. Запуск как сервис

    ./app --config config.yaml daemon

---

## CLI

| Команда | Назначение |
|---------|------------|
| `app sync` | Синхронизировать все сервисы |
| `app sync --service instagram` | Синхронизировать один сервис |
| `app sync --group social` | Синхронизировать группу |
| `app sync --dry-run` | Рассчитать изменения без применения |
| `app sync --service youtube --force` | Обойти safety-check (max_delete_ratio) |
| `app diff <service>` | Показать diff в JSON |
| `app list` | Список сервисов и их эффективных расписаний |
| `app info <service>` | Информация о сервисе (расписание, overrides) |
| `app add-service <service>` | Добавить и первоначально синхронизировать сервис |
| `app remove-service <service>` | Удалить сервис и только его управляемые маршруты |
| `app backup <service>` | Экспорт маршрутов сервиса в JSON |
| `app restore <service> --from-file f.json --force` | Восстановить из файла |
| `app restore <service> --from-snapshot <id> --force` | Восстановить из снапшота |
| `app snapshots list <service>` | Список снапшотов сервиса |
| `app snapshots delete <service> <id>` | Удалить снапшот |
| `app snapshots cleanup <service> --ttl 168` | Очистка старых снапшотов |
| `app schedule list` | Показать эффективные расписания |
| `app schedule reload` | Перечитать расписания |
| `app bot` | Запустить Telegram-бота |
| `app web` | Запустить Web UI |
| `app daemon` | Запустить всё (bot + web + scheduler) |
| `app config validate` | Проверить конфигурацию |
| `app config reload` | Отправить SIGHUP для перезагрузки |
| `app config edit` | Открыть конфиг в `$EDITOR` |
| `app config get <key>` | Прочитать параметр (секреты маскируются) |
| `app logs tail -n 100` | Последние строки лога |
| `app logs tail -f` | Следить за логом |
| `app logs clear` | Очистить логи |
| `app logs size` | Размер и количество файлов логов |
| `app test-telegram` | Проверить Telegram |
| `app test-mikrotik` | Проверить RouterOS REST API |
| `app test-dns <domain>` | Проверить резолвинг |
| `app version` | Версия и информация о сборке |

**Глобальные флаги:**

    ./app --config /path/to/config.yaml <command>

---

## Управление сервисами

### Добавление

    ./app add-service instagram
    ./app add-service youtube
    ./app add-service cloudflare

Можно передать домен, IP или ASN — resolver / classifier выберет подходящий метод.

### Переопределения (overrides)

Для нестандартных сервисов используйте `overrides` в `config.yaml`:

    overrides:
      rutor:
        domains:
          - rutor.info
          - rutor.org
        max_asn_prefixes: 50
        exclude:
          - 1.2.3.0/24

      example-static:
        method: static_url
        static_url: https://example.org/prefixes.txt

      youtube:
        method: dynamic
        domains: [youtube.com, googlevideo.com, ytimg.com]
        also_cdn: [google]

> `max_asn_prefixes` особенно важен для небольших сайтов на shared-инфраструктуре: он ограничивает риск случайно добавить слишком большое количество сетей ASN.

### Удаление

    ./app remove-service instagram
    ./app remove-service rutor --force

Удаляются **только** маршруты с комментарием `AUTO:<service>`. Маршруты других сервисов и обычные пользовательские маршруты не затрагиваются.

---

## Расписания

Приоритет: **service → group → global**

Поддерживаются:
- **cron** (5 полей): `0 */6 * * *`
- **интервалы:** `every 6h`, `every 30m`
- **человекочитаемые:** `daily at 03:00`, `weekly on sunday at 04:00`
- **специальные:** `manual`, `disabled`, `inherit`

### Пример

    schedules:
      global: "every 6h"

      groups:
        social:
          schedule: "daily at 03:00"
          services: [instagram, telegram]
        video:
          schedule: "every 12h"
          services: [youtube]
        torrents:
          schedule: "weekly on sunday at 05:00"
          services: [rutor]

      services:
        instagram: {schedule: "every 4h"}
        cloudflare: {schedule: "every 1h"}
        rutor: {schedule: "manual"}

### Параллельность

    scheduler:
      parallel: true
      max_concurrent: 3
      reload_interval: 1m

Один и тот же сервис защищён от наложения запусков (mutex).

---

## Конфигурация

Полный рабочий шаблон — в `config.example.yaml`.

### Основные разделы

    timezone: Europe/Moscow

    logging:
      level: info
      file: /var/log/mikrotik-route-sync/app.log
      max_size_mb: 10
      max_files: 5
      max_total_mb: 50
      compress: true
      also_stdout: true

    mikrotik:
      host: 192.168.88.1
      port: 443
      username: api
      password: "CHANGE_ME"
      use_ssl: true
      verify_ssl: true
      timeout: 30s
      gateway: wg-cz-vpn
      routing_table: main
      distance: 2
      comment_prefix: AUTO
      rate_limit: 20

    telegram:
      enabled: false
      bot_token: ""
      chat_id: ""
      authorized_chat_ids: []
      rate_limit: 1

    web:
      enabled: true
      listen: "127.0.0.1:8080"
      allowed_cidrs:
        - 127.0.0.0/8
        - 192.168.0.0/16
        - 10.0.0.0/8
        - 172.16.0.0/12
      trusted_proxies: []
      auth:
        enabled: true
        username: admin
        password: "CHANGE_ME"
      session_timeout: 24h
      csrf_enabled: true
      security_headers: true

    scheduler:
      parallel: true
      max_concurrent: 3
      reload_interval: 1m
      cache_ttl: 24h
      cache_purge: "every 1h"

    safety:
      max_delete_ratio: 0.5
      require_confirmation_over: 100
      min_prefix_v4: 8
      min_prefix_v6: 16
      allow_host_routes: false
      max_asn_prefixes: 100

    retry:
      max_attempts: 3
      base_delay: 1s
      max_delay: 30s
      jitter: true

    external:
      http_timeout: 15s
      max_response_mb: 50
      bgpview_api_key: ""
      akamai_api_key: ""
      rdap_timeout: 10s
      resolver: "1.1.1.1:53"

    snapshots:
      enabled: true
      ttl: 168h
      max_count: 50

    schedules:
      global: "every 6h"

    services:
      - cloudflare

    overrides: {}

### Переменные окружения

Секреты можно передать через ENV (приоритет над `config.yaml`):

    MRS_MIKROTIK_PASSWORD
    MRS_TELEGRAM_BOT_TOKEN
    MRS_WEB_PASSWORD

> `config.yaml` намеренно добавлен в `.gitignore` и должен иметь права **0600**.

---

## Изоляция маршрутов

Для каждого сервиса используется отдельный комментарий:

    AUTO:instagram
    AUTO:youtube
    AUTO:cloudflare

При синхронизации `instagram` приложение работает **только** с маршрутами `AUTO:instagram`. Маршруты других сервисов и обычные пользовательские маршруты не участвуют в diff и не удаляются.

---

## Безопасность

Перед production-развёртыванием:

- Создайте отдельного RouterOS API-пользователя с минимальными правами
- Ограничьте доступ пользователя по IP / firewall
- Используйте HTTPS и `verify_ssl: true`
- Оставляйте `config.yaml` с правами `0600`
- Не коммитьте `config.yaml`
- Замените все `CHANGE_ME`
- Ограничьте Web UI через `allowed_cidrs`
- Используйте сложный пароль Web UI
- Ограничьте Telegram через `authorized_chat_ids`
- Выполните `sync --dry-run` перед первой синхронизацией
- Сделайте независимый backup конфигурации RouterOS

Подробнее: [docs/SECURITY.md](docs/SECURITY.md), [docs/THREAT_MODEL.md](docs/THREAT_MODEL.md), [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)

---

## Отказоустойчивость

- **Fail-closed:** при любой неопределённости существующие маршруты не изменяются
- **Safe-diff:** массовое удаление блокируется при `deleted / existing > max_delete_ratio` (по умолчанию 0.5)
- **Best-effort rollback:** при ошибке применения выполняется откат к исходному состоянию
- **Retry** с экспоненциальной задержкой и jitter
- **Circuit Breaker** для внешних сервисов (MikroTik, BGPView, RDAP, Telegram)
- **Graceful shutdown** по SIGTERM / SIGINT
- **Hot-reload** конфигурации по SIGHUP

> Автоматизацию маршрутизации нельзя считать заменой резервной копии RouterOS.

---

## Snapshots

Перед каждой синхронизацией создаётся снимок текущих маршрутов сервиса в `bbolt`.

    # Список снапшотов
    ./app snapshots list instagram

    # Восстановление из снапшота
    ./app restore instagram --from-snapshot <id> --force

    # Экспорт маршрутов в файл
    ./app backup instagram -o instagram-backup.json

    # Очистка старых снапшотов
    ./app snapshots cleanup instagram --ttl 168

Настройка:

    snapshots:
      enabled: true
      ttl: 168h
      max_count: 50

---

## Telegram-бот

### Настройка

    telegram:
      enabled: true
      bot_token: "YOUR_BOT_TOKEN"
      chat_id: "123456789"
      authorized_chat_ids:
        - "123456789"
      rate_limit: 1

### Запуск

    ./app bot        # только бот
    ./app daemon     # бот + web + scheduler

### Команды

    /start      — начать работу
    /menu       — главное меню
    /status     — статус сервисов
    /sync       — синхронизация
    /schedule   — расписания
    /help       — справка

> Бот проверяет `authorized_chat_ids`. Сообщения от неразрешённых chat ID игнорируются и логируются.

---

## Web UI

### Запуск

    ./app web        # только web UI
    ./app daemon     # web + bot + scheduler

### Страницы

| URL | Назначение |
|-----|-----------|
| `/` | Дашборд |
| `/services` | Управление сервисами |
| `/schedules` | Расписания |
| `/settings` | Настройки |
| `/logs` | Просмотр логов |

### Безопасность

    web:
      enabled: true
      listen: "127.0.0.1:8080"
      allowed_cidrs:
        - 192.168.0.0/16
        - 10.0.0.0/8
      auth:
        enabled: true
        username: admin
        password: "CHANGE_ME"

Web UI использует **Basic Auth** и **CSRF** для изменяющих запросов.

> ⚠️ Не публикуйте Web UI напрямую в интернет. Используйте LAN / VPN, firewall или reverse proxy с дополнительной защитой.

---

## REST API

| Метод | Endpoint | Назначение |
|-------|----------|-----------|
| `GET` | `/healthz` | liveness-проба (без авторизации) |
| `GET` | `/api/v1/status` | Общий статус |
| `GET` | `/api/v1/services` | Список сервисов |
| `POST` | `/api/v1/services/sync` | Запустить синхронизацию набора сервисов |
| `POST` | `/api/v1/services/{name}/sync` | Синхронизировать сервис |
| `GET` | `/api/v1/schedules` | Расписания |
| `PUT` | `/api/v1/schedules/{service}` | Изменить расписание сервиса |
| `GET` | `/api/v1/logs` | Получить логи |
| `WS` | `/api/v1/ws` | Live-обновления |

Изменяющие состояние запросы требуют корректный **CSRF-токен**.

Полная спецификация: [docs/openapi.yaml](docs/openapi.yaml)

---

## Логирование

Логи формируются в **JSON** через `log/slog`.

    logging:
      level: info
      file: /var/log/mikrotik-route-sync/app.log
      max_size_mb: 10
      max_files: 5
      max_total_mb: 50
      compress: true
      also_stdout: true

Ротация выполняется через `lumberjack`. Значения паролей и токенов **не должны** попадать в логи.

---

## Кэш

ASN и сетевые данные кэшируются в `bbolt`, чтобы уменьшить количество внешних запросов и ускорить повторные синхронизации.

    scheduler:
      cache_ttl: 24h
      cache_purge: "every 1h"

Для контейнерного запуска файл кэша (`cache.db`) следует хранить на **persistent volume**.

---

## Docker

### Запуск

    docker compose up -d --build

Или собрать образ вручную:

    docker build -t mikrotik-route-sync:latest .

### Особенности образа

- **Multi-stage** build (builder: `golang:1.27.1-alpine`, runtime: `alpine:3.20`)
- **Non-root** пользователь (`USER 65534:65534`)
- **Read-only** root filesystem
- `cap_drop: [ALL]`, `security_opt: [no-new-privileges:true]`
- **Healthcheck** через `/healthz`
- Ограничение ресурсов: `mem_limit: 128m`, `cpus: 1.0`

### Volumes

| Путь | Назначение |
|------|-----------|
| `/data` | config.yaml, cache.db |
| `/var/log/mikrotik-route-sync` | логи |

---

## Proxmox LXC

Проект рассчитан на лёгкое развёртывание в x86/amd64 LXC:

    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
      go build -trimpath -ldflags="-s -w" -o app ./cmd/app

### Рекомендации

- Запускать бинарник через `systemd` / другой supervisor
- Хранить `config.yaml` с правами `0600`
- Сохранять `cache.db` между перезапусками
- Вынести каталог логов на постоянное хранилище
- Разрешать Web UI и RouterOS API только из доверенных сетей / VPN

---

## Разработка

    git clone https://github.com/Kfaraon/mikrotik-route-sync.git
    cd mikrotik-route-sync

    go mod tidy
    go test ./...
    go vet ./...
    go run ./cmd/app --config config.yaml web

Форматирование:

    gofmt -w ./cmd ./internal

### Makefile

    make build   # сборка статического бинарника
    make test    # запуск тестов
    make fmt     # форматирование
    make vet     # статический анализ
    make clean   # очистка

### Структура проекта

    cmd/
      app/              CLI entrypoint
    internal/
      aggregator/       безопасная агрегация CIDR
      bot/              Telegram
      classifier/       выбор метода сбора
      collectors/       источники сетей
      config/           конфигурация + атомарная запись
      core/             orchestration, syncer, diff
      history/          история синхронизаций
      logging/          slog + rotation + redaction
      mikrotik/         RouterOS REST API
      notifier/         уведомления
      resolver/         DNS / ASN resolution
      scheduler/        расписания
      storage/          bbolt cache
      validator/        фильтрация сетей
      web/              Web UI / REST / WebSocket
    docs/
      ARCHITECTURE.md   описание архитектуры
      SECURITY.md       безопасность
      THREAT_MODEL.md   модель угроз
      openapi.yaml      OpenAPI 3.1 спецификация

---

## Лицензия

[MIT](LICENSE)

---

> Перед использованием в production рекомендуется сначала выполнить `sync --dry-run` для каждого нового сервиса и проверить получившиеся префиксы и diff маршрутов.

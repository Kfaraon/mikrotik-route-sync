# mikrotik-route-sync

[![Go](https://img.shields.io/badge/Go-1.27.1-00ADD8?logo=go&logoColor=white)](https://go.dev/)
[![RouterOS](https://img.shields.io/badge/RouterOS-v7-293239)](https://help.mikrotik.com/docs/)
[![License](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)
[![Docker](https://img.shields.io/badge/Docker-ready-2496ED?logo=docker&logoColor=white)](Dockerfile)

**mikrotik-route-sync** — легковесный сервис на Go для автоматического управления маршрутами MikroTik RouterOS v7 через REST API.

Пользователь указывает сервис, домен, IP-адрес или ASN. Приложение определяет подходящий источник сетей, собирает и валидирует CIDR, безопасно агрегирует их и синхронизирует **только маршруты выбранного сервиса**. Каждый управляемый маршрут помечается комментарием `AUTO:<service>`.

> Главный принцип проекта: один сервис — одна изолированная область синхронизации. Ошибка при обработке `youtube` не должна затрагивать маршруты `instagram`, `cloudflare` или любого другого сервиса.

## Возможности

- автоматическая классификация источника: `asn`, `cdn`, `dynamic`, `whois`, `static_url`;
- определение ASN по домену/IP и работа с ASN, указанным напрямую;
- BGPView с fallback на RIPEstat;
- официальные источники Cloudflare, AWS CloudFront, Google и Fastly;
- DNS/dynamic-сбор для сервисов с меняющейся инфраструктурой;
- пользовательские списки через `static_url`;
- кэш ASN и префиксов в bbolt;
- фильтрация приватных, link-local, multicast и слишком широких сетей;
- безопасная CIDR-агрегация только настоящих sibling-префиксов;
- проверка сохранности множества IP-адресов после агрегации;
- инкрементальная diff-синхронизация RouterOS;
- точная изоляция по комментарию `AUTO:<service>`;
- fail-closed: при пустом/ошибочном результате существующие маршруты не удаляются;
- best-effort rollback при ошибке применения изменений;
- retry и circuit breaker для RouterOS REST API;
- CLI на Cobra;
- трёхуровневые расписания: сервис → группа → глобальное;
- защита от параллельного запуска одного сервиса;
- ограничение общей параллельности через `max_concurrent`;
- Telegram-бот с авторизацией и inline-кнопками;
- встроенный Web UI и REST API;
- WebSocket для live-обновлений;
- Basic Auth, CSRF и ограничение Web UI по CIDR;
- JSON-логирование через `log/slog` и ротация логов;
- статическая сборка `CGO_ENABLED=0`;
- Docker scratch/Alpine и развёртывание в Proxmox LXC.

## Как работает синхронизация

```text
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
```

Если после сбора и валидации не осталось ни одной сети, приложение завершает синхронизацию с ошибкой и **не изменяет текущие маршруты MikroTik**.

## Требования

- Go **1.27.1**;
- MikroTik RouterOS v7 с доступным REST API;
- отдельный RouterOS-пользователь с минимально необходимыми правами;
- доступ приложения к DNS и настроенным внешним источникам;
- для Docker/LXC — постоянное хранилище для конфигурации, кэша и логов.

## Быстрый старт

### 1. Клонирование и конфигурация

```bash
git clone https://github.com/Kfaraon/mikrotik-route-sync.git
cd mikrotik-route-sync

cp config.example.yaml config.yaml
chmod 600 config.yaml
```

Минимально настройте подключение к MikroTik:

```yaml
mikrotik:
  host: 192.168.88.1
  port: 443
  username: api
  password: CHANGE_ME
  use_ssl: true
  verify_ssl: false
  gateway: wg-cz-vpn
  routing_table: main
  distance: 2
  comment_prefix: AUTO
```

Для production рекомендуется использовать доверенный TLS-сертификат и `verify_ssl: true`.

### 2. Сборка и тесты

```bash
go mod tidy
go test ./...
CGO_ENABLED=0 go build -trimpath -o app ./cmd/app
```

### 3. Проверка подключения

```bash
./app --config config.yaml test-mikrotik
```

### 4. Dry-run

Перед первым применением маршрутов:

```bash
./app --config config.yaml sync --dry-run
```

### 5. Запуск Web UI

```bash
./app --config config.yaml web
```

По умолчанию интерфейс доступен на:

```text
http://<server>:8080/
```

## Docker

```bash
docker compose up -d --build
```

Или собрать образ вручную:

```bash
docker build -t mikrotik-route-sync:latest .
```

В проекте также есть `Dockerfile.alpine`. Основной `Dockerfile` предназначен для минимального статического scratch-образа.

## Proxmox LXC

Проект рассчитан на лёгкое развёртывание в x86/amd64 LXC:

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -trimpath -ldflags="-s -w" -o app ./cmd/app
```

Рекомендуется:

- запускать бинарник через `systemd`/другой supervisor;
- хранить `config.yaml` с правами `0600`;
- сохранять `cache.db` между перезапусками;
- вынести каталог логов на постоянное хранилище;
- разрешать Web UI и RouterOS API только из доверенных сетей/VPN.

## CLI

| Команда | Назначение |
|---|---|
| `app sync` | Синхронизировать все сервисы |
| `app sync --service instagram` | Синхронизировать один сервис |
| `app sync --group social` | Синхронизировать группу |
| `app sync --dry-run` | Рассчитать изменения без применения |
| `app add-service youtube` | Добавить и первоначально синхронизировать сервис |
| `app remove-service rutor` | Удалить сервис и только его управляемые маршруты |
| `app info cloudflare` | Показать информацию о сервисе |
| `app list` | Список сервисов |
| `app schedule list` | Показать эффективные расписания |
| `app schedule reload` | Перечитать расписания |
| `app bot` | Запустить Telegram-бота |
| `app web` | Запустить Web UI и планировщик |
| `app config get <key>` | Прочитать параметр конфигурации |
| `app config set <key> <value>` | Изменить параметр |
| `app config edit` | Открыть конфигурацию в `$EDITOR` |
| `app config reload` | Перечитать конфигурацию |
| `app logs tail -n 100` | Последние строки лога |
| `app logs tail -f` | Следить за логом |
| `app logs clear` | Очистить логи |
| `app logs size` | Размер и количество файлов логов |
| `app test-telegram` | Проверить Telegram |
| `app test-mikrotik` | Проверить RouterOS REST API |

Глобальный путь к конфигурации можно передать через:

```bash
./app --config /path/to/config.yaml <command>
```

## Добавление сервисов

Самый простой вариант:

```bash
./app --config config.yaml add-service instagram
./app --config config.yaml add-service youtube
./app --config config.yaml add-service cloudflare
```

Можно передать домен, IP или ASN — resolver/classifier выберет подходящий метод.

Для нестандартных сервисов используйте `overrides`:

```yaml
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
```

`max_asn_prefixes` особенно важен для небольших сайтов на shared-инфраструктуре: он ограничивает риск случайно добавить слишком большое количество сетей ASN.

## Изоляция маршрутов

Для каждого сервиса используется отдельный комментарий:

```text
AUTO:instagram
AUTO:youtube
AUTO:cloudflare
```

При синхронизации `instagram` приложение работает только с маршрутами `AUTO:instagram`.

Маршруты других сервисов и обычные пользовательские маршруты не должны участвовать в diff и не должны удаляться.

## Расписания

Приоритет:

```text
service → group → global
```

Поддерживаются cron-выражения из пяти полей:

```text
0 */6 * * *
```

и человекочитаемые формы:

```text
every 6h
every 30m
daily at 03:00
weekly on sunday at 04:00
manual
disabled
inherit
```

Пример:

```yaml
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
    instagram:
      schedule: "every 4h"
    cloudflare:
      schedule: "every 1h"
    rutor:
      schedule: manual
```

Параллельность настраивается отдельно:

```yaml
scheduler:
  parallel: true
  max_concurrent: 3
  reload_interval: 1m
```

Один и тот же сервис защищён от наложения запусков.

## Telegram-бот

Настройка:

```yaml
telegram:
  enabled: true
  bot_token: CHANGE_ME
  chat_id: "123456789"
  authorized_chat_ids:
    - "123456789"
```

Запуск:

```bash
./app --config config.yaml bot
```

Основные команды:

```text
/start
/menu
/status
/sync
/schedule
/help
```

Бот проверяет `authorized_chat_ids`; сообщения от неразрешённых chat ID игнорируются.

## Web UI

Страницы:

| URL | Назначение |
|---|---|
| `/` | Dashboard |
| `/services` | Управление сервисами |
| `/schedules` | Расписания |
| `/settings` | Настройки |
| `/logs` | Просмотр логов |

Для ограничения доступа:

```yaml
web:
  enabled: true
  listen: ":8080"
  allowed_cidrs:
    - 192.168.0.0/16
    - 10.0.0.0/8
    - 172.16.0.0/12
  auth:
    enabled: true
    username: admin
    password: CHANGE_ME
```

Web UI использует Basic Auth и CSRF для изменяющих состояние запросов. WebSocket ограничен same-origin-проверкой.

Не публикуйте Web UI напрямую в интернет. Используйте LAN/VPN, firewall или reverse proxy с дополнительной защитой.

## REST API

| Метод | Endpoint | Назначение |
|---|---|---|
| `GET` | `/api/v1/status` | Общий статус |
| `GET` | `/api/v1/services` | Сервисы |
| `POST` | `/api/v1/services/sync` | Запустить синхронизацию набора сервисов |
| `POST` | `/api/v1/services/{name}/sync` | Синхронизировать сервис |
| `GET` | `/api/v1/schedules` | Расписания |
| `PUT` | `/api/v1/schedules/{name}` | Изменить расписание сервиса |
| `GET` | `/api/v1/logs` | Получить логи |
| `WS` | `/api/v1/ws` | Live-обновления |

Изменяющие состояние запросы требуют корректный CSRF-токен.

## Логирование

Пример:

```yaml
logging:
  level: info
  file: /var/log/mikrotik-route-sync/app.log
  max_size_mb: 10
  max_files: 5
  max_total_mb: 50
  compress: true
  also_stdout: true
```

Логи формируются в JSON через `log/slog`. Значения паролей и токенов не должны попадать в аудит изменений конфигурации.

## Кэш

ASN и сетевые данные кэшируются в bbolt, чтобы уменьшить количество внешних запросов и ускорить повторные синхронизации.

Пример параметров:

```yaml
scheduler:
  cache_ttl: 24h
  cache_purge: "every 1h"
```

Для контейнерного запуска файл кэша следует хранить на persistent volume.

## Безопасность

Перед production-развёртыванием:

- создайте отдельного RouterOS API-пользователя;
- ограничьте доступ пользователя по IP/firewall;
- используйте HTTPS и по возможности `verify_ssl: true`;
- оставляйте `config.yaml` с правами `0600`;
- не коммитьте `config.yaml`;
- замените все `CHANGE_ME`;
- ограничьте Web UI через `allowed_cidrs`;
- используйте сложный пароль Web UI;
- ограничьте Telegram через `authorized_chat_ids`;
- выполните `sync --dry-run` перед первой синхронизацией;
- сделайте независимый backup конфигурации RouterOS.

Секреты также можно передать через переменные окружения:

```text
MRS_MIKROTIK_PASSWORD
MRS_TELEGRAM_BOT_TOKEN
MRS_WEB_PASSWORD
```

## Поведение при ошибках

Безопасность существующих маршрутов имеет приоритет над обновлением.

Если collector/resolver/API возвращает ошибку или после валидации список CIDR пуст:

```text
синхронизация завершается ошибкой
→ существующие AUTO:<service> остаются без изменений
```

Если ошибка происходит во время применения diff, выполняется best-effort восстановление исходного набора маршрутов сервиса.

Тем не менее автоматизацию маршрутизации нельзя считать заменой резервной копии RouterOS.

## Конфигурация

Полный рабочий шаблон находится в [`config.example.yaml`](config.example.yaml).

Основные разделы:

```text
timezone
logging
mikrotik
telegram
web
scheduler
external
schedules
services
overrides
```

`config.yaml` намеренно добавлен в `.gitignore`.

## Разработка

```bash
git clone https://github.com/Kfaraon/mikrotik-route-sync.git
cd mikrotik-route-sync

go mod tidy
go test ./...
go vet ./...
go run ./cmd/app --config config.yaml web
```

Форматирование:

```bash
gofmt -w ./cmd ./internal
```

Структура проекта:

```text
cmd/                  CLI entrypoint
internal/
  aggregator/         безопасная агрегация CIDR
  bot/                Telegram
  classifier/         выбор метода сбора
  collectors/         источники сетей
  core/               orchestration
  logging/            slog + rotation
  mikrotik/           RouterOS REST API
  notifier/           уведомления
  resolver/           DNS / ASN resolution
  scheduler/          расписания
  storage/            bbolt cache
  validator/          фильтрация сетей
  web/                Web UI / REST / WebSocket
```

## Проверка перед обновлением RouterOS

Рекомендуемый порядок:

```bash
./app test-mikrotik
./app info instagram
./app sync --service instagram --dry-run
./app sync --service instagram
```

После применения проверьте маршруты в RouterOS:

```text
/ip/route/print where comment="AUTO:instagram"
```

## Troubleshooting

**Маршруты не создаются**

Проверьте REST API, gateway, routing table, права RouterOS-пользователя и логи приложения.

**Collector вернул 0 сетей**

Это считается ошибкой безопасности. Существующие маршруты не удаляются. Проверьте DNS, доступ к BGPView/RIPEstat/официальному источнику и `overrides`.

**Web UI возвращает 401/403**

Проверьте Basic Auth, `allowed_cidrs` и CSRF. Если приложение находится за reverse proxy, отдельно убедитесь, что доверенная схема адресации соответствует вашей конфигурации.

**TLS RouterOS не проходит проверку**

Для production установите корректный сертификат. `verify_ssl: false` допустим для контролируемой домашней сети, но снижает защиту от MITM.

## Лицензия

MIT. См. [`LICENSE`](LICENSE).

---

Перед использованием в production рекомендуется сначала выполнить dry-run для каждого нового сервиса и проверить получившиеся префиксы и diff маршрутов.

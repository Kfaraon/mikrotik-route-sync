# mikrotik-route-sync

Автоматическая синхронизация маршрутов MikroTik RouterOS v7 по имени сервиса.

## Возможности
- Автоопределение метода сбора IP (ASN / CDN / dynamic / WHOIS / static URL).
- Агрегация CIDR с проверкой сохранности суммы адресов.
- Инкрементальная синхронизация: маршруты других сервисов не затрагиваются.
- Управление через CLI, Telegram-бота и веб-интерфейс.
- Планировщик с тремя уровнями расписаний (сервис → группа → глобальное).

## Быстрый старт
```bash
docker run -d --name mrs \
  -v $PWD/config.yaml:/data/config.yaml \
  -v $PWD/logs:/var/log/mikrotik-sync \
  -p 8080:8080 \
  mikrotik-route-sync:latest web
```

## CLI
| Команда | Назначение |
|---|---|
| `app sync` | Синхронизировать все сервисы |
| `app sync --service instagram` | Один сервис |
| `app sync --group social` | Группа |
| `app add-service youtube` | Добавить сервис |
| `app remove-service rutor` | Удалить сервис |
| `app logs tail -n 100 --follow` | Логи |
| `app test-mikrotik` | Проверка подключения |
| `app test-telegram` | Проверка уведомлений |

## Скриншоты
- `docs/dashboard.png` — дашборд
- `docs/services.png` — сервисы
- `docs/bot-menu.png` — меню бота

## Конфигурация
См. `config.example.yaml`.

## Разработка
```bash
git clone https://github.com/Kfaraon/mikrotik-route-sync
cd mikrotik-route-sync
go test ./...
go run ./cmd/app web
```

## Лицензия
MIT# mikrotik-route-sync

Автоматическое управление маршрутами на MikroTik RouterOS v7 через REST API.

## Возможности

- Автоматическое определение метода сбора IP (ASN, CDN, dynamic, WHOIS, static_url).
- Агрегация CIDR (объединение смежных и вложенных подсетей).
- Инкрементальная синхронизация по сервисам (маркер `AUTO:<service>`).
- Три уровня расписаний: сервис → группа → глобальное.
- CLI, Telegram-бот с inline-кнопками, встроенный Web UI (HTMX).
- Retry + Circuit Breaker, кэш ASN/prefixes (bbolt), ротация логов.
- Работа в LXC / Docker, единый статический бинарник.

## Быстрый старт

```bash
# 1. Собрать
CGO_ENABLED=0 go build -o app ./cmd/app

# 2. Проверить план
./app --config config.yaml sync --dry-run

# 3. Запустить web + scheduler
./app --config config.yaml web

# 4. Запустить telegram-бот
./app --config config.yaml bot

## CLI

Команда	Описание
app sync	Синхронизация всех сервисов
app sync --service <name>	Один сервис
app sync --group <name>	Все сервисы группы
app sync --dry-run	Показать план без применения
app add-service <name>	Добавить сервис
app remove-service <name>	Удалить сервис и его маршруты
app info <name>	Информация о сервисе
app list	Список сервисов
app schedule list	Показать расписания
app web	Веб-интерфейс + планировщик
app bot	Telegram-бот + планировщик
app config get/set <key> [value]	Работа с конфигом
app test-telegram	Проверить связь с ботом

## Web UI

    http://<host>:8080/ — дашборд

    http://<host>:8080/settings — настройки (Basic Auth)

    http://<host>:8080/logs — логи

REST API: /api/v1/status, /api/v1/services/sync, /api/v1/schedules.

## Безопасность

    config.yaml содержит пароли и токены — права 0600, не коммитить в git.

    Web UI защищён Basic Auth + CSRF.

    Откройте порт только для доверенных сетей / VPN.

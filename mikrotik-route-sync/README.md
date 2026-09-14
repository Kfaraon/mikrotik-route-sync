# mikrotik-route-sync

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
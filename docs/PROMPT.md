# Промпт для проекта mikrotik-route-sync

Ты — senior Go-разработчик, сетевой инженер и security-инженер. Создай полноценный, готовый к production проект на Go для автоматического управления маршрутами на MikroTik RouterOS v7. Проект должен быть безопасным по умолчанию (secure by default), отказоустойчивым, наблюдаемым (observable), с минимумом зависимостей и пригодным для работы в LXC-контейнере на Proxmox (x86_64) с ограниченными ресурсами (≤128 МБ RAM, ≤1 CPU).

## Главная цель

Пользователь указывает только название сервиса (`instagram`, `youtube`, `rutor`, `cloudflare`) — либо его домен / IP / ASN. Всё остальное система делает сама:

1. Определяет, к какому типу относится сервис (CDN, крупный ASN, динамический, WHOIS, публичный список).
2. Автоматически выбирает один или несколько методов сбора IP-диапазонов.
3. Собирает CIDR, строго валидирует, нормализует и агрегирует в минимальный набор подсетей.
4. Инкрементально синхронизирует маршруты на MikroTik через REST API RouterOS v7.
5. Отправляет уведомления в Telegram (старт, прогресс, результат, ошибки).
6. Работает по многоуровневому расписанию (глобальное → группа → сервис).
7. Имеет CLI для ручного управления, диагностики и восстановления.
8. Управляется через Telegram-бота с inline-кнопками.
9. Управляется через встроенный веб-интерфейс (dashboard, services, schedules, settings, logs).
10. Работает как статический бинарник в LXC без внешних зависимостей (кроме сетевых API).

---

## I. Ключевые архитектурные принципы

### 1.1 Изоляция по сервисам (Service Isolation)
- Все маршруты, созданные приложением, помечаются комментарием `AUTO:<ServiceName>` (например, `AUTO:instagram`).
- Каждый сервис — изолированная область управления. Ошибка при обработке `youtube` **не должна затрагивать** маршруты `instagram`, `cloudflare` или любого другого сервиса.
- Все REST-запросы к MikroTik фильтруются по `comment=AUTO:<service>`.
- Никакие операции (GET / DELETE / POST) не выходят за пределы одного сервиса.
- Удаление сервиса (`app remove-service`) удаляет только его маршруты, никогда не затрагивая маршруты других сервисов и не-AUTO-маршруты.

### 1.2 Fail-closed
- Если resolver / collector / validator / MikroTik API вернул ошибку или после валидации список CIDR пуст — синхронизация сервиса завершается ошибкой, **существующие маршруты `AUTO:<service>` не удаляются и не изменяются**.
- Пропуск синхронизации логируется с уровнем `WARN` и отправляется в Telegram.
- Никогда не выполняй «удалить всё, что было, и добавить то, что получилось» при пустом/подозрительно коротком результате.

### 1.3 Best-effort rollback
- Перед применением diff выполняется снимок текущих маршрутов сервиса.
- При ошибке во время применения diff выполняется восстановление исходного набора (rollback) через REST API RouterOS.
- Если rollback частично неуспешен — сервис помечается как `degraded`, отправляется алерт с высоким приоритетом.

### 1.4 Safe-diff (Защита от массового удаления)
- Перед удалением маршрутов проверяется соотношение `deleted / existing`.
- Если будет удалено более `safety.max_delete_ratio` (по умолчанию 0.5 = 50%) от текущего количества маршрутов сервиса, синхронизация **прерывается** и требует ручного подтверждения через CLI (`--force`).
- Если `existing > 0`, а новый набор пуст — прерывать всегда, без исключений.

### 1.5 Idempotency (Идемпотентность)
- Повторный запуск синхронизации с теми же входными данными не должен приводить к изменениям в RouterOS.
- Сравнение CIDR — по нормализованному виду (network + prefix length), без учёта хостовых бит.

### 1.6 Кэширование
- Результаты ASN-resolution, WHOIS, RDAP, BGPView, RIPEstat кэшируются в bbolt.
- TTL кэша задаётся в конфиге (`scheduler.cache_ttl`, по умолчанию 24 часа).
- Периодическая очистка (`scheduler.cache_purge`, по умолчанию `every 1h`).
- Кэш используется только как оптимизация, но не как источник истины для удаления маршрутов.

### 1.7 Минимизация прав
- Для RouterOS используется **отдельный пользователь API** с минимальными правами (`read`, `write`, `rest-api`, `api`, `policy`).
- Пользователь ограничен по IP-адресу/подсети через firewall-правило.
- Веб-интерфейс слушает только LAN/loopback.
- Docker/LXC — без `--privileged`, с `cap_drop: [ALL]`, `security_opt: [no-new-privileges:true]`, read-only root FS.

---

## II. Автоматическое определение метода сбора IP

### 2.1 Шаг 1. Определить ASN сервиса
- Если указан IP — использовать напрямую (проверить через RDAP, к какому ASN относится).
- Если указан домен — резолвить через DNS (A + AAAA, с указанием резолверов из конфига).
- Через WHOIS/RDAP или BGPView определить ASN. Основной источник — BGPView, fallback — RIPEstat, второй fallback — RDAP-запросы к RIR (ARIN, RIPE, APNIC, LACNIC, AFRINIC).
- Если указан ASN напрямую — использовать его.
- Результат кэшируется в bbolt с TTL.
- При неопределённости (несколько ASN для домена) — использовать эвристику: приоритет ASN с наибольшим количеством анонсируемых префиксов.

### 2.2 Шаг 2. Классификация сервиса

**Известные CDN (словарь ASN):**
- Cloudflare (AS13335) → `https://www.cloudflare.com/ips-v4` (и `-v6` при `ipv6: true`).
- AWS CloudFront (AS16509, service=CLOUDFRONT) → `https://ip-ranges.amazonaws.com/ip-ranges.json` (фильтр по `service`).
- Google (AS15169) → `https://www.gstatic.com/ipranges/goog.json`.
- Fastly → официальный API `https://api.fastly.com/public-ip-list`.

**Крупные сервисы с собственным ASN:** метод `asn` через BGPView/RIPEstat.

**Динамические сервисы (Google, YouTube, Telegram):** метод `dynamic` — DNS-резолвинг + официальный JSON, агрегация всех полученных сетей.

**Мелкие сайты без ASN (Rutor):** метод `whois` — DNS → ASN → все префиксы. Ограничение `max_asn_prefixes` в `overrides` (по умолчанию 100) для защиты от захвата лишнего.

**Публичные списки:** метод `static_url` — antifilter, re:filter, произвольные URL.

**Совмещённые методы:** можно указать список методов (например, `[cdn, dynamic]`), результат объединяется и агрегируется.

### 2.3 Шаг 3. Валидация (Multi-layer Validation)

**Уровень 1. Синтаксис:** проверка формата CIDR, нормализация в network-адрес, допустимый диапазон (IPv4: 8..32, IPv6: 16..128).

**Уровень 2. Приватные/служебные диапазоны (RFC + IANA):** отфильтровать RFC 1918, loopback, link-local, CGNAT, TEST-NET, multicast, reserved и т.д.

**Уровень 3. Проверка принадлежности ASN (WHOIS/RDAP):** для каждого префикса из метода `asn`/`whois` — проверка через RDAP, что префикс действительно принадлежит указанному ASN.

**Уровень 4. Ширина префикса:** отклонять слишком широкие сети (IPv4 `/<min_prefix_v4>`, IPv6 `/<min_prefix_v6>`).

**Уровень 5. Пользовательские фильтры:** `overrides.<service>.exclude`, `include_only`, `max_prefixes`.

**Уровень 6. Дедупликация:** удаление точных дубликатов и вложенных префиксов.

### 2.4 Шаг 4. Агрегация
- Использовать префиксное дерево (Radix Tree) в `internal/aggregator/radix.go` для O(n·L).
- Объединять только настоящие sibling-префиксы (никогда не объединять несмежные сети и не «раздувать» покрытие).
- Проверять инвариант: **сумма адресов до агрегации = сумма адресов после агрегации**.
- Проверять инвариант: **каждый исходный префикс полностью покрыт результирующим набором**.
- При нарушении инварианта — откатить агрегацию, логировать `ERROR`, использовать неагрегированный (но валидированный) список.
- Валидация агрегации вынесена в `aggregator/validate.go`.

---

## III. Инкрементальная синхронизация по сервисам

### 3.1 Добавление нового сервиса (`app add-service <service>`)
1. Определить метод(ы) сбора.
2. Собрать CIDR только для нового сервиса.
3. Валидировать, нормализовать, агрегировать.
4. Проверить `safety.max_delete_ratio` и `existing > 0 → new non-empty`.
5. Получить с MikroTik маршруты с `AUTO:<new_service>`.
6. Вычислить diff (add / remove / unchanged) с нормализацией.
7. Применить diff транзакционно.
8. Определить эффективное расписание (приоритет: сервис → группа → глобальное).
9. Записать сервис в `config.yaml` (атомарно через `config.AtomicWrite`: временный файл + `fsync` + `os.Rename`).
10. Уведомить в Telegram.

### 3.2 Синхронизация (`app sync`)
- Для каждого сервиса независимо: собрать → валидировать → агрегировать → получить `AUTO:<service>` → diff → применить → записать историю.
- Параллельность ограничена `scheduler.max_concurrent`.
- Используется mutex на сервис — повторный запуск по тому же сервису пропускается с `WARN`.
- Ошибка одного сервиса не останавливает остальные.
- Итоговый отчёт агрегируется и отправляется в Telegram.

### 3.3 Dry-run (`app sync --dry-run`)
- Полностью проходит все шаги, но не выполняет никаких изменений в MikroTik.
- Выводит diff в JSON (через `printJSON`).

### 3.4 Транзакционность на уровне сервиса
- Порядок операций: сначала `POST` (добавление новых), затем `DELETE` (удаление устаревших).
- Это гарантирует, что в случае сбоя на этапе DELETE маршруты не будут потеряны полностью.
- При сбое любого шага — rollback.
- Максимум ретраев на операцию — `retry.max_attempts` (по умолчанию 3) с экспоненциальной задержкой и jitter.

### 3.5 Snapshots (backup / restore)
- Перед каждой синхронизацией — снимок текущих маршрутов сервиса в bbolt (bucket `snapshots`, TTL по умолчанию 168h).
- Экспорт маршрутов сервиса в JSON по команде `app backup <service>`.
- Восстановление из снимка: `app restore <service> --from-snapshot <id> --force` или `--from-file <file.json> --force`.
- Управление снапшотами: `app snapshots list`, `app snapshots delete`, `app snapshots cleanup --ttl <hours>`.

### 3.6 Изоляция
- Все запросы к MikroTik фильтруются: `GET /rest/ip/route?comment=AUTO:instagram`.
- DELETE и POST адресуются по `id` маршрута, полученному из отфильтрованного списка.
- `app remove-service <service>` удаляет только маршруты этого сервиса.

---

## IV. Система расписаний (три уровня)

**Приоритет:** сервис → группа → глобальное.

### 4.1 Форматы расписаний
- **Cron** (5 полей): `"0 */6 * * *"` (через `github.com/robfig/cron/v3`).
- **Простой интервал:** `"every 6h"`, `"every 30m"`, `"daily at 03:00"`, `"weekly on sunday at 04:00"`.
- **Специальные:** `"manual"` (только вручную), `"disabled"` (не запускать), `"inherit"` (взять из группы/глобального).
- Разделение: `internal/scheduler/schedule.go` (парсинг и описание расписания) и `internal/scheduler/scheduler.go` (сам планировщик).

### 4.2 Поведение планировщика
- Mutex на сервис — защита от наложения.
- Пропуск запуска логируется с `WARN`, отправляется уведомление.
- Ограничение общей параллельности через `scheduler.max_concurrent`.
- Горячая перезагрузка расписаний: SIGHUP + периодическое перечитывание раз в `reload_interval`.
- Часовой пояс из `timezone` (IANA-название, например `Europe/Moscow`).
- DST-переходы учитываются планировщиком (`WithLocation`).

### 4.3 Пример конфигурации

    schedules:
      global: "every 6h"
      groups:
        social:
          schedule: "daily at 03:00"
          services: [instagram, telegram]
        video:
          schedule: "every 12h"
          services: [youtube]
      services:
        cloudflare: {schedule: "every 1h"}

---

## V. Управление через Telegram-бота

### 5.1 Команды
`/start`, `/menu`, `/status`, `/sync`, `/schedule`, `/help`.

### 5.2 Реализация
- `github.com/go-telegram-bot-api/telegram-bot-api/v5`.
- Один файл `internal/bot/bot.go` — компактная реализация без сложной state machine.
- Авторизация: **только `chat_id` из `authorized_chat_ids`**. Все остальные — игнорировать и логировать с `WARN` (не отвечать, чтобы не раскрывать существование бота).
- Ограничение частоты (rate limit) — не более `telegram.rate_limit` сообщений/сек на chat_id.
- `telegram.enabled: false` по умолчанию (пользователь явно включает).

### 5.3 Уведомления
- Старт синхронизации, прогресс (опционально), завершение (сводная таблица), ошибки (без утечки секретов).

---

## VI. Веб-интерфейс

### 6.1 Общая концепция
- Встроенный в бинарник дашборд через `//go:embed` (папки `internal/web/static` и `internal/web/templates`).
- Лёгкий, адаптивный, без внешних CDN.

### 6.2 Страницы

| URL | Назначение |
|-----|-----------|
| `/` | Dashboard — статус, сводка, список сервисов, quick actions |
| `/services` | Управление сервисами — список, обновление, удаление, карточка сервиса |
| `/schedules` | Расписания — inline-редактирование, отображение эффективного расписания |
| `/settings` | Настройки MikroTik, Telegram, Web, Scheduler, Safety, Retry, External, Logging |
| `/logs` | Просмотр последних строк, фильтрация по уровню/сервису, очистка, скачивание |

### 6.3 Технические детали
- **Фреймворк:** `net/http` + `github.com/go-chi/chi/v5`.
- **Шаблоны:** `html/template` + `//go:embed` с автоэкранированием.
- **CSS:** Pico CSS (встроенный в `static/app.css`).
- **JS:** HTMX + vanilla JS (без зависимостей).
- **WebSocket:** `github.com/gorilla/websocket` (`internal/web/server.go`).
- **Аутентификация:** Basic Auth + session cookie + CSRF-токен.
- **Ограничение доступа:** `allowed_cidrs` (LAN, VPN).
- **Заголовки безопасности:** `Content-Security-Policy`, `X-Frame-Options: DENY`, `X-Content-Type-Options: nosniff`, `Strict-Transport-Security` (при HTTPS).
- **Секреты:** отдельный модуль `internal/web/secret.go` для маскирования паролей/токенов в UI (показ только последних 4 символов, кнопка 👁 на 10 секунд).

### 6.4 Middleware
- Реализованы **внутри проекта** (без внешних `go-chi/httplog`, `go-chi/httprate`).
- Rate limiting через `golang.org/x/time/rate`.

---

## VII. REST API

| Метод | Endpoint | Назначение |
|-------|----------|-----------|
| `GET` | `/healthz` | liveness-проба (без авторизации, используется в Docker HEALTHCHECK) |
| `GET` | `/api/v1/status` | общий статус |
| `GET` | `/api/v1/services` | список сервисов |
| `POST` | `/api/v1/services/sync` | запустить синхронизацию набора сервисов |
| `POST` | `/api/v1/services/{name}/sync` | синхронизировать сервис |
| `GET` | `/api/v1/schedules` | расписания |
| `PUT` | `/api/v1/schedules/{service}` | изменить расписание сервиса |
| `GET` | `/api/v1/logs` | получить логи (с параметрами level, service, since, limit) |
| `WS` | `/api/v1/ws` | live-обновления |

**Правила API:**
- Изменяющие состояние запросы требуют CSRF-токен и аутентификацию.
- Все ответы в JSON с единой схемой `{ "ok": bool, "data": ..., "error": ... }`.
- Ограничение размера тела запроса (≤1 МБ) через `http.MaxBytesReader`.
- Таймауты: ReadTimeout, WriteTimeout, IdleTimeout, ReadHeaderTimeout.

---

## VIII. Логирование и наблюдаемость

### 8.1 Логирование
- Формат: JSON через `log/slog` (`internal/logging/logging.go`).
- Вывод: stdout + файл (одновременно).
- Ротация: `gopkg.in/natefinsh/lumberjack.v2`.
- Параметры: `max_size_mb` (10 МБ), `max_files` (5), `max_total_mb` (50 МБ), `compress: true`, `level: info`, `also_stdout: true`.
- Структурированные поля: `service`, `method`, `duration_ms`, `added`, `removed`, `error`.
- **Запрещено логировать:** пароли, токены, содержимое `Authorization`, cookie. Автоматический redaction в логах, CLI, Web UI, Telegram-сообщениях.

### 8.2 Health checks
- `/healthz` — процесс жив (используется Docker HEALTHCHECK: `wget --spider http://127.0.0.1:8080/healthz`).

---

## IX. Хранение секретов

- Пароли и ключи хранятся в `config.yaml` в открытом виде (домашняя сеть).
- **Права файла — `0600`** (проверка при старте в `loadConfig()` через `os.Stat`, отказ при нарушении).
- Файл добавлен в `.gitignore`.
- Редактирование через веб-интерфейс, CLI (`config edit` открывает `$EDITOR`) или вручную.
- **Приоритет источников:** ENV > config.yaml > defaults.
- Поддерживаемые ENV: `MRS_MIKROTIK_PASSWORD`, `MRS_TELEGRAM_BOT_TOKEN`, `MRS_WEB_PASSWORD`.
- **Атомарная запись:** `config.AtomicWrite()` — tmpfile + fsync + rename.
- **Маскирование в CLI и Web:** `secret.go` возвращает `••••••••` (только последние 4 символа).

---

## X. Архитектура и модули

    cmd/
      app/
        main.go           — CLI entrypoint (cobra), DI через withSyncer()
    internal/
      aggregator/         — безопасная агрегация CIDR
        aggregator.go     — основной интерфейс
        radix.go          — Radix Tree
        validate.go       — проверка инвариантов агрегации
      bot/                — Telegram-бот
        bot.go            — компактная реализация
      classifier/         — выбор метода сбора
        classifier.go
      collectors/         — источники сетей
        collectors.go     — registry и интерфейсы
        asn.go            — ASN-сбор
        cdn.go            — CDN (Cloudflare, AWS, Google, Fastly)
        dynamic.go        — DNS + официальный JSON
        http.go           — HTTP-клиент с retry + circuit breaker
        registry.go       — реестр collector'ов
        ripestat.go       — RIPEstat fallback
        static.go         — static_url
        whois.go          — WHOIS/RDAP
      config/             — конфигурация
        config.go         — загрузка (yaml + ENV overrides)
        save.go           — атомарная запись (tmpfile + fsync + rename)
      core/               — оркестратор, синхронизация
        syncer.go         — Syncer: AddService, RemoveService, SyncService, SyncMany, Backup, Restore, Snapshots
        diff.go           — вычисление diff (add/remove/unchanged)
      history/            — история синхронизаций
        history.go
      logging/            — slog + rotation + redaction
        logging.go
      mikrotik/           — RouterOS REST API
        client.go         — HTTP-клиент с retry/circuit breaker
        transaction.go    — транзакции и rollback
      notifier/           — уведомления (Telegram)
        notifier.go
      resolver/           — DNS / ASN resolution
        resolver.go       — BGPView + RIPEstat + RDAP
        rdap.go           — RDAP-клиент
      scheduler/          — расписания
        schedule.go       — парсинг cron/human-readable
        scheduler.go      — сам планировщик
      storage/            — bbolt cache
        storage.go
      validator/          — фильтрация сетей (RFC, ASN-проверка)
        validator.go
      web/                — Web UI / REST / WebSocket
        server.go         — chi router, endpoints, WS
        secret.go         — маскирование секретов
        templates/        — HTML-шаблоны
        static/           — CSS/JS/изображения

### 10.1 Разделение ответственности
- **Collector** — только сбор сырых данных из источника (не валидирует).
- **Validator** — только валидация и нормализация.
- **Aggregator** — только агрегация с проверкой инвариантов.
- **Syncer** — оркестрация: collector → validator → aggregator → diff → MikroTik.
- **MikroTik Client** — только REST-операции, без бизнес-логики.

### 10.2 DI (Dependency Injection)
- Явная передача зависимостей через конструкторы (`core.NewSyncer`, `bot.New`, `webui.New`, `scheduler.New`).
- Хелпер `withSyncer()` в `main.go` загружает конфиг, открывает bbolt-кэш, создаёт Syncer и передаёт их в команду.
- Никаких глобальных переменных и `init()`-магии.
- Интерфейсы для мокирования при интеграционной отладке.

---

## XI. Безопасность (расширенно)

### 11.1 Безопасность конфигурации
- Проверка прав файла при старте (`0600`).
- Атомарная запись через `config.AtomicWrite` (tmpfile + fsync + os.Rename).
- Валидация всей конфигурации через `c.Validate()` перед применением.
- Отказ старта при критичных ошибках (невалидный cron, отсутствие `mikrotik.host`).

### 11.2 Безопасность сети
- Веб-интерфейс слушает только явно указанный адрес (по умолчанию `127.0.0.1:8080`).
- Проверка `allowed_cidrs` для всех входящих (кроме `/healthz`).
- TLS ≥ 1.2, предпочтительно 1.3. `verify_ssl: true` по умолчанию (для production).
- HTTP Strict Transport Security при HTTPS.
- Rate limiting (собственная реализация).
- Security headers (CSP, X-Frame-Options, X-Content-Type-Options).
- Ограничение размера запросов через `http.MaxBytesReader`.

### 11.3 Безопасность MikroTik
- Отдельный пользователь API с минимальными правами.
- `verify_ssl: true` по умолчанию; при `false` — предупреждение в логах.
- Ограничение частоты запросов к RouterOS (`rate_limit` в клиенте).
- Circuit Breaker (`github.com/sony/gobreaker`).
- Retry с экспоненциальной задержкой + jitter (`github.com/hashicorp/go-retryablehttp`).
- Никогда не логировать пароль и заголовок `Authorization`.

### 11.4 Безопасность внешних API
- Таймауты на все HTTP-запросы (`external.http_timeout`, по умолчанию 15 сек).
- Ограничение размера ответа (`io.LimitReader`, `external.max_response_mb`, по умолчанию 50 МБ).
- Проверка `Content-Type` перед парсингом JSON.
- Ограничение редиректов (≤3).
- Проверка TLS-сертификата (без `InsecureSkipVerify` по умолчанию).
- Sanitization URL из конфига (только `http/https`).
- Защита от SSRF: блокировка запросов к private/loopback/link-local адресам.
- Circuit Breaker для каждого источника.

### 11.5 Безопасность Telegram
- Авторизация только по `authorized_chat_ids`.
- Не отвечать неавторизованным (чтобы не раскрывать существование).
- Rate limit на команды.
- Логировать попытки неавторизованного доступа с `WARN`.
- Не передавать в Telegram содержимое секретов.

### 11.6 Безопасность контейнера (Docker)
- Multi-stage build: `golang:1.27.1-alpine` (builder) → `alpine:3.20` (runtime, с `ca-certificates` и `tzdata`).
- Static binary (`CGO_ENABLED=0`), `-trimpath`, `-ldflags="-s -w"`.
- Non-root пользователь (`USER 65534:65534`, `appuser:appgroup`).
- Read-only root filesystem (`read_only: true`).
- `cap_drop: [ALL]`, `security_opt: [no-new-privileges:true]`.
- Ограничение ресурсов (`mem_limit: 128m`, `cpus: 1.0`).
- Healthcheck через `/healthz` (wget --spider).
- Volume для `/data` (config.yaml + cache.db) и `/var/log/mikrotik-route-sync`.
- Никаких секретов в образе; передача через ENV или mounted volumes.

### 11.7 Защита от утечек данных
- Redaction в логах: значения полей `password`, `token`, `secret`, `api_key`, `authorization`, `cookie`.
- Redaction в ошибках, отправляемых в Telegram / API.
- Redaction в CLI-выводе через `secret.go`.

### 11.8 Защита от supply chain
- `go.sum` зафиксирован.
- `govulncheck` и `gosec` — как обязательные шаги при релизе.
- Зависимости — только популярные, с активной поддержкой.

### 11.9 Защита от небезопасных входных данных
- Валидация имени сервиса: `^[a-z0-9][a-z0-9_-]{0,63}$`.
- Валидация ASN: `^AS\d{1,10}$`.
- Валидация CIDR: только корректный формат.
- Валидация URL: только `http`/`https`.
- Валидация cron: только через `robfig/cron/v3`.
- Экранирование при вставке в RouterOS-комментарии (без `"`, `\`, `\n`).

---

## XII. Отказоустойчивость

### 12.1 Retry
- Экспоненциальная задержка: `base_delay * 2^attempt + jitter` через `hashicorp/go-retryablehttp`.
- По умолчанию: `base_delay=1s`, `max_delay=30s`, `max_attempts=3`.
- Retry только для идемпотентных операций и сетевых ошибок (5xx, timeout, connection refused).
- Не retry для 4xx (кроме 429).

### 12.2 Circuit Breaker
- `github.com/sony/gobreaker`.
- Состояния: Closed → Open → Half-Open → Closed.
- Порог открытия: 5 ошибок подряд.
- Таймаут Open: 60 сек.
- Отдельный breaker для каждого внешнего сервиса (MikroTik, BGPView, RDAP, Telegram).

### 12.3 Восстановление после краха
- Все критические операции — с записью состояния в bbolt.
- Graceful shutdown по SIGTERM/SIGINT: завершение текущих операций, закрытие соединений, flush логов (через `signal.NotifyContext`).

### 12.4 Бэкапы
- Перед каждой синхронизацией — снимок текущих маршрутов сервиса в bbolt (bucket `snapshots`, TTL 7 дней).
- Экспорт/восстановление через CLI (`backup`, `restore`, `snapshots`).

---

## XIII. CLI (cobra)

| Команда | Назначение |
|---------|------------|
| `app sync` | Синхронизировать все сервисы |
| `app sync --service <name>` | Синхронизировать один сервис |
| `app sync --group <name>` | Синхронизировать группу |
| `app sync --dry-run` | Рассчитать изменения без применения |
| `app sync --service <name> --force` | Обойти safety-check |
| `app diff <service>` | Показать diff в JSON |
| `app list` | Список сервисов и их эффективных расписаний |
| `app info <service>` | Показать информацию о сервисе (schedule, overrides) |
| `app add-service <service>` | Добавить и первоначально синхронизировать сервис |
| `app remove-service <service> [--force]` | Удалить сервис и его управляемые маршруты |
| `app backup <service> [-o file.json] [--from-snapshot id]` | Экспорт маршрутов сервиса в JSON |
| `app restore <service> --from-file / --from-snapshot --force` | Восстановление из файла или снапшота |
| `app snapshots list <service>` | Список снапшотов |
| `app snapshots delete <service> <snapshot>` | Удалить снапшот |
| `app snapshots cleanup <service> --ttl <hours>` | Удалить старые снапшоты |
| `app schedule list` | Показать эффективные расписания |
| `app schedule reload` | Перечитать расписания |
| `app bot` | Запустить Telegram-бота |
| `app web` | Запустить Web UI |
| `app daemon` | Запустить всё (bot + web + scheduler), SIGHUP для reload |
| `app config validate` | Проверить конфигурацию без применения |
| `app config reload` | Отправить SIGHUP самому себе |
| `app config edit` | Открыть конфигурацию в `$EDITOR` |
| `app config get <key>` | Прочитать параметр (секреты маскируются через secret.go) |
| `app logs tail [-n N] [-f]` | Последние строки лога / follow |
| `app logs clear` | Очистить логи |
| `app logs size` | Размер и количество файлов логов |
| `app test-telegram` | Проверить Telegram |
| `app test-mikrotik` | Проверить RouterOS REST API |
| `app test-dns <domain>` | Проверить резолвинг |
| `app version` | Версия (из переменной `version`, задаваемой через `-ldflags`) |

**Глобальные флаги:**
- `--config /path/to/config.yaml` (persistent flag)

**Архитектура CLI:**
- Один файл `cmd/app/main.go` с `newRoot() *cobra.Command`.
- Общий хелпер `withSyncer(fn)` — загружает конфиг, открывает bbolt, создаёт Syncer, настраивает graceful shutdown.
- `loadConfig()` проверяет права `0600` через `os.Stat`.
- `printJSON()` для машиночитаемого вывода.
- `tailLog()` — собственная реализация tail без внешних зависимостей.

---

## XIV. Конфигурация (пример config.yaml)

    # ВНИМАНИЕ: файл содержит секреты. Права 0600, добавлен в .gitignore.

    timezone: Europe/Moscow

    logging:
      level: info
      file: /var/log/mikrotik-sync/app.log
      max_size_mb: 10
      max_files: 5
      max_total_mb: 50
      compress: true
      also_stdout: true

    mikrotik:
      host: 192.168.88.1
      port: 443
      username: api
      password: "SET_A_SECRET"
      use_ssl: true
      verify_ssl: true      # по умолчанию true для production
      timeout: 30s
      gateway: wg-cz-vpn
      routing_table: main
      distance: 2
      comment_prefix: AUTO
      rate_limit: 20        # запросов в секунду

    telegram:
      enabled: false         # по умолчанию false, пользователь явно включает
      bot_token: ""
      chat_id: ""
      authorized_chat_ids: []
      rate_limit: 1

    web:
      enabled: true
      listen: 127.0.0.1:8080
      allowed_cidrs: [127.0.0.0/8, 192.168.0.0/16, 10.0.0.0/8, 172.16.0.0/12]
      trusted_proxies: []
      auth:
        enabled: true
        username: admin
        password: "SET_A_SECRET"
      session_timeout: 24h
      csrf_enabled: true
      security_headers: true

    scheduler:
      parallel: true
      max_concurrent: 3
      reload_interval: 1m
      cache_ttl: 24h
      cache_purge: every 1h

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
      rdap_timeout: 10s
      resolver: 1.1.1.1:53

    snapshots:
      enabled: true
      ttl: 168h             # 7 дней
      max_count: 50

    schedules:
      global: every 6h
      groups:
        social:
          schedule: daily at 03:00
          services: [instagram, telegram]
      services:
        cloudflare: {schedule: every 1h}

    services: [cloudflare]
    overrides: {}

**Переопределения для конкретных сервисов (`overrides`):**

    overrides:
      rutor:
        domains: [rutor.info, rutor.org]
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

---

## XV. Технические требования

- **Go 1.27.1** (последняя стабильная версия).
- **Реальные зависимости из `go.mod`:**
  - `github.com/go-chi/chi/v5 v5.2.3` — HTTP router.
  - `github.com/go-telegram-bot-api/telegram-bot-api/v5 v5.5.1` — Telegram.
  - `github.com/gorilla/websocket v1.5.3` — WebSocket для Web UI.
  - `github.com/hashicorp/go-retryablehttp v0.7.8` — retry + exponential backoff.
  - `github.com/robfig/cron/v3 v3.0.1` — планировщик.
  - `github.com/sony/gobreaker v1.0.0` — Circuit Breaker.
  - `github.com/spf13/cobra v1.10.1` — CLI.
  - `go.etcd.io/bbolt v1.4.3` — кэш и снапшоты.
  - `golang.org/x/time v0.12.0` — rate limiting.
  - `gopkg.in/natefinch/lumberjack.v2 v2.2.1` — ротация логов.
  - `gopkg.in/yaml.v3 v3.0.1` — парсинг YAML.
- **Стандартная библиотека:** `net/http`, `net`, `encoding/json`, `crypto/tls`, `log/slog`, `html/template`, `embed`, `crypto/rand`, `crypto/subtle`, `os/signal`, `syscall`.
- **НЕ используются:** `spf13/viper`, `go-chi/httplog`, `go-chi/httprate`, OpenTelemetry — всё это реализовано собственными средствами или не требуется.
- **Сборка:** статический бинарник (`CGO_ENABLED=0`), `-trimpath`, `-ldflags="-s -w"`.
- **Docker:** multi-stage (`golang:1.27.1-alpine` → `alpine:3.20`), non-root, read-only, `cap_drop: [ALL]`.
- **Docker Compose:** `docker-compose.yml` с ограничениями ресурсов (`mem_limit: 128m`, `cpus: 1.0`).
- **Makefile:** `make build`, `make test`, `make fmt`, `make vet`, `make clean`.
- **Документация:**
  - `README.md` — полное руководство с примерами.
  - `LICENSE` — MIT.
  - `docs/ARCHITECTURE.md` — описание архитектуры.
  - `docs/SECURITY.md` — безопасность и архитектурные гарантии.
  - `docs/THREAT_MODEL.md` — модель угроз и контрмеры.
  - `docs/openapi.yaml` — OpenAPI 3.1 спецификация.
  - `config.example.yaml` — шаблон конфигурации.

---

## XVI. Docker и деплой

### 16.1 Dockerfile (multi-stage)

    # Stage 1: Builder
    FROM golang:1.27.1-alpine AS builder
    RUN apk add --no-cache git ca-certificates tzdata
    WORKDIR /src
    COPY go.mod go.sum ./
    RUN go mod download
    COPY . .
    RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
        -trimpath \
        -ldflags="-s -w" \
        -o /bin/mikrotik-route-sync \
        ./cmd/app

    # Stage 2: Runtime (alpine, не scratch — нужны ca-certificates и tzdata)
    FROM alpine:3.20 AS runtime
    RUN apk add --no-cache ca-certificates tzdata && \
        addgroup -g 65534 appgroup && \
        adduser -D -H -u 65534 -G appgroup appuser
    COPY --from=builder /bin/mikrotik-route-sync /usr/local/bin/mikrotik-route-sync
    USER 65534:65534
    WORKDIR /data
    EXPOSE 8080
    VOLUME ["/data", "/var/log/mikrotik-route-sync"]
    HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
        CMD wget --no-verbose --tries=1 --spider http://127.0.0.1:8080/healthz || exit 1
    ENTRYPOINT ["/usr/local/bin/mikrotik-route-sync"]
    CMD ["daemon", "--config", "/data/config.yaml"]

### 16.2 docker-compose.yml

    services:
      mrs:
        build: .
        restart: unless-stopped
        read_only: true
        user: "65534:65534"
        cap_drop: [ALL]
        security_opt: [no-new-privileges:true]
        mem_limit: 128m
        cpus: 1.0
        volumes:
          - ./config.yaml:/data/config.yaml:ro
          - ./data:/data
          - ./logs:/var/log/mikrotik-sync
        ports:
          - "127.0.0.1:8080:8080"

### 16.3 Proxmox LXC

    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
      go build -trimpath -ldflags="-s -w" -o app ./cmd/app

Рекомендации:
- запускать через systemd/supervisor;
- хранить `config.yaml` с правами `0600`;
- сохранять `cache.db` между перезапусками;
- вынести каталог логов на постоянное хранилище.

---

## XVII. Поведение при ошибках (Troubleshooting)

- **Маршруты не создаются:** проверить REST API, gateway, routing table, права RouterOS-пользователя, логи приложения (`app logs tail -f`), `app test-mikrotik`.
- **Collector вернул 0 сетей:** это ошибка безопасности. Существующие маршруты не удаляются. Проверить DNS (`app test-dns`), доступ к BGPView/RIPEstat/официальному источнику, `overrides`.
- **Web UI возвращает 401/403:** проверить Basic Auth, `allowed_cidrs`, CSRF. При работе за reverse proxy — проверить `trusted_proxies`.
- **TLS RouterOS не проходит проверку:** для production установить корректный сертификат. `verify_ssl: false` допустим только в контролируемой домашней сети.
- **Массовое удаление остановлено safety-check:** проверить diff (`app diff <service>`). Если изменения корректны — `app sync --service <service> --force`.
- **Частичный сбой rollback:** сервис помечается как `degraded`. Проверить маршруты вручную, восстановить из снапшота (`app restore <service> --from-snapshot <id> --force`).

---

## XVIII. Проверка перед обновлением RouterOS

Рекомендуемый порядок:

    # 1. Проверка окружения
    ./app test-mikrotik
    ./app test-telegram
    ./app config validate

    # 2. Просмотр информации о сервисе
    ./app info instagram

    # 3. Dry-run (без изменений)
    ./app sync --service instagram --dry-run

    # 4. Применение
    ./app sync --service instagram

    # 5. Проверка в RouterOS
    # /ip/route/print where comment="AUTO:instagram"

---

## XIX. Порядок разработки (рекомендации)

1. Каркас проекта (`cmd/app/main.go`, `internal/*`, `go.mod`, `Makefile`, `Dockerfile`).
2. Конфигурация (`config/config.go` + `save.go` + yaml + ENV overrides + hot-reload + validation).
3. Логирование (`logging/logging.go` + slog + lumberjack + redaction).
4. Storage (`storage/storage.go` — bbolt wrapper).
5. Resolver (`resolver/resolver.go` + `rdap.go` + BGPView + RIPEstat + cache).
6. Classifier (`classifier/classifier.go` + словарь CDN/ASN).
7. Collectors (`collectors/*.go` + registry + http + Circuit Breaker + Retry).
8. Validator (`validator/validator.go` + RFC-диапазоны + RDAP-проверка).
9. Aggregator (`aggregator/radix.go` + `validate.go` + инварианты).
10. MikroTik client (`mikrotik/client.go` + `transaction.go` + rollback + snapshot).
11. Syncer (`core/syncer.go` + `diff.go` + safety-check + mutex).
12. Scheduler (`scheduler/schedule.go` + `scheduler.go` + cron + human-readable).
13. Telegram bot (`bot/bot.go` + auth + rate limit).
14. Notifier (`notifier/notifier.go` + templates).
15. History (`history/history.go` — bbolt bucket).
16. Web UI (`web/server.go` + `secret.go` + templates + static + WebSocket + auth + CSRF + headers).
17. CLI (`cmd/app/main.go` + cobra + все команды через withSyncer).
18. Health checks (`/healthz`).
19. Docker + compose + LXC.
20. Docs (README + ARCHITECTURE + THREAT_MODEL + SECURITY + openapi.yaml + config.example.yaml).

---

## XX. Итоговые требования к качеству

- **Security by default:** никаких `InsecureSkipVerify` по умолчанию, никаких дефолтных паролей, никаких публичных интерфейсов без явного указания.
- **Fail-closed:** при любой неопределённости — не изменять маршруты.
- **Observable:** структурированные логи (slog + JSON), `/healthz`, redaction секретов.
- **Maintainable:** понятная архитектура, разделение ответственности, минимум зависимостей (11 прямых пакетов).
- **Production-ready:** graceful shutdown, retry, circuit breaker, rollback, snapshots.
- **Documented:** README, ARCHITECTURE, THREAT_MODEL, SECURITY, openapi.yaml, config.example.yaml.
- **Compliant:** MIT-лицензия, `.gitignore` (включая `config.yaml`), статический анализ (`gosec`, `govulncheck` — как обязательные шаги перед релизом).
- **Resource-friendly:** укладывается в ≤128 МБ RAM и ≤1 CPU в LXC/Docker.

# Промпт для проекта mikrotik-route-sync / Firewall Address List mode

Ты — senior Go-разработчик, сетевой инженер и security-инженер.

Создай полноценный, готовый к production проект на Go для автоматического управления списками адресов в MikroTik RouterOS v7 через REST API.

Проект должен быть:
- secure by default;
- fail-closed;
- отказоустойчивым;
- наблюдаемым;
- с минимумом зависимостей;
- пригодным для работы в LXC-контейнере на Proxmox x86_64 с ограничениями ≤128 МБ RAM и ≤1 CPU.

## Главная цель

Пользователь указывает только название сервиса (`instagram`, `youtube`, `rutor`, `cloudflare`) — либо домен, IPv4-адрес или ASN.

Всё остальное система делает сама:

1. Определяет тип сервиса: CDN, крупный ASN, динамический сервис, WHOIS/RDAP, публичный список.
2. Автоматически выбирает один или несколько методов сбора IPv4-диапазонов.
3. Собирает CIDR.
4. Строго валидирует, нормализует и агрегирует сети в минимальный набор префиксов.
5. Синхронизирует результат не как маршруты, а как записи Firewall Address List в MikroTik RouterOS v7.
6. Использует один глобальный address-list, заданный в настройках программы, например `TO-VPN`.
7. Автоматически подставляет собранные и оптимизированные IPv4-сети в поле `address`.
8. Автоматически проставляет комментарий на основе имени сервиса/сети, например `AUTO:instagram`.
9. Использует комментарий и имя address-list для автоматического поиска управляемых записей в RouterOS.
10. Изменяет только те записи address-list, которые принадлежат конкретному сервису.
11. Отправляет уведомления в Telegram.
12. Работает по многоуровневому расписанию: глобальное → группа → сервис.
13. Имеет CLI для ручного управления, диагностики и восстановления.
14. Управляется через Telegram-бота с inline-кнопками.
15. Имеет встроенный веб-интерфейс: dashboard, services, schedules, settings, logs, address-list.
16. Работает как статический бинарник в LXC без внешних зависимостей, кроме сетевых API.

---

## I. Целевая модель RouterOS

Проект управляет не `/ip/route`, а:

```text
/ip/firewall/address-list
```

Через RouterOS v7 REST API:

```text
/rest/ip/firewall/address-list
```

### Единый address-list для всех сервисов

Имя Firewall Address List задаётся один раз в настройках программы и используется всеми сервисами.

Пример:

```yaml
firewall:
  address_list: TO-VPN
  comment_prefix: AUTO
```

Все управляемые записи должны иметь одинаковое поле `list`:

```json
{
  "address": "172.64.0.0/13",
  "list": "TO-VPN",
  "comment": "AUTO:cloudflare",
  "disabled": false
}
```

Где:

- `address` — автоматически подставляется из оптимизированного агрегированного IPv4-префикса.
- `list` — один глобальный Firewall Address List, заданный в настройках программы.
- `comment` — автоматически генерируется программой на основе имени сервиса/сети.
- `disabled` — всегда `false` для управляемых записей, если не задано иное в конфигурации.

Нельзя создавать отдельные address-list для разных сервисов.  
Нельзя поддерживать `overrides.<service>.address_list`.  
Единственный источник имени списка — `firewall.address_list`.

### Управляемая запись

Каждая запись, созданная или изменяемая программой, должна иметь:

```text
list = firewall.address_list
comment = AUTO:<service>
dynamic = false
```

Примеры комментариев:

```text
AUTO:instagram
AUTO:youtube
AUTO:cloudflare
AUTO:rutor
```

Комментарий генерируется автоматически. Пользователь не задаёт его вручную.

### Только IPv4

Проект сознательно работает только с IPv4.

IPv6-префиксы:
- отбрасываются коллекторами;
- отбрасываются валидатором;
- не попадают в агрегацию;
- не синхронизируются в RouterOS.

---

## II. Ключевые архитектурные принципы

### 2.1 Изоляция сервисов внутри одного address-list

Все сервисы используют один список, например `TO-VPN`, но изолируются по комментарию.

При синхронизации `instagram` программа должна работать только с записями:

```text
list = TO-VPN
comment = AUTO:instagram
dynamic = false
```

Нельзя трогать:
- записи других сервисов;
- записи без `AUTO:`;
- записи с чужим комментарием;
- dynamic-записи;
- записи в других address-list.

Удаление сервиса:

```bash
app remove-service instagram
```

должно удалять только записи:

```text
list = TO-VPN
comment = AUTO:instagram
dynamic = false
```

### 2.2 Автоматический поиск управляемых записей

Программа должна самостоятельно находить все префиксы, которыми управляет.

Базовый фильтр:

```text
list = <firewall.address_list>
comment = AUTO:<service>
dynamic = false
```

Если RouterOS REST API поддерживает серверный фильтр — использовать его.

Если серверный фильтр ненадёжен — получить записи конкретного `list` и фильтровать локально по точному `comment`.

Нельзя без необходимости читать весь `/ip/firewall/address-list`.

### 2.3 Address подставляется автоматически

Поле `address` заполняется программой автоматически из оптимизированного набора IPv4-префиксов.

Пример:

Собрано:

```text
173.245.48.0/20
173.245.64.0/18
```

После валидации и агрегации:

```text
173.245.48.0/20
173.245.64.0/18
```

или, если безопасно:

```text
173.245.48.0/19
173.245.64.0/18
```

Итоговые CIDR автоматически записываются в `address`.

Пользователь не вводит CIDR вручную для обычной синхронизации.

Ручное включение/исключение допускается только через:

```yaml
overrides:
  rutor:
    include_only:
      - 1.2.3.0/24
    exclude:
      - 1.2.3.4/32
```

### 2.4 Fail-closed

Если resolver, collector, validator, aggregator или MikroTik API вернул ошибку — синхронизация сервиса завершается ошибкой.

Существующие управляемые записи address-list:
- не удаляются;
- не изменяются.

Если после сбора и валидации список CIDR пуст:
- синхронизация прерывается;
- существующие записи не удаляются;
- логируется `WARN` или `ERROR`;
- отправляется уведомление в Telegram.

Нельзя выполнять логику:

```text
удалить всё, что было, и добавить то, что получилось
```

при пустом или подозрительно коротком результате.

### 2.5 Best-effort rollback

Перед применением diff выполняется снимок текущих управляемых записей address-list сервиса.

Снимок сохраняется в bbolt.

При ошибке во время применения diff выполняется восстановление исходного набора записей через REST API RouterOS.

Если rollback частично неуспешен:
- сервис помечается как `degraded`;
- отправляется алерт с высоким приоритетом;
- в логах указывается список проблемных записей без секретов.

### 2.6 Safe-diff: защита от массового удаления

Перед удалением записей address-list проверяется соотношение:

```text
deleted / existing
```

Если будет удалено более `safety.max_delete_ratio` от текущего количества управляемых записей сервиса, синхронизация прерывается и требует ручного подтверждения через CLI:

```bash
app sync --service <name> --force
```

По умолчанию:

```yaml
safety:
  max_delete_ratio: 0.5
```

Если `existing > 0`, а новый набор пуст — прерывать всегда, без исключений.

### 2.7 Идемпотентность

Повторный запуск синхронизации с теми же входными данными не должен приводить к изменениям в RouterOS.

Сравнение записей выполняется по:
- нормализованному `address`;
- глобальному `list`;
- точному `comment`;
- `disabled`.

CIDR сравнивается в нормализованном виде: network + prefix length, без учёта хостовых бит.

### 2.8 Кэширование

Результаты ASN-resolution, WHOIS, RDAP, bgp.tools, RIPEstat кэшируются в bbolt.

TTL кэша задаётся в конфиге:

```yaml
scheduler:
  cache_ttl: 24h
```

Периодическая очистка:

```yaml
scheduler:
  cache_purge: every 1h
```

Кэш используется только как оптимизация, но не как источник истины для удаления записей address-list.

### 2.9 Минимизация прав

Для RouterOS используется отдельный API-пользователь с минимальными правами:

- `read`;
- `write`;
- `rest-api`;
- `api`.

Пользователь должен быть ограничен по IP-адресу или подсети через firewall RouterOS.

Веб-интерфейс слушает только LAN/loopback, если не указано иное.

Docker/LXC:
- без `--privileged`;
- `cap_drop: [ALL]`;
- `security_opt: [no-new-privileges:true]`;
- read-only root FS;
- non-root user.

---

## III. Автоматическое определение метода сбора IP

Логика определения источников остаётся почти без изменений, но результат используется не для маршрутов, а для address-list.

### 3.1 Шаг 1. Определить ASN сервиса

Если указан IPv4-адрес:
- проверить через RDAP, к какому ASN он относится.

Если указан домен:
- резолвить через DNS A-записи;
- определить ASN через WHOIS/RDAP или bgp.tools.

Если указан ASN напрямую:
- использовать его.

Основные источники:
1. bgp.tools
2. RIPEstat
3. RDAP: ARIN, RIPE, APNIC, LACNIC, AFRINIC

Результат кэшируется в bbolt с TTL.

При неопределённости, например несколько ASN для домена, использовать эвристику:
- приоритет ASN с наибольшим количеством анонсируемых IPv4-префиксов.

### 3.2 Шаг 2. Классификация сервиса

Поддерживаемые методы:

- `cdn`
- `asn`
- `dynamic`
- `whois`
- `static_url`
- комбинированные списки методов, например `[cdn, dynamic]`

Известные CDN:

- Cloudflare AS13335 → `https://www.cloudflare.com/ips-v4`
- AWS CloudFront AS16509 → `https://ip-ranges.amazonaws.com/ip-ranges.json`, фильтр по service и IPv4
- Google AS15169 → `https://www.gstatic.com/ipranges/goog.json`, только IPv4
- Fastly → `https://api.fastly.com/public-ip-list`, только IPv4
- Akamai → официальный API, если задан ключ

Крупные сервисы с собственным ASN:
- метод `asn` через bgp.tools/RIPEstat.

Динамические сервисы:
- метод `dynamic`: DNS + официальные JSON + агрегация.

Мелкие сайты без стабильного ASN:
- метод `whois`: DNS → ASN → префиксы.
- обязателен лимит `max_asn_prefixes`.

Публичные списки:
- метод `static_url`.

Результат всех методов объединяется, валидируется и агрегируется.

### 3.3 Шаг 3. Multi-layer Validation

Уровень 1. Синтаксис:
- проверка формата CIDR;
- нормализация в network-адрес;
- допускается только IPv4;
- диапазон префикса: по умолчанию `/8../32`.

Уровень 2. Приватные и служебные диапазоны:
- отфильтровать RFC1918;
- loopback;
- link-local;
- CGNAT;
- TEST-NET;
- multicast;
- reserved;
- IANA special-purpose.

Уровень 3. Проверка принадлежности ASN:
- для методов `asn` и `whois` проверить через RDAP, что префикс принадлежит указанному ASN.

Уровень 4. Ширина префикса:
- отклонять слишком широкие сети;
- минимум задаётся в `safety.min_prefix_v4`.

Уровень 5. Пользовательские фильтры:
- `overrides.<service>.exclude`;
- `overrides.<service>.include_only`;
- `overrides.<service>.max_prefixes`.

Уровень 6. Дедупликация:
- удалить точные дубликаты;
- удалить вложенные префиксы, если они полностью покрыты другими.

### 3.4 Шаг 4. Агрегация

Использовать префиксное дерево radix tree.

Агрегация должна:
- объединять только настоящие sibling-префиксы;
- не расширять покрытие;
- сохранять точное множество IPv4-адресов.

Проверять инварианты:
- сумма адресов до агрегации равна сумме адресов после агрегации;
- каждый исходный префикс полностью покрыт результирующим набором.

При нарушении инварианта:
- откатить агрегацию;
- логировать `ERROR`;
- использовать неагрегированный, но валидированный список.

Валидация агрегации выносится в `aggregator/validate.go`.

---

## IV. Инкрементальная синхронизация Firewall Address List

### 4.1 Добавление нового сервиса

Команда:

```bash
app add-service <service>
```

Алгоритм:

1. Валидировать имя сервиса.
2. Взять глобальный `firewall.address_list`.
3. Определить метод или методы сбора.
4. Собрать IPv4 CIDR только для нового сервиса.
5. Валидировать, нормализовать, агрегировать.
6. Проверить safety-ограничения.
7. Получить с MikroTik управляемые записи:
   ```text
   list = TO-VPN
   comment = AUTO:<service>
   dynamic = false
   ```
8. Вычислить diff:
   - add
   - remove
   - unchanged
9. Применить diff транзакционно.
10. Определить эффективное расписание:
    - сервис → группа → глобальное.
11. Записать сервис в `config.yaml` атомарно.
12. Уведомить в Telegram.

### 4.2 Синхронизация

Команда:

```bash
app sync
```

Для каждого сервиса независимо:

```text
collect → validate → aggregate → get managed address-list entries → diff → apply → history → notify
```

Параллельность ограничивается:

```yaml
scheduler:
  max_concurrent: 3
```

Используется mutex на сервис.

Повторный запуск по тому же сервису пропускается с `WARN`.

Ошибка одного сервиса не останавливает остальные.

Итоговый отчёт агрегируется и отправляется в Telegram.

### 4.3 Dry-run

Команда:

```bash
app sync --dry-run
```

или:

```bash
app sync --service instagram --dry-run
```

Полностью проходит все шаги, но не выполняет изменений в RouterOS.

Выводит diff в JSON.

Пример структуры diff:

```json
{
  "service": "instagram",
  "list": "TO-VPN",
  "comment": "AUTO:instagram",
  "add": [
    "157.240.0.0/16"
  ],
  "remove": [
    "157.240.1.0/24"
  ],
  "unchanged": [
    "31.13.64.0/18"
  ]
}
```

### 4.4 Транзакционность на уровне сервиса

Порядок операций:

1. Сначала `POST` новых записей address-list.
2. Затем `DELETE` устаревших записей.

Это снижает риск временной потери покрытия, если записи address-list используются в firewall rules.

При сбое любого шага выполняется rollback.

Максимум ретраев на операцию:

```yaml
retry:
  max_attempts: 3
```

Используется экспоненциальная задержка и jitter.

### 4.5 Snapshots

Перед каждой синхронизацией создаётся снимок текущих управляемых записей address-list сервиса.

Хранение:
- bbolt;
- bucket `address_list_snapshots`.

TTL по умолчанию:

```yaml
snapshots:
  ttl: 168h
```

Команды:

```bash
app backup <service>
app restore <service> --from-snapshot <id> --force
app restore <service> --from-file <file.json> --force
app snapshots list <service>
app snapshots delete <service> <snapshot-id>
app snapshots cleanup <service> --ttl <hours>
```

Формат backup:

```json
{
  "service": "cloudflare",
  "list": "TO-VPN",
  "comment": "AUTO:cloudflare",
  "created_at": "2026-09-26T12:00:00Z",
  "entries": [
    {
      "address": "173.245.48.0/20",
      "list": "TO-VPN",
      "comment": "AUTO:cloudflare",
      "disabled": false
    }
  ]
}
```

### 4.6 Удаление сервиса и аудит AUTO-записей

Команда:

```bash
app remove-service <service>
```

Удаляет только записи:

```text
list = TO-VPN
comment = AUTO:<service>
dynamic = false
```

Дополнительно программа должна обнаруживать orphan-записи:

```text
list = TO-VPN
comment начинается с AUTO:
но комментарий не соответствует активному сервису
```

По умолчанию orphan-записи не удаляются.

Для аудита:

```bash
app audit-address-list
```

Для очистки только явно:

```bash
app cleanup-auto-entries --force
```

Нельзя автоматически удалять записи, которые не принадлежат текущим активным сервисам, без явного флага `--force`.

---

## V. Система расписаний

Логика расписаний остаётся без изменений.

Приоритет:

```text
сервис → группа → глобальное
```

Поддерживаются форматы:

- cron 5 полей:

```text
0 */6 * * *
```

- простой интервал:

```text
every 6h
every 30m
```

- человекочитаемый:

```text
daily at 03:00
weekly on sunday at 04:00
```

- специальные:

```text
manual
disabled
inherit
```

Пример:

```yaml
schedules:
  global: every 6h
  groups:
    social:
      schedule: daily at 03:00
      services: [instagram, telegram]
    video:
      schedule: every 12h
      services: [youtube]
  services:
    cloudflare:
      schedule: every 1h
```

Планировщик:
- защищает сервис от наложения через mutex;
- ограничивает общую параллельность;
- поддерживает SIGHUP;
- поддерживает периодический reload;
- учитывает timezone и DST.

---

## VI. Управление через Telegram-бота

Команды:

```text
/start
/menu
/status
/sync
/schedule
/help
```

Бот должен показывать:
- имя сервиса;
- глобальный address-list;
- комментарий;
- количество управляемых записей;
- статус последней синхронизации;
- ошибки без утечки секретов.

Авторизация:
- только `chat_id` из `authorized_chat_ids`;
- остальные сообщения игнорируются;
- попытки логируются с `WARN`.

Rate limit:

```yaml
telegram:
  rate_limit: 1
```

По умолчанию:

```yaml
telegram:
  enabled: false
```

---

## VII. Веб-интерфейс

Встроенный dashboard через `//go:embed`.

Страницы:

| URL | Назначение |
|---|---|
| `/` | Dashboard: статус, сводка, сервисы, quick actions |
| `/services` | Управление сервисами, синхронизация, удаление |
| `/schedules` | Расписания, inline-редактирование, effective schedule |
| `/settings` | MikroTik, Firewall Address List, Telegram, Web, Scheduler, Safety, Retry, External, Logging |
| `/logs` | Логи, фильтрация, очистка, скачивание |
| `/address-list` | Текущие управляемые записи в глобальном address-list |

Технически:
- `net/http` + `github.com/go-chi/chi/v5`;
- `html/template` + `//go:embed`;
- CSS;
- HTMX + vanilla JS;
- WebSocket через `github.com/gorilla/websocket`;
- Basic Auth + session cookie + CSRF;
- ограничение по `allowed_cidrs`;
- security headers;
- маскирование секретов.

В UI для каждого сервиса должно отображаться:

```text
Service: cloudflare
Address List: TO-VPN
Comment: AUTO:cloudflare
Managed entries: 12
Last sync: success
```

---

## VIII. REST API

Единый формат ответа:

```json
{
  "ok": true,
  "data": {},
  "error": null
}
```

Endpoints:

| Метод | Endpoint | Назначение |
|---|---|---|
| GET | `/healthz` | liveness |
| GET | `/readyz` | readiness: RouterOS address-list API + bbolt |
| GET | `/api/v1/status` | общий статус |
| GET | `/api/v1/services` | список сервисов |
| POST | `/api/v1/services` | добавить сервис |
| DELETE | `/api/v1/services/{name}` | удалить сервис |
| POST | `/api/v1/services/sync` | запустить синхронизацию |
| POST | `/api/v1/services/{name}/sync` | синхронизировать сервис |
| POST | `/api/v1/services/{name}/dry-run` | рассчитать diff |
| GET | `/api/v1/address-list` | записи глобального address-list |
| GET | `/api/v1/address-list/{service}` | управляемые записи сервиса |
| GET | `/api/v1/schedules` | расписания |
| PUT | `/api/v1/schedules/{service}` | изменить расписание |
| GET | `/api/v1/logs` | логи |
| GET | `/api/v1/history` | история синхронизаций |
| WS | `/api/v1/ws` | live-обновления |

Изменяющие запросы требуют аутентификацию и CSRF, если используется cookie-сессия.

Ограничение тела запроса:

```text
≤1 МБ
```

Таймауты сервера обязательны.

---

## IX. Логирование и наблюдаемость

Формат: JSON через `log/slog`.

Вывод:
- stdout;
- файл.

Ротация:
- `gopkg.in/natefinch/lumberjack.v2`.

Параметры:

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

Структурированные поля:

```text
service
list
comment
method
duration_ms
added
removed
unchanged
error
```

Запрещено логировать:
- пароли;
- токены;
- Authorization;
- cookie.

Redaction обязателен в:
- логах;
- CLI;
- Web UI;
- Telegram;
- REST API ошибках.

Health checks:

```text
/healthz
/readyz
```

`/readyz` должен проверять:
- доступность RouterOS REST API;
- возможность чтения `/ip/firewall/address-list`;
- доступность bbolt.

---

## X. Хранение секретов

Пароли и ключи хранятся в `config.yaml`.

Права файла:

```text
0600
```

Проверка прав при старте.

Файл добавлен в `.gitignore`.

Приоритет источников:

```text
ENV > config.yaml > defaults
```

Поддерживаемые ENV:

```text
MRS_MIKROTIK_PASSWORD
MRS_TELEGRAM_BOT_TOKEN
MRS_WEB_PASSWORD
MRS_BGP_TOOLS_CONTACT
MRS_AKAMAI_API_KEY
```

Служебный ENV:

```text
MRS_REQUIRE_FILE_PERMS
```

Атомарная запись конфига:
- tmpfile;
- fsync;
- os.Rename.

Маскирование секретов:
- показывать только последние 4 символа;
- кнопка reveal на 10 секунд в Web UI.

---

## XI. Архитектура и модули

```text
cmd/
  app/
    main.go

internal/
  addresslist/
    model.go
    diff.go
    safety.go

  aggregator/
    aggregator.go
    radix.go
    validate.go

  bot/
    bot.go

  classifier/
    classifier.go

  collectors/
    collectors.go
    asn.go
    cdn.go
    dynamic.go
    http.go
    registry.go
    ripestat.go
    static.go
    whois.go

  config/
    config.go
    save.go

  core/
    syncer.go
    diff.go

  history/
    history.go

  logging/
    logging.go

  mikrotik/
    client.go
    addresslist.go
    transaction.go

  notifier/
    notifier.go

  resolver/
    resolver.go
    rdap.go

  scheduler/
    schedule.go
    scheduler.go

  storage/
    storage.go

  validator/
    validator.go

  web/
    server.go
    secret.go
    templates/
    static/
```

### Разделение ответственности

- Collector — только сбор сырых IPv4-сетей.
- Validator — только валидация и нормализация.
- Aggregator — только безопасная агрегация CIDR.
- AddressList model — описание целевой записи address-list.
- Syncer — оркестрация: collector → validator → aggregator → diff → MikroTik address-list.
- MikroTik Client — только REST-операции над `/ip/firewall/address-list`.

### DI

Явная передача зависимостей через конструкторы.

Никаких глобальных переменных и `init()`-магии.

Хелпер:

```go
withSyncer()
```

загружает конфиг, открывает bbolt, создаёт Syncer и передаёт зависимости в команды.

---

## XII. Безопасность

### 12.1 Безопасность конфигурации

- проверка прав `0600`;
- атомарная запись;
- валидация всей конфигурации;
- отказ старта при критичных ошибках;
- обязательное наличие `mikrotik.host`;
- обязательное наличие `firewall.address_list`.

### 12.2 Безопасность сети

- Web UI слушает только указанный адрес;
- по умолчанию `127.0.0.1:8080`;
- `allowed_cidrs` для всех входящих, кроме `/healthz`;
- TLS ≥1.2, предпочтительно 1.3;
- `verify_ssl: true` по умолчанию;
- HSTS при HTTPS;
- rate limiting;
- security headers;
- ограничение размера запросов.

### 12.3 Безопасность MikroTik

- отдельный API-пользователь;
- минимальные права;
- ограничение по source IP;
- `verify_ssl: true` по умолчанию;
- rate limit к RouterOS;
- circuit breaker;
- retry с jitter;
- никогда не логировать пароль и Authorization.

### 12.4 Безопасность внешних API

- таймауты на все HTTP-запросы;
- ограничение размера ответа;
- проверка Content-Type;
- ограничение редиректов;
- проверка TLS-сертификата;
- sanitization URL;
- только `http`/`https`;
- защита от SSRF;
- блокировка private/loopback/link-local адресов;
- circuit breaker для каждого источника.

### 12.5 Безопасность Telegram

- авторизация по `authorized_chat_ids`;
- не отвечать неавторизованным;
- rate limit;
- логировать попытки доступа;
- не передавать секреты.

### 12.6 Безопасность контейнера

Multi-stage build:

```text
golang:1.27.1-alpine → alpine:3.20
```

- static binary;
- `CGO_ENABLED=0`;
- `-trimpath`;
- `-ldflags="-s -w"`;
- non-root `65534:65534`;
- read-only root FS;
- `cap_drop: [ALL]`;
- `no-new-privileges:true`;
- `mem_limit: 128m`;
- `cpus: 1.0`;
- healthcheck через `/healthz`.

### 12.7 Защита от утечек данных

Redaction полей:

```text
password
token
secret
api_key
authorization
cookie
```

### 12.8 Supply chain

- `go.sum` зафиксирован;
- `govulncheck`;
- `gosec`;
- только популярные поддерживаемые зависимости.

### 12.9 Входные данные

Валидация:

- имя сервиса: `^[a-z0-9][a-z0-9_-]{0,63}$`
- ASN: `^AS\d{1,10}$`
- CIDR: корректный IPv4 format
- URL: только http/https
- cron: через `robfig/cron/v3`
- address-list name: безопасное имя RouterOS, например `^[A-Za-z0-9_.-]{1,63}$`
- комментарий: автоматически генерируется и санитизируется

---

## XIII. Отказоустойчивость

### Retry

Экспоненциальная задержка:

```text
base_delay * 2^attempt + jitter
```

По умолчанию:

```yaml
retry:
  max_attempts: 3
  base_delay: 1s
  max_delay: 30s
  jitter: true
```

Retry только для идемпотентных операций и сетевых ошибок:
- 5xx;
- timeout;
- connection refused.

Не retry для 4xx, кроме 429.

### Circuit Breaker

`github.com/sony/gobreaker`.

Состояния:

```text
Closed → Open → Half-Open → Closed
```

Порог открытия: 5 ошибок подряд.

Таймаут Open: 60 секунд.

Отдельный breaker для:
- MikroTik;
- bgp.tools;
- RIPEstat;
- RDAP;
- Telegram;
- каждого внешнего CDN/source.

### Восстановление после краха

- критические операции пишут состояние в bbolt;
- graceful shutdown по SIGTERM/SIGINT;
- завершение текущих операций;
- закрытие соединений;
- flush логов.

### Бэкапы

Перед каждой синхронизацией:
- снимок управляемых записей address-list сервиса в bbolt.

Экспорт/восстановление через CLI.

---

## XIV. CLI

Используются те же команды, но семантика изменена с routes на firewall address-list entries.

| Команда | Назначение |
|---|---|
| `app sync` | Синхронизировать все сервисы |
| `app sync --service <name>` | Синхронизировать один сервис |
| `app sync --group <name>` | Синхронизировать группу |
| `app sync --dry-run` | Рассчитать изменения без применения |
| `app sync --service <name> --force` | Обойти safety-check |
| `app diff <service>` | Показать diff address-list в JSON |
| `app list` | Список сервисов и расписаний |
| `app info <service>` | Информация о сервисе |
| `app add-service <service>` | Добавить и первоначально синхронизировать сервис |
| `app remove-service <service>` | Удалить сервис и только его управляемые address-list entries |
| `app backup <service>` | Экспорт управляемых записей в JSON |
| `app restore <service> --from-file f.json --force` | Восстановить из файла |
| `app restore <service> --from-snapshot <id> --force` | Восстановить из снапшота |
| `app snapshots list <service>` | Список снапшотов |
| `app snapshots delete <service> <id>` | Удалить снапшот |
| `app snapshots cleanup <service> --ttl <hours>` | Очистка старых снапшотов |
| `app address-list` | Показать глобальный address-list |
| `app address-list --service <name>` | Показать записи сервиса |
| `app audit-address-list` | Найти AUTO-записи без активного сервиса |
| `app cleanup-auto-entries --force` | Удалить потерянные AUTO-записи |
| `app schedule list` | Показать эффективные расписания |
| `app schedule reload` | Перечитать расписания |
| `app bot` | Запустить Telegram-бота |
| `app web` | Запустить Web UI |
| `app daemon` | Запустить всё: bot + web + scheduler |
| `app config validate` | Проверить конфигурацию |
| `app config reload` | Отправить SIGHUP |
| `app config edit` | Открыть конфиг в `$EDITOR` |
| `app config get <key>` | Прочитать параметр, секреты маскируются |
| `app logs tail -n 100` | Последние строки лога |
| `app logs tail -f` | Follow лога |
| `app logs clear` | Очистить логи |
| `app logs size` | Размер логов |
| `app test-telegram` | Проверить Telegram |
| `app test-mikrotik` | Проверить RouterOS REST API и доступ к address-list |
| `app test-dns <domain>` | Проверить резолвинг |
| `app version` | Версия |

Глобальный флаг:

```bash
--config /path/to/config.yaml
```

---

## XV. Пример конфигурации

```yaml
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
  password: "SET_A_SECRET"
  use_ssl: true
  verify_ssl: true
  timeout: 30s
  rate_limit: 20

firewall:
  address_list: TO-VPN
  comment_prefix: AUTO
  ignore_dynamic: true
  manage_disabled: false

telegram:
  enabled: false
  bot_token: ""
  chat_id: ""
  authorized_chat_ids: []
  rate_limit: 1

web:
  enabled: true
  listen: 127.0.0.1:8080
  allowed_cidrs:
    - 127.0.0.0/8
    - 192.168.0.0/16
    - 10.0.0.0/8
    - 172.16.0.0/12
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
  allow_host_routes: true
  max_asn_prefixes: 100

retry:
  max_attempts: 3
  base_delay: 1s
  max_delay: 30s
  jitter: true

external:
  http_timeout: 15s
  max_response_mb: 50
  bgp_tools_contact: "you@example.com"
  akamai_api_key: ""
  rdap_timeout: 10s
  resolver: 1.1.1.1:53

snapshots:
  enabled: true
  ttl: 168h
  max_count: 50

schedules:
  global: every 6h
  groups:
    social:
      schedule: daily at 03:00
      services: [instagram, telegram]
    video:
      schedule: every 12h
      services: [youtube]
  services:
    cloudflare:
      schedule: every 1h

services:
  - cloudflare
  - instagram
  - youtube
  - rutor

overrides:
  rutor:
    domains:
      - rutor.info
      - rutor.org
    max_asn_prefixes: 50
    exclude:
      - 1.2.3.0/24

  youtube:
    method: dynamic
    domains:
      - youtube.com
      - googlevideo.com
      - ytimg.com
    also_cdn:
      - google
```

Важно:

```yaml
overrides.<service>.address_list
```

не поддерживается.

Единственный address-list:

```yaml
firewall.address_list
```

---

## XVI. Технические требования

Go:

```text
1.27.1
```

Зависимости:

```text
github.com/go-chi/chi/v5 v5.2.3
github.com/go-telegram-bot-api/telegram-bot-api/v5 v5.5.1
github.com/gorilla/websocket v1.5.3
github.com/hashicorp/go-retryablehttp v0.7.8
github.com/robfig/cron/v3 v3.0.1
github.com/sony/gobreaker v1.0.0
github.com/spf13/cobra v1.10.1
go.etcd.io/bbolt v1.4.3
golang.org/x/time v0.12.0
gopkg.in/natefinch/lumberjack.v2 v2.2.1
gopkg.in/yaml.v3 v3.0.1
```

Стандартная библиотека:

```text
net/http
net
encoding/json
crypto/tls
log/slog
html/template
embed
crypto/rand
crypto/subtle
os/signal
syscall
```

Не использовать:

```text
spf13/viper
go-chi/httplog
go-chi/httprate
OpenTelemetry
```

Сборка:

```bash
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o app ./cmd/app
```

Docker:
- multi-stage;
- non-root;
- read-only;
- cap_drop ALL;
- healthcheck.

Makefile:

```makefile
build
test
fmt
vet
clean
```

Документация:

```text
README.md
LICENSE
docs/ARCHITECTURE.md
docs/SECURITY.md
docs/THREAT_MODEL.md
docs/openapi.yaml
config.example.yaml
```

---

## XVII. Docker и деплой

### Dockerfile (multi-stage)

```dockerfile
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

# Stage 2: Runtime
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
```

### docker-compose.yml

```yaml
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
      - ./logs:/var/log/mikrotik-route-sync
    ports:
      - "127.0.0.1:8080:8080"
```

### Proxmox LXC

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -trimpath -ldflags="-s -w" -o app ./cmd/app
```

Рекомендации:
- запускать через systemd/supervisor;
- хранить `config.yaml` с правами `0600`;
- сохранять `cache.db` между перезапусками;
- вынести каталог логов на постоянное хранилище.

---

## XVIII. Поведение при ошибках

### Записи address-list не создаются

Проверить:
- REST API RouterOS;
- права пользователя;
- доступ к `/ip/firewall/address-list`;
- имя `list`;
- комментарий;
- логи;
- `app test-mikrotik`.

### Collector вернул 0 сетей

Это ошибка безопасности.

Существующие записи address-list не удаляются.

Проверить:
- DNS;
- доступ к bgp.tools;
- RIPEstat;
- RDAP;
- официальные CDN-списки;
- `overrides`.

### Web UI возвращает 401/403

Проверить:
- Basic Auth;
- `allowed_cidrs`;
- CSRF;
- `trusted_proxies`.

### TLS RouterOS не проходит проверку

Для production использовать корректный сертификат.

`verify_ssl: false` допустим только в контролируемой домашней сети.

### Массовое удаление остановлено safety-check

Проверить:

```bash
app diff <service>
```

Если изменения корректны:

```bash
app sync --service <service> --force
```

### Частичный сбой rollback

Сервис помечается как `degraded`.

Проверить записи address-list вручную.

Восстановить:

```bash
app restore <service> --from-snapshot <id> --force
```

---

## XIX. Проверка перед применением

Рекомендуемый порядок:

```bash
./app test-mikrotik
./app test-telegram
./app config validate
./app info instagram
./app sync --service instagram --dry-run
./app sync --service instagram
```

Проверка в RouterOS:

```routeros
/ip/firewall/address-list/print where list="TO-VPN" and comment="AUTO:instagram"
```

или:

```routeros
/ip/firewall/address-list/print where list="TO-VPN"
```

---

## XX. Порядок разработки (рекомендации)

1. Каркас проекта (`cmd/app/main.go`, `internal/*`, `go.mod`, `Makefile`, `Dockerfile`).
2. Конфигурация (`config/config.go` + `save.go` + yaml + ENV overrides + hot-reload + validation).
3. Логирование (`logging/logging.go` + slog + lumberjack + redaction).
4. Storage (`storage/storage.go` — bbolt wrapper).
5. Resolver (`resolver/resolver.go` + `rdap.go` + bgp.tools + RIPEstat + cache).
6. Classifier (`classifier/classifier.go` + словарь CDN/ASN).
7. Collectors (`collectors/*.go` + registry + http + Circuit Breaker + Retry).
8. Validator (`validator/validator.go` + RFC-диапазоны + RDAP-проверка).
9. Aggregator (`aggregator/radix.go` + `validate.go` + инварианты).
10. AddressList model (`addresslist/model.go` + `diff.go` + `safety.go`).
11. MikroTik client (`mikrotik/client.go` + `addresslist.go` + `transaction.go` + rollback + snapshot).
12. Syncer (`core/syncer.go` + `diff.go` + safety-check + mutex).
13. Scheduler (`scheduler/schedule.go` + `scheduler.go` + cron + human-readable).
14. Telegram bot (`bot/bot.go` + auth + rate limit).
15. Notifier (`notifier/notifier.go` + templates).
16. History (`history/history.go` — bbolt bucket).
17. Web UI (`web/server.go` + `secret.go` + templates + static + WebSocket + auth + CSRF + headers).
18. CLI (`cmd/app/main.go` + cobra + все команды через withSyncer).
19. Health checks (`/healthz`, `/readyz`).
20. Docker + compose + LXC.
21. Docs (README + ARCHITECTURE + THREAT_MODEL + SECURITY + openapi.yaml + config.example.yaml).

---

## XXI. Итоговые требования к качеству

- Security by default.
- Fail-closed.
- Observable.
- Maintainable.
- Production-ready.
- Documented.
- Resource-friendly.
- Управление только Firewall Address List, не routes.
- Один глобальный address-list из настроек, например `TO-VPN`.
- Все сервисы используют один и тот же `list`.
- Изоляция сервисов через `comment = AUTO:<service>`.
- Программа автоматически ищет управляемые записи по `list + comment + dynamic=false`.
- Поле `address` автоматически заполняется из оптимизированных IPv4-сетей.
- Комментарий генерируется автоматически.
- Ошибка одного сервиса не влияет на другие сервисы.
- Один сервис — одна изолированная область синхронизации.

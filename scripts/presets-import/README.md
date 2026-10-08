# presets-import — генератор пресетов

Пересобирает `internal/presets/data/*.yaml` из апстрим-источников.
Результат коммитится в репозиторий: в рантайме приложение ничего не
качает, домены лежат в бинаре через `embed`.

## Запуск

```bash
go run ./scripts/presets-import
```

Флаги:

| Флаг | Умолчание | Назначение |
| --- | --- | --- |
| `-out` | `internal/presets/data` | каталог для YAML |
| `-rublock` | `true` | качать antifilter.download и собирать пресет `rublock` |
| `-rublock-max` | `0` | кап для rublock; `0` = без капа (полный список) |
| `-timeout` | `10m` | таймаут скачивания |
| `-user-agent` | `mikrotik-route-sync/presets-import` | User-Agent |

Полезно при отладке и в CI:

```bash
# только v2fly, без 40 МБ блоклиста РКН
go run ./scripts/presets-import -rublock=false

# сухой прогон на маленьком блоклисте
go run ./scripts/presets-import -rublock=true -rublock-max=100
```

> **Bootstrap.** `internal/presets` вшивает `data/*.yaml` через `embed`,
> поэтому генератор не собирается, пока каталог пуст. Для первого
> прогона в чистом клоне:
>
> ```bash
> mkdir -p internal/presets/data && touch internal/presets/data/meta.yaml
> go run ./scripts/presets-import
> ```

## Что качает

| Источник | URL | Пинован | Что даёт |
| --- | --- | --- | --- |
| v2fly/domain-list-community | `codeload.github.com/v2fly/domain-list-community/tar.gz/<commit>` | да, константа `upstreamCommit` | 32 сервисных пресета + 2 категории |
| antifilter.download | `https://antifilter.download/list/domains.lst` | нет (`unpinned`) | пресет `rublock` (~1.7 млн доменов, `large: true`) |

`upstreamCommit` и `upstreamDate` заданы константами в `main.go` и
пишутся в `meta.yaml` и в `source:` каждого пресета. Коммит меняется
только осознанно — это и есть «зафиксировать версию источника».

Скачивание antifilter не закреплено версией и не является причиной падения:
при недоступности генератор печатает `WARN` и собирает пресеты без
`rublock`. Отсутствующий апстрим-список v2fly — тоже `WARN` + пропуск,
не ошибка.

## Таблица конвертации

Формат входа v2fly (`data/<list>`):

```
# комментарий
include:другой-список          # рекурсивно
full:точный.домен              # только этот домен
domain:домен.ру                # домен + поддомены
домен.без.точки                # = domain:, отбрасывается
keyword:что-то                 # отбрасывается
regexp:^ads[0-9]+              # отбрасывается
example.com @ads               # отбрасывается (реклама/трекинг)
example.cn @!cn                # остаётся (регион не влияет на выбор)
example.com # inline-комментарий
```

| Запись | Действие | Счётчик |
| --- | --- | --- |
| `full:X` | принять `X` | `Full` |
| `domain:X` / голое `X` | принять, если в `X` есть точка | `Domain` |
| `domain:X` без точки | отбросить | `DotlessDomain` |
| `keyword:X` | отбросить | `Keyword` |
| `regexp:X` | отбросить | `Regexp` |
| `include:X` | развернуть рекурсивно | `Includes` |
| `include:X` уже разобран | пропустить (дедуп) | `IncludeDup` |
| `include:X` в цикле | остановить обход | `IncludeCycles` |
| `include:X`, файла нет | пропустить + `WARN` | `MissingIncludes` |
| `@ads`, `@telemetry` | отбросить | `Ads` |
| `@cn`, `@!cn`, прочие | оставить запись | — |
| `# ...` и inline `#` | отбросить | `Comments` |
| не домен (URL, пробелы, `nodots`) | отбросить | `Invalid` |
| повтор домена в пресете | оставить первый | `Duplicates` |

Дополнительно:

- **Нормализация**: lower-case, снятие кавычек (в antifilter они
  встречаются), хвостовой точки и ведущей точки; метки — буквы/цифры
  Unicode, `-`, `_`. Кириллические IDN (`.рф`, `.рус`) сохраняются:
  это ~0.5 % блоклиста РКН.
- **Витринный apex первым**: `Domains[0]` — узнаваемый apex категории
  (`youtube.com`, `telegram.org`, `deepmind.com`). Он же
  `ServiceOverride.Primary` в конфиге: с него идут ASN-резолв и
  SNI-проба. Задан явно в каталоге `internal/presets/import.go`; если
  домен не найден — детерминированный фолбэк + `WARN` в stdout.
- **Сортировка детерминирована**: витринный первый, остальное
  лексикографически (побайтово, поэтому IDN уходят в конец).
- **Кап 50 доменов** на обычный пресет, обрезка с `WARN` (в логе
  пресеты помечены `!`). `rublock` — исключение: `large: true`, без
  капа, с предупреждением о размере файла.
- **Нет map-итерации в выводе**: порядок зависит только от входа.

## Выход

```
internal/presets/data/
  youtube.yaml        # slug, title, domains[], method, large?, source{repo,commit,date}
  ...
  rublock.yaml        # large: true, ~1.7 млн доменов, ~39 МБ
  meta.yaml           # upstream{repo,commit,date}, generated_at, generator,
                      # preset_count, domains, rublock_entries, presets[] (индекс)
```

`meta.yaml` дополнительно содержит индекс `presets[]` (slug, title,
method, large, count, source — без доменов). Он нужен, чтобы
`presets.List()` не разбирал `rublock.yaml`: холодный `List()` без
индекса — это ~1.1 с и ~315 МБ, с индексом — ~0.7 мс.

Файлы пишутся атомарно (`.tmp` + rename), устаревшие `*.yaml` в
каталоге удаляются, так что переименование пресета не оставляет мусор.

> **Размер бинаря.** `rublock.yaml` вшивается в бинарь целиком и
> добавляет ~39 МиБ к любой сборке, которая импортирует
> `internal/presets`. Парсится он только при `Get("rublock")`
> (~2 с, ~630 МБ аллокаций) — `List()` его не трогает. Если такой вес
> неприемлем, варианты: не вшивать rublock (отдельный файл на диске) или
> разбить его на чанки с ленивой загрузкой.

## Проверка после генерации

```bash
gofmt -l internal/presets scripts/presets-import
go vet ./internal/presets/
go test ./internal/presets/ -count=1
```

Тесты (`internal/presets/presets_test.go`) проверяют и встроенные
данные (`List`, `Get("youtube")`, `Get("rublock")`, витринный домен,
соответствие индекса файлам), и конвертацию на golden-фикстуре с
include-циклом, `@ads`/keyword/regexp/безточечными записями, капом и
сортировкой.
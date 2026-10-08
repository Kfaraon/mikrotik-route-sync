// Package presets даёт готовые списки доменов (пресеты) для сервисов
// и блоклистов.
//
// Данные лежат в data/*.yaml и вшиты в бинарь через embed: внешних
// зависимостей и сетевых обращений в рантайме нет. Файлы генерируются
// генератором scripts/presets-import (см. scripts/presets-import/README.md).
//
// Формат файла пресета:
//
//	slug: youtube
//	title: YouTube
//	domains:
//	  - youtube.com
//	  - youtu.be
//	method: static_url          # совместимо с config.knownMethods
//	large: true                 # необязательное, только для больших списков
//	source:
//	  repo: v2fly/domain-list-community
//	  commit: e84921279c9142434b43cb84ec6c17f987e025df
//	  date: 2026-10-08T09:30:42Z
//
// Первый домен (Domains[0]) — витринный apex: он же ServiceOverride.Primary
// в конфиге, с него идут ASN-резолв и SNI-проба.
package presets

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

//go:embed data/*.yaml
var dataFS embed.FS

const (
	dataDir  = "data"
	metaName = "meta.yaml"
	metaSlug = "meta"
	yamlExt  = ".yaml"
)

// PresetSource — provenance пресета: откуда взяты домены.
type PresetSource struct {
	Repo   string `yaml:"repo" json:"repo"`
	Commit string `yaml:"commit" json:"commit"`
	Date   string `yaml:"date" json:"date"`
}

// Empty — источник не задан (используется в тестах и как zero value).
func (s PresetSource) Empty() bool { return s.Repo == "" && s.Commit == "" && s.Date == "" }

// String — компактное представление источника для логов.
func (s PresetSource) String() string {
	if s.Empty() {
		return "unknown"
	}
	return fmt.Sprintf("%s@%s (%s)", s.Repo, shortCommit(s.Commit), s.Date)
}

func shortCommit(c string) string {
	if len(c) > 12 {
		return c[:12]
	}
	return c
}

// Preset — готовый список доменов одного сервиса/категории.
type Preset struct {
	Slug    string   `yaml:"slug" json:"slug"`
	Title   string   `yaml:"title" json:"title"`
	Domains []string `yaml:"domains" json:"domains"`
	Method  string   `yaml:"method" json:"method"`
	// StaticURL — для method=static_url (например, RU IP-лист).
	StaticURL string       `yaml:"static_url,omitempty" json:"static_url,omitempty"`
	Large     bool         `yaml:"large,omitempty" json:"large,omitempty"`
	Source    PresetSource `yaml:"source" json:"source"`
}

// PresetMeta — строчка индекса пресетов из meta.yaml: всё, кроме
// доменов. Индекс нужен, чтобы List() не разбирал rublock.yaml
// (1.7 млн доменов) ради выпадающего списка в UI.
type PresetMeta struct {
	Slug   string       `yaml:"slug" json:"slug"`
	Title  string       `yaml:"title" json:"title"`
	Method string       `yaml:"method" json:"method"`
	Large  bool         `yaml:"large,omitempty" json:"large,omitempty"`
	Count  int          `yaml:"count" json:"count"`
	Source PresetSource `yaml:"source" json:"source"`
}

// Meta — содержимое data/meta.yaml: от какого апстрим-коммита собраны
// данные, когда и какие пресеты получились (без доменов).
type Meta struct {
	Upstream    PresetSource `yaml:"upstream" json:"upstream"`
	GeneratedAt string       `yaml:"generated_at" json:"generated_at"`
	Generator   string       `yaml:"generator" json:"generator"`
	PresetCount int          `yaml:"preset_count" json:"preset_count"`
	Domains     int          `yaml:"domains" json:"domains"`
	Rublock     int          `yaml:"rublock_entries" json:"rublock_entries"`
	Presets     []PresetMeta `yaml:"presets" json:"presets"`
}

var (
	cacheMu sync.Mutex
	cache   = map[string]*Preset{} // slug -> разобранный пресет
	metaOne *Meta
	metaErr error
)

// DiskDir — каталог runtime-зеркала (рядом с cache.db/presets/): тяжёлые
// пресеты (rublock, ~38МБ) не вшиваются в бинарь и не коммитятся, а читаются
// с диска. Пусто — только embed. Задаётся один раз при старте.
var DiskDir string

// List возвращает все пресеты, отсортированные по Slug.
// meta.yaml в список не попадает.
//
// Список собирается по индексу из meta.yaml, поэтому rublock.yaml
// (1.7 млн доменов) не разбирается: его Domains остаётся nil, как и у
// остальных Large-пресетов. Домены большого пресета берутся через
// Get() или Domains(). Обычные пресеты материализуются целиком —
// суммарно это десятки килобайт.
func List() []Preset {
	m, ok := MetaData()
	if !ok || len(m.Presets) == 0 {
		return listSlow()
	}

	out := make([]Preset, 0, len(m.Presets))
	seen := map[string]bool{}
	for _, pm := range m.Presets {
		if pm.Slug == metaSlug {
			continue
		}
		seen[pm.Slug] = true
		p := Preset{
			Slug:   pm.Slug,
			Title:  pm.Title,
			Method: pm.Method,
			Large:  pm.Large,
			Source: pm.Source,
		}
		if !pm.Large {
			// Из индекса известно только число доменов; сами домены
			// дешёвые, добираем из файла пресета.
			if full, ok := Get(pm.Slug); ok {
				p.Domains = full.Domains
				p.StaticURL = full.StaticURL
			}
		}
		out = append(out, p)
	}
	// Ручные пресеты вне индекса (rublock-ip): добираем сканированием файлов.
	for _, name := range presetFiles() {
		if seen[name] {
			continue
		}
		if p, ok := Get(name); ok {
			if p.Large {
				p.Domains = nil
			}
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Slug < out[j].Slug })
	return out
}

// listSlow — обход всех файлов data/*.yaml. Используется только если
// meta.yaml без индекса (генератор допишет индекс при следующем прогоне).
func listSlow() []Preset {
	names := presetFiles()
	out := make([]Preset, 0, len(names))
	for _, name := range names {
		p, ok := Get(name)
		if !ok {
			continue
		}
		if p.Large {
			p.Domains = nil
		}
		out = append(out, p)
	}
	return out
}

// Domains возвращает домены пресета по слагу — в том числе Large,
// который List() намеренно не материализует.
func Domains(slug string) ([]string, bool) {
	p, ok := Get(slug)
	if !ok {
		return nil, false
	}
	return p.Domains, true
}

// Get возвращает пресет по слагу. Второе значение false, если пресета
// нет (в том числе если файл не распарсился).
//
// Файл читается и разбирается не более одного раза за процесс
// (rublock содержит >1.5 млн доменов, повторный разбор недопустим).
func Get(slug string) (Preset, bool) {
	slug = strings.ToLower(strings.TrimSpace(slug))
	if slug == "" {
		return Preset{}, false
	}

	cacheMu.Lock()
	if p, ok := cache[slug]; ok {
		cp := *p
		cacheMu.Unlock()
		return cp, true
	}
	cacheMu.Unlock()

	raw, err := dataFS.ReadFile(path.Join(dataDir, slug+yamlExt))
	if err != nil {
		// Нет в embed — пробуем диск зеркала (rublock и обновления).
		return getDisk(slug)
	}
	var p Preset
	if err := yaml.Unmarshal(raw, &p); err != nil {
		return Preset{}, false
	}
	if p.Slug == "" {
		p.Slug = slug
	}
	if len(p.Domains) == 0 {
		return Preset{}, false
	}

	cacheMu.Lock()
	cache[slug] = &p
	cacheMu.Unlock()

	cp := p
	return cp, true
}

// getDisk читает пресет с диска зеркала (DiskDir). Промах не кэшируется:
// файл может появиться позже (первое скачивание зеркала).
func getDisk(slug string) (Preset, bool) {
	if DiskDir == "" {
		return Preset{}, false
	}
	raw, err := os.ReadFile(filepath.Join(DiskDir, slug+yamlExt))
	if err != nil {
		return Preset{}, false
	}
	var p Preset
	if err := yaml.Unmarshal(raw, &p); err != nil || len(p.Domains) == 0 {
		return Preset{}, false
	}
	if p.Slug == "" {
		p.Slug = slug
	}
	cacheMu.Lock()
	cache[slug] = &p
	cacheMu.Unlock()
	cp := p
	return cp, true
}

// FlushAll сбрасывает кэш разобранных пресетов (после обновления зеркала).
func FlushAll() {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	cache = map[string]*Preset{}
	metaOne = nil
	metaErr = nil
}

// MetaData возвращает содержимое data/meta.yaml. Если файла нет,
// возвращается zero Meta и false.
func MetaData() (Meta, bool) {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	if metaErr != nil {
		return Meta{}, false
	}
	if metaOne != nil {
		return *metaOne, true
	}
	raw, err := dataFS.ReadFile(path.Join(dataDir, metaName))
	if err != nil {
		metaErr = err
		return Meta{}, false
	}
	var m Meta
	if err := yaml.Unmarshal(raw, &m); err != nil {
		metaErr = err
		return Meta{}, false
	}
	metaOne = &m
	return m, true
}

// Has сообщает, существует ли пресет с таким слагом, без разбора
// его доменов (достаточно наличия файла).
func Has(slug string) bool {
	slug = strings.ToLower(strings.TrimSpace(slug))
	if slug == "" || slug == metaSlug {
		return false
	}
	if _, err := dataFS.ReadFile(path.Join(dataDir, slug+yamlExt)); err != nil {
		return false
	}
	return true
}

// presetFiles — слаги всех файлов data/*.yaml, кроме meta.yaml.
func presetFiles() []string {
	entries, err := fs.ReadDir(dataFS, dataDir)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), yamlExt) {
			continue
		}
		slug := strings.TrimSuffix(e.Name(), yamlExt)
		if slug == metaSlug {
			continue
		}
		out = append(out, slug)
	}
	sort.Strings(out)
	return out
}

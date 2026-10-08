// Command presets-import пересобирает internal/presets/data/*.yaml
// из апстрим-источников:
//
//   - v2fly/domain-list-community (закреплённый коммит, файлы data/*)
//   - antifilter.download/list/domains.lst (плоский блоклист РКН)
//
// Парсинг и правила конвертации живут в internal/presets/import.go,
// здесь только скачивание, запись файлов и отчёт.
//
// Запуск:
//
//	go run ./scripts/presets-import
//	go run ./scripts/presets-import -out internal/presets/data -rublock=false
package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/Kfaraon/mikrotik-route-sync/internal/presets"
)

const (
	// upstreamRepo — репозиторий со списками доменов.
	upstreamRepo = "v2fly/domain-list-community"

	// upstreamCommit — закреплённый коммит (v2fly master на момент
	// генерации). Меняется только осознанно: с ним связаны все
	// сгенерированные файлы и meta.yaml.
	upstreamCommit = "e84921279c9142434b43cb84ec6c17f987e025df"

	// upstreamDate — дата коммита в RFC3339 (из git show).
	upstreamDate = "2026-10-08T09:30:42Z"

	// rublockURL — плоский список заблокированных в РФ доменов.
	rublockURL = "https://antifilter.download/list/domains.lst"
)

var (
	outDir    = flag.String("out", filepath.Join("internal", "presets", "data"), "каталог для сгенерированных YAML")
	rublock   = flag.Bool("rublock", true, "скачивать antifilter.download и собирать пресет rublock")
	rublockM  = flag.Int("rublock-max", 0, "кап для rublock (0 = без капа, полный список)")
	timeout   = flag.Duration("timeout", 10*time.Minute, "таймаут скачивания")
	userAgent = flag.String("user-agent", "mikrotik-route-sync/presets-import", "User-Agent для запросов")
)

func main() {
	flag.Parse()
	logf("upstream: %s@%s (%s)", upstreamRepo, short(upstreamCommit), upstreamDate)

	files, err := fetchUpstream(*timeout, *userAgent)
	if err != nil {
		fatal("скачать %s: %v", upstreamRepo, err)
	}
	logf("распаковано %d файлов из data/*", len(files))

	src := presets.PresetSource{
		Repo:   presets.SourceV2Fly,
		Commit: upstreamCommit,
		Date:   upstreamDate,
	}

	list, st := presets.ParseUpstream(files, src)
	logStats("v2fly", st)
	for _, slug := range st.MissingLists {
		logf("WARN: апстрим-список %q отсутствует — пресет пропущен", slug)
	}
	for _, inc := range st.MissingIncludes {
		logf("WARN: include:%s — файла нет в data/*, пропущен", inc)
	}
	for _, fb := range st.ShowcaseFallback {
		logf("WARN: витринный домен не найден (%s), взят фолбэк", fb)
	}

	out := append([]presets.Preset(nil), list...)
	rublockEntries := 0

	if *rublock {
		body, err := fetch(rublockURL, *timeout, *userAgent)
		if err != nil {
			// Блоклист РКН — внешний и не пинован: его недоступность
			// не должна ломать генерацию основных пресетов.
			logf("WARN: не удалось скачать %s (%v) — рублок пропущен", rublockURL, err)
		} else {
			rsrc := presets.PresetSource{
				Repo:   presets.SourceRublock,
				Commit: "unpinned",
				Date:   time.Now().UTC().Format(time.RFC3339),
			}
			p, rst := presets.ParseRublock(string(body), rsrc, *rublockM)
			logStats(presets.RublockSlug, rst)
			rublockEntries = len(p.Domains)
			if rublockEntries > presets.MaxDomains {
				logf("WARN: %s содержит %d доменов (large) — файл будет большой, ~%s",
					p.Slug, rublockEntries, estimateYAMLSize(rublockEntries))
			}
			out = append(out, p)
		}
	}

	if len(out) == 0 {
		fatal("ни один пресет не собран — генерация прервана")
	}

	meta := presets.Meta{
		Upstream:    src,
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Generator:   "scripts/presets-import",
		PresetCount: len(out),
		Domains:     totalDomains(out),
		Rublock:     rublockEntries,
		Presets:     indexOf(out),
	}

	if err := write(out, meta); err != nil {
		fatal("записать %s: %v", *outDir, err)
	}

	logf("готово: %d пресетов, %d обычных + %d в rublock, %s",
		len(out), totalDomains(out)-rublockEntries, rublockEntries, *outDir)
	for _, p := range out {
		mark := " "
		if len(p.Domains) >= presets.MaxDomains {
			mark = "!"
		}
		logf("  %s %-26s %4d доменов  %s", mark, p.Slug, len(p.Domains), p.Domains[0])
	}
}

// write раскладывает пресеты по файлам и чистит устаревшие.
func write(list []presets.Preset, meta presets.Meta) error {
	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		return err
	}

	keep := map[string]bool{"meta.yaml": true}
	for _, p := range list {
		raw, err := marshalPreset(p)
		if err != nil {
			return fmt.Errorf("%s: %w", p.Slug, err)
		}
		name := p.Slug + ".yaml"
		keep[name] = true
		if err := writeFile(filepath.Join(*outDir, name), raw); err != nil {
			return err
		}
	}

	raw, err := marshalMeta(meta)
	if err != nil {
		return err
	}
	if err := writeFile(filepath.Join(*outDir, "meta.yaml"), raw); err != nil {
		return err
	}

	// Чистим файлы пресетов, которых больше нет в каталоге: иначе
	// embed-индекс в internal/presets будет содержать мусор.
	entries, err := os.ReadDir(*outDir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".yaml") || keep[name] {
			continue
		}
		if err := os.Remove(filepath.Join(*outDir, name)); err != nil {
			return err
		}
		logf("удалён устаревший файл %s", name)
	}
	return nil
}

// marshalPreset сериализует пресет с 2-шаговым отступом.
func marshalPreset(p presets.Preset) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString("# Сгенерировано scripts/presets-import — не редактировать вручную.\n")
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(p); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func marshalMeta(m presets.Meta) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString("# Сгенерировано scripts/presets-import — не редактировать вручную.\n")
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(m); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func writeFile(name string, data []byte) error {
	tmp := name + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, name)
}

// fetchUpstream скачивает tarball закреплённого коммита и достаёт data/*.
func fetchUpstream(timeout time.Duration, ua string) (map[string]string, error) {
	url := fmt.Sprintf("https://codeload.github.com/%s/tar.gz/%s", upstreamRepo, upstreamCommit)
	body, err := fetch(url, timeout, ua)
	if err != nil {
		return nil, err
	}
	return extractData(body)
}

// fetch — GET с редиректами, лимитом ответа и таймаутом.
func fetch(url string, timeout time.Duration, ua string) ([]byte, error) {
	const maxBytes = 512 << 20

	client := &http.Client{Timeout: timeout}
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", ua)

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %s", resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes))
	if err != nil {
		return nil, err
	}
	return data, nil
}

// extractData распаковывает gz-tar и читает только data/* на верхнем
// уровне вложенности (в tarball в корне лежит <repo>-<sha>/data/...).
func extractData(tgz []byte) (map[string]string, error) {
	zr, err := gzip.NewReader(bytes.NewReader(tgz))
	if err != nil {
		return nil, fmt.Errorf("gzip: %w", err)
	}
	defer func() { _ = zr.Close() }()

	out := map[string]string{}
	tr := tar.NewReader(zr)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("tar: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		parts := strings.Split(path.Clean(hdr.Name), "/")
		if len(parts) != 3 || parts[1] != "data" {
			continue
		}
		slug := parts[2]
		if slug == "" || strings.Contains(slug, ".") {
			continue
		}
		body, err := io.ReadAll(io.LimitReader(tr, 16<<20))
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", hdr.Name, err)
		}
		out[slug] = string(body)
	}
	if len(out) == 0 {
		return nil, errors.New("в tarball не найдено ни одного файла data/*")
	}
	return out, nil
}

func totalDomains(list []presets.Preset) int {
	n := 0
	for _, p := range list {
		n += len(p.Domains)
	}
	return n
}

// indexOf строит индекс пресетов для meta.yaml: всё, кроме доменов.
// List() в internal/presets читает его вместо разбора всех файлов —
// иначе rublock.yaml (1.7 млн доменов) пришлось бы парсить всегда.
func indexOf(list []presets.Preset) []presets.PresetMeta {
	out := make([]presets.PresetMeta, 0, len(list))
	for _, p := range list {
		out = append(out, presets.PresetMeta{
			Slug:   p.Slug,
			Title:  p.Title,
			Method: p.Method,
			Large:  p.Large,
			Count:  len(p.Domains),
			Source: p.Source,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Slug < out[j].Slug })
	return out
}

// estimateYAMLSize — грубая оценка размера YAML по средней длине
// домена (для предупреждения в stdout).
func estimateYAMLSize(n int) string {
	const avg = 24 // "- <домен>\n"
	return humanBytes(int64(n * avg))
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGT"[exp])
}

func short(commit string) string {
	if len(commit) > 12 {
		return commit[:12]
	}
	return commit
}

func logStats(label string, st presets.Stats) {
	logf("%s: full=%d domain=%d keyword=%d(отброшено) regexp=%d(отброшено) "+
		"безточечных=%d @ads/@telemetry=%d(отброшено) include=%d дуп-include=%d циклов=%d "+
		"невалидных=%d повторов=%d срезано=%d",
		label, st.Full, st.Domain, st.Keyword, st.Regexp, st.DotlessDomain, st.Ads,
		st.Includes, st.IncludeDup, st.IncludeCycles, st.Invalid, st.Duplicates, st.Truncated)
}

func logf(format string, args ...any) {
	_, _ = fmt.Fprintf(os.Stdout, "presets-import: "+format+"\n", args...)
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "presets-import: "+format+"\n", args...)
	os.Exit(1)
}

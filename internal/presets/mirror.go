// Package presets — продолжение: runtime-зеркало тяжёлых списков.
//
// Лёгкие пресеты вшиты в бинарь (embed, data/*.yaml коммитятся).
// Тяжёлые (rublock, ~38МБ) в репозиторий не коммитятся и качаются сюда:
// <mirrorDir>/rublock.yaml + mirror.json. Читаются через DiskDir-оверлей
// (см. presets.go). Семантика снимка: зеркало никого не трогает само —
// домены копируются в сервис только явным применением пресета.
package presets

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// AntifilterURL — плоский список заблокированных доменов (домен на строку).
const AntifilterURL = "https://antifilter.download/list/domains.lst"

// maxMirrorBytes — предел скачивания зеркала (защита от мусора).
const maxMirrorBytes = 100 << 20

// mirrorFetchTimeout — таймаут скачивания.
const mirrorFetchTimeout = 120 * time.Second

// minRublockLines — меньше строк = битый ответ, старое не трогаем.
const minRublockLines = 1000

// MirrorMeta — состояние зеркала (mirror.json рядом с rublock.yaml).
type MirrorMeta struct {
	UpdatedAt string `yaml:"updated_at" json:"updated_at"`
	Source    string `yaml:"source" json:"source"`
	Entries   int    `yaml:"entries" json:"entries"`
}

// MirrorStatus — итог RefreshMirror.
type MirrorStatus struct {
	Entries   int    `yaml:"entries" json:"entries"`
	UpdatedAt string `yaml:"updated_at" json:"updated_at"`
}

// MirrorDir — каталог runtime-зеркала рядом с cache.db.
// Единая точка расчёта (main и web): два разных вычисления разъехались бы.
func MirrorDir(cachePath string) string {
	if cachePath == "" {
		return ""
	}
	if dir := filepath.Dir(cachePath); dir != "" && dir != "." {
		return filepath.Join(dir, "presets")
	}
	return ""
}

// RefreshMirror качает antifilter-список, конвертирует через ParseRublock
// и кладёт rublock.yaml + mirror.json в dir. При любой проблеме старое
// не трогается (ошибка возвращается, файл не перезаписывается).
func RefreshMirror(ctx context.Context, dir string) (MirrorStatus, error) {
	var st MirrorStatus
	body, err := fetchMirror(ctx, AntifilterURL)
	if err != nil {
		return st, err
	}
	p, _ := ParseRublock(string(body), PresetSource{Repo: "antifilter.download", Commit: "", Date: time.Now().UTC().Format(time.RFC3339)}, 0)
	if len(p.Domains) < minRublockLines {
		return st, fmt.Errorf("mirror suspiciously small: %d domains, keeping old", len(p.Domains))
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return st, fmt.Errorf("mkdir mirror: %w", err)
	}
	raw, err := yaml.Marshal(p)
	if err != nil {
		return st, fmt.Errorf("marshal mirror: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".rublock-*.tmp")
	if err != nil {
		return st, fmt.Errorf("tmp mirror: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return st, fmt.Errorf("write mirror: %w", err)
	}
	_ = tmp.Close()
	if err := os.Rename(tmpName, filepath.Join(dir, RublockSlug+yamlExt)); err != nil {
		_ = os.Remove(tmpName)
		return st, fmt.Errorf("rename mirror: %w", err)
	}
	meta := MirrorMeta{
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
		Source:    AntifilterURL,
		Entries:   len(p.Domains),
	}
	mraw, err := yaml.Marshal(meta)
	if err != nil {
		return st, fmt.Errorf("marshal mirror meta: %w", err)
	}
	// Атомарно тем же приёмом: иначе читатель видит обрезанный mirror.json,
	// а версия в нём (ключ blockCheckCache) рассинхронизируется с rublock.yaml.
	mtmp, err := os.CreateTemp(dir, ".mirror-*.tmp")
	if err != nil {
		return st, fmt.Errorf("tmp mirror meta: %w", err)
	}
	if _, err := mtmp.Write(mraw); err != nil {
		_ = mtmp.Close()
		_ = os.Remove(mtmp.Name())
		return st, fmt.Errorf("write mirror meta: %w", err)
	}
	_ = mtmp.Close()
	if err := os.Rename(mtmp.Name(), filepath.Join(dir, "mirror.json")); err != nil {
		_ = os.Remove(mtmp.Name())
		return st, fmt.Errorf("rename mirror meta: %w", err)
	}
	FlushAll()
	FlushBlockCheckCache()
	st.Entries = len(p.Domains)
	st.UpdatedAt = meta.UpdatedAt
	return st, nil
}

// MirrorInfo читает состояние зеркала (nil если не скачивалось).
func MirrorInfo(dir string) *MirrorMeta {
	raw, err := os.ReadFile(filepath.Join(dir, "mirror.json"))
	if err != nil {
		return nil
	}
	var m MirrorMeta
	if err := yaml.Unmarshal(raw, &m); err != nil {
		return nil
	}
	return &m
}

func fetchMirror(ctx context.Context, url string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, mirrorFetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "mikrotik-route-sync presets-mirror")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch mirror: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch mirror: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxMirrorBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read mirror: %w", err)
	}
	if int64(len(body)) > maxMirrorBytes {
		return nil, fmt.Errorf("mirror too large: >%d bytes", maxMirrorBytes)
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return nil, fmt.Errorf("mirror empty response")
	}
	return body, nil
}

// UpstreamTarball — полный снапшот v2fly/domain-list-community (~1МБ).
// Пин ветки master: детерминированный коммит фиксирует генератор,
// зеркало хранит дату скачивания (свежесть важнее пина в рантайме).
const UpstreamTarball = "https://codeload.github.com/v2fly/domain-list-community/tar.gz/refs/heads/master"

// UpstreamMeta — состояние v2fly-зеркала (upstream.json рядом).
type UpstreamMeta struct {
	UpdatedAt string `yaml:"updated_at" json:"updated_at"`
	Source    string `yaml:"source" json:"source"`
	Files     int    `yaml:"files" json:"files"`
	Size      int64  `yaml:"size" json:"size"`
}

// maxUpstreamBytes — tarball ~1МБ, запас с головой.
const maxUpstreamBytes = 32 << 20

// RefreshUpstream качает tarball v2fly целиком в dir/upstream/ (только data/*)
// и обновляет upstream.json. Валидация: минимум 100 файлов data/*.
func RefreshUpstream(ctx context.Context, dir string) (UpstreamMeta, error) {
	var st UpstreamMeta
	body, err := fetchMirror(ctx, UpstreamTarball)
	if err != nil {
		return st, err
	}
	if int64(len(body)) > maxUpstreamBytes {
		return st, fmt.Errorf("upstream too large: %d bytes", len(body))
	}
	files, err := extractDataFiles(body)
	if err != nil {
		return st, err
	}
	if len(files) < 100 {
		return st, fmt.Errorf("upstream suspiciously small: %d files, keeping old", len(files))
	}
	upDir := filepath.Join(dir, "upstream")
	if err := os.MkdirAll(upDir, 0o755); err != nil {
		return st, fmt.Errorf("mkdir upstream: %w", err)
	}
	// Чистим исчезнувшие upstream: иначе индекс вечно держит stale-слаги.
	if err := syncUpstreamDir(upDir, files); err != nil {
		return st, err
	}
	for name, content := range files {
		tmp, err := os.CreateTemp(upDir, ".upstream-*.tmp")
		if err != nil {
			return st, fmt.Errorf("tmp upstream: %w", err)
		}
		if _, err := tmp.WriteString(content); err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
			return st, fmt.Errorf("write upstream: %w", err)
		}
		_ = tmp.Close()
		if err := os.Rename(tmp.Name(), filepath.Join(upDir, filepath.Base(name))); err != nil {
			_ = os.Remove(tmp.Name())
			return st, fmt.Errorf("rename upstream: %w", err)
		}
	}
	var total int64
	for _, c := range files {
		total += int64(len(c))
	}
	meta := UpstreamMeta{
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
		Source:    UpstreamTarball,
		Files:     len(files),
		Size:      total,
	}
	if mraw, err := yaml.Marshal(meta); err == nil {
		_ = os.WriteFile(filepath.Join(dir, "upstream.json"), mraw, 0o644)
	}
	// Индекс сразу здесь (а не только в хендлере): прямой вызов
	// RefreshUpstream не должен оставлять его несвежим.
	if err := RebuildUpstreamIndex(dir); err != nil {
		return st, err
	}
	return meta, nil
}

// syncUpstreamDir удаляет из upDir файлы, которых нет в свежем манифесте.
func syncUpstreamDir(upDir string, files map[string]string) error {
	keep := make(map[string]bool, len(files))
	for name := range files {
		keep[filepath.Base(name)] = true
	}
	entries, err := os.ReadDir(upDir)
	if err != nil {
		return fmt.Errorf("read upstream dir: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.HasPrefix(e.Name(), ".upstream-") {
			continue
		}
		if !keep[e.Name()] {
			if err := os.Remove(filepath.Join(upDir, e.Name())); err != nil {
				return fmt.Errorf("remove stale upstream %s: %w", e.Name(), err)
			}
		}
	}
	return nil
}

// extractDataFiles достаёт файлы data/* из tar.gz (имена без каталогов).
func extractDataFiles(body []byte) (map[string]string, error) {
	gz, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("gunzip upstream: %w", err)
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)
	files := map[string]string{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read tar: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		// data/<name> на любой глубине префикса tarball.
		idx := strings.Index(hdr.Name, "/data/")
		if idx < 0 {
			continue
		}
		name := hdr.Name[idx+len("/data/"):]
		// Явно отклоняем опасное: filepath.Join/Filepath.Base ниже спасают,
		// но молчаливая фильтрация скрыла бы атаку в логах.
		if name == "" || name == "." || name == ".." || strings.Contains(name, "/") {
			continue
		}
		content, err := io.ReadAll(io.LimitReader(tr, maxUpstreamBytes+1))
		if err != nil {
			return nil, fmt.Errorf("read entry: %w", err)
		}
		files[name] = string(content)
	}
	return files, nil
}

// --- In-memory индекс полного v2fly-зеркала ("домен → пресеты") ---

var (
	ixMu sync.Mutex
	ix   *Index
)

// RebuildUpstreamIndex строит инвертированный индекс по файлам dir/upstream/.
// Вызывать при старте (если зеркало скачано) и после каждого обновления.
func RebuildUpstreamIndex(dir string) error {
	upDir := filepath.Join(dir, "upstream")
	entries, err := os.ReadDir(upDir)
	if err != nil {
		return fmt.Errorf("read upstream: %w", err)
	}
	files := map[string]string{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(upDir, e.Name()))
		if err != nil {
			continue
		}
		files[e.Name()] = string(raw)
	}
	if len(files) == 0 {
		return fmt.Errorf("upstream mirror empty: %s", upDir)
	}
	built := BuildIndex(files)
	ixMu.Lock()
	ix = built
	ixMu.Unlock()
	return nil
}

// MatchUpstream ищет слаги пресетов по домену (точное + apex).
// Без индекса (зеркало не скачано) — nil.
func MatchUpstream(domain string) []string {
	ixMu.Lock()
	defer ixMu.Unlock()
	if ix == nil {
		return nil
	}
	return ix.Match(domain)
}

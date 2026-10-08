// Package presets — продолжение: runtime-зеркало тяжёлых списков.
//
// Лёгкие пресеты вшиты в бинарь (embed, data/*.yaml коммитятся).
// Тяжёлые (rublock, ~38МБ) в репозиторий не коммитятся и качаются сюда:
// <mirrorDir>/rublock.yaml + mirror.json. Читаются через DiskDir-оверлей
// (см. presets.go). Семантика снимка: зеркало никого не трогает само —
// домены копируются в сервис только явным применением пресета.
package presets

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
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
	if err == nil {
		_ = os.WriteFile(filepath.Join(dir, "mirror.json"), mraw, 0o644)
	}
	FlushAll()
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

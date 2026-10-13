package presets

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// makeUpstreamTarball собирает минимальный tar.gz с data/*-файлами.
func makeUpstreamTarball(t *testing.T, count int) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for i := 0; i < count; i++ {
		name := fmt.Sprintf("v2fly-master/data/list%d", i)
		body := fmt.Sprintf("domain%d.example.com\n", i)
		hdr := &tar.Header{
			Name:     name,
			Mode:     0o644,
			Size:     int64(len(body)),
			Typeflag: tar.TypeReg,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// withMirrorSources подменяет URL-швы на httptest-серверы, возвращая cleanup.
func withMirrorSources(t *testing.T, rublock, upstream []byte) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/rublock", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(rublock)
	})
	mux.HandleFunc("/upstream", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(upstream)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	oldMirror, oldUp := mirrorSourceURL, upstreamSourceURL
	mirrorSourceURL = srv.URL + "/rublock"
	upstreamSourceURL = srv.URL + "/upstream"
	t.Cleanup(func() {
		mirrorSourceURL = oldMirror
		upstreamSourceURL = oldUp
	})
}

// TestAtomicWriteFile — запись не оставляет временных файлов и создаёт target.
func TestAtomicWriteFile(t *testing.T) {
	dir := t.TempDir()
	if err := atomicWriteFile(dir, "upstream.json", []byte("hello"), 0o644); err != nil {
		t.Fatalf("atomicWriteFile: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "upstream.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello" {
		t.Fatalf("получено %q, ожидалось hello", got)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".upstream.json.") {
			t.Fatalf("остался временный файл: %s", e.Name())
		}
	}
}

// TestAtomicWriteFileOverwrite — повторная запись заменяет содержимое.
func TestAtomicWriteFileOverwrite(t *testing.T) {
	dir := t.TempDir()
	for _, v := range []string{"one", "two", "three"} {
		if err := atomicWriteFile(dir, "x.json", []byte(v), 0o600); err != nil {
			t.Fatalf("atomicWriteFile(%q): %v", v, err)
		}
	}
	got, _ := os.ReadFile(filepath.Join(dir, "x.json"))
	if string(got) != "three" {
		t.Fatalf("получено %q, ожидалось three", got)
	}
}

// TestRefreshMirrorAtomicMeta — после RefreshMirror mirror.json валиден и
// соответствует rublock.yaml.
func TestRefreshMirrorAtomicMeta(t *testing.T) {
	// 1200 доменов: > minRublockLines.
	var lines strings.Builder
	for i := 0; i < 1200; i++ {
		fmt.Fprintf(&lines, "d%d.example.com\n", i)
	}
	withMirrorSources(t, []byte(lines.String()), nil)

	dir := t.TempDir()
	st, err := RefreshMirror(context.Background(), dir)
	if err != nil {
		t.Fatalf("RefreshMirror: %v", err)
	}
	if st.Entries != 1200 {
		t.Fatalf("entries=%d, ожидалось 1200", st.Entries)
	}
	if info := MirrorInfo(dir); info == nil || info.Entries != 1200 {
		t.Fatalf("MirrorInfo=%+v, ожидалось 1200", info)
	}
}

// TestRefreshUpstreamAtomicMeta — после RefreshUpstream upstream.json
// читается и не остаётся временных файлов.
func TestRefreshUpstreamAtomicMeta(t *testing.T) {
	tarball := makeUpstreamTarball(t, 150)
	withMirrorSources(t, nil, tarball)

	dir := t.TempDir()
	st, err := RefreshUpstream(context.Background(), dir)
	if err != nil {
		t.Fatalf("RefreshUpstream: %v", err)
	}
	if st.Files != 150 {
		t.Fatalf("files=%d, ожидалось 150", st.Files)
	}
	info := UpstreamInfo(dir)
	if info == nil || info.Files != 150 {
		t.Fatalf("UpstreamInfo=%+v, ожидалось files=150", info)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".upstream.json.") {
			t.Fatalf("остался временный meta-файл: %s", e.Name())
		}
	}
}

// TestRefreshSerialized — параллельные RefreshMirror/RefreshUpstream не
// пересекаются: последовательность гарантирует refreshWriteMu, а финальные
// метаданные и каталог остаются консистентными.
func TestRefreshSerialized(t *testing.T) {
	var lines strings.Builder
	for i := 0; i < 1200; i++ {
		fmt.Fprintf(&lines, "d%d.example.com\n", i)
	}
	tarball := makeUpstreamTarball(t, 150)
	withMirrorSources(t, []byte(lines.String()), tarball)

	dir := t.TempDir()

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := RefreshMirror(context.Background(), dir); err != nil {
				t.Errorf("RefreshMirror: %v", err)
			}
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := RefreshUpstream(context.Background(), dir); err != nil {
				t.Errorf("RefreshUpstream: %v", err)
			}
		}()
	}
	wg.Wait()

	if info := MirrorInfo(dir); info == nil || info.Entries != 1200 {
		t.Fatalf("после гонки MirrorInfo=%+v", info)
	}
	if info := UpstreamInfo(dir); info == nil || info.Files != 150 {
		t.Fatalf("после гонки UpstreamInfo=%+v", info)
	}
	// upstream-каталог должен содержать ровно ожидаемые файлы (stale удалены).
	files := LoadUpstreamFiles(dir)
	if len(files) != 150 {
		t.Fatalf("upstream-файлов %d, ожидалось 150", len(files))
	}
}

// TestRefreshReaderWriterParallel — читатели (LoadUpstreamFiles,
// RebuildUpstreamIndex) не дедлочат с писателем RefreshUpstream и не видят
// частичного каталога. Регрессия на общий refreshWriteMu.
func TestRefreshReaderWriterParallel(t *testing.T) {
	tarball := makeUpstreamTarball(t, 150)
	withMirrorSources(t, nil, tarball)

	dir := t.TempDir()
	// Первичное наполнение.
	if _, err := RefreshUpstream(context.Background(), dir); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
			}
			_ = LoadUpstreamFiles(dir)
			_ = RebuildUpstreamIndex(dir)
		}
	}()
	for i := 0; i < 5; i++ {
		if _, err := RefreshUpstream(context.Background(), dir); err != nil {
			t.Errorf("RefreshUpstream: %v", err)
		}
	}
	close(done)
	wg.Wait()

	if files := LoadUpstreamFiles(dir); len(files) != 150 {
		t.Fatalf("upstream-файлов %d, ожидалось 150", len(files))
	}
}

// TestRefreshMirrorKeepsOldOnSmall — битый (маленький) ответ не трогает
// существующее зеркало.
func TestRefreshMirrorKeepsOldOnSmall(t *testing.T) {
	dir := t.TempDir()
	// Сначала валидное зеркало.
	var lines strings.Builder
	for i := 0; i < 1200; i++ {
		fmt.Fprintf(&lines, "d%d.example.com\n", i)
	}
	withMirrorSources(t, []byte(lines.String()), nil)
	if _, err := RefreshMirror(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	// Теперь источник отдаёт 5 доменов.
	withMirrorSources(t, []byte("a.com\nb.com\nc.com\nd.com\ne.com\n"), nil)
	if _, err := RefreshMirror(context.Background(), dir); err == nil {
		t.Fatal("ожидалась ошибка на маленьком ответе")
	}
	if info := MirrorInfo(dir); info == nil || info.Entries != 1200 {
		t.Fatalf("старое зеркало затёрто: %+v", info)
	}
}

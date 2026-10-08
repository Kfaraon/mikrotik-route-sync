package logging

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeLogLines(t *testing.T, lines []string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "app.log")
	var sb strings.Builder
	for _, l := range lines {
		sb.WriteString(l + "\n")
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func entryLevel(e map[string]any) string {
	v, _ := e["level"].(string)
	return v
}

// TestTailLogFileFiltered — фильтр применяется при чтении с конца:
// старые подходящие записи не теряются, порядок — как в файле.
func TestTailLogFileFiltered(t *testing.T) {
	var lines []string
	for i := 0; i < 200; i++ {
		lvl := "info"
		if i%50 == 0 {
			lvl = "error"
		}
		lines = append(lines, `{"level":"`+lvl+`","msg":"m`+strings.Repeat("0", i%7)+`"}`)
	}
	lines = append(lines, "not json at all", "", `{"level":"error","msg":"last"}`)
	path := writeLogLines(t, lines)

	got, truncated, err := TailLogFileFiltered(path, 100, func(e map[string]any) bool {
		return entryLevel(e) == "error"
	})
	if err != nil {
		t.Fatalf("tail: %v", err)
	}
	if truncated {
		t.Fatal("small file must not be truncated")
	}
	// error-строки: i=0,50,100,150 + last = 5 штук.
	if len(got) != 5 {
		t.Fatalf("want 5 errors, got %d", len(got))
	}
	if entryLevel(got[0]) != "error" || got[len(got)-1]["msg"] != "last" {
		t.Fatalf("order/content wrong: %+v", got)
	}

	// Лимит режет с новых: последние 2 error.
	got, _, err = TailLogFileFiltered(path, 2, func(e map[string]any) bool {
		return entryLevel(e) == "error"
	})
	if err != nil || len(got) != 2 {
		t.Fatalf("limit: %+v %v", got, err)
	}
	if got[1]["msg"] != "last" {
		t.Fatalf("want newest last: %+v", got)
	}

	// Без фильтра — обычный хвост.
	got, _, err = TailLogFileFiltered(path, 3, nil)
	if err != nil || len(got) != 3 {
		t.Fatalf("no filter: %+v %v", got, err)
	}
}

// TestTailLogFileTruncated — файл больше maxTailScan: сканируется только
// хвост, возвращается truncated=true вместо чтения всего файла в память.
func TestTailLogFileTruncated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "big.log")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	big := strings.Repeat("z", 1000)
	// ~9 МБ строк без совпадений + одна подходящая в самом конце.
	for i := 0; i < 9000; i++ {
		if _, err := f.WriteString(`{"level":"info","pad":"` + big + `"}` + "\n"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.WriteString(`{"level":"error","msg":"tail-hit"}` + "\n"); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	got, truncated, err := TailLogFileFiltered(path, 10, func(e map[string]any) bool {
		return entryLevel(e) == "error"
	})
	if err != nil {
		t.Fatalf("tail: %v", err)
	}
	if !truncated {
		t.Fatal("want truncated=true past maxTailScan")
	}
	if len(got) != 1 || got[0]["msg"] != "tail-hit" {
		t.Fatalf("want the tail match: %+v", got)
	}
}

// TestTailLogFileMissing — нет файла: пусто без ошибки.
func TestTailLogFileMissing(t *testing.T) {
	got, err := TailLogFile(filepath.Join(t.TempDir(), "nope.log"), 10)
	if err != nil || len(got) != 0 {
		t.Fatalf("missing: %+v %v", got, err)
	}
}

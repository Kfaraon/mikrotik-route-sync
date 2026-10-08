package presets

// Тесты инвертированного индекса «домен → пресеты» (index.go).
// Полностью офлайн: только инлайн-фикстура, без сети и data/.

import (
	"slices"
	"testing"
)

// indexFixture — четыре апстрим-файла:
//
//   - youtube: точные домены, повтор той же строки (дубль),
//     TLD-запись без точки, @ads-запись, keyword:/regexp:,
//     include:yt-common;
//   - yt-common: include:youtube — цикл youtube <-> yt-common;
//   - common:   apex и поддомен shared.com (многоуровневый apex);
//   - other:    пересечение с youtube — несколько слагов на домен.
func indexFixture() map[string]string {
	return map[string]string{
		"youtube": `# youtube
domain:youtube.com
domain:www.youtube.com
youtube.com
youtube
domain:ads.tracker.example @ads
keyword:tracker.invalid
regexp:^spam\.invalid$
include:yt-common
`,
		"yt-common": `domain:video.example.com
include:youtube # цикл назад
`,
		"common": `domain:shared.com
domain:sub.shared.com
domain:cdn.shared.com
`,
		"other": `domain:www.shared.com
domain:www.youtube.com
`,
	}
}

func TestIndexMatch(t *testing.T) {
	ix := BuildIndex(indexFixture())

	cases := []struct {
		name  string
		query string
		want  []string // nil = ожидаем ровно nil
	}{
		// --- точное совпадение и нормализация ---
		{"exact", "youtube.com", []string{"youtube", "yt-common"}},
		{"lowercase и точка в конце", "YOUTUBE.COM.", []string{"youtube", "yt-common"}},
		{"пробелы вокруг", "  www.youtube.com  ", []string{"other", "youtube", "yt-common"}},
		// Домен из include:yt-common проиндексирован и под yt-common.
		{"из include", "video.example.com", []string{"youtube", "yt-common"}},

		// --- apex ---
		{"apex один уровень", "v4.youtube.com", []string{"youtube", "yt-common"}},
		{"apex два уровня", "a.b.shared.com", []string{"common"}},
		// Точные обязаны идти первыми: apex дал бы иначе [common, other].
		{"точные перед apex", "www.shared.com", []string{"other", "common"}},
		// Один слаг на двух apex-уровнях не задваивается.
		{"дедуп между уровнями apex", "x.sub.shared.com", []string{"common"}},
		// Точные и apex пересекаются — дедуп, а не конкатенация.
		{"дедуп точных и apex", "www.youtube.com", []string{"other", "youtube", "yt-common"}},

		// --- пропуски ---
		{"TLD без точки", "youtube", nil}, // безточечная запись не индексируется
		{"@ads-запись", "ads.tracker.example", nil},
		{"keyword:", "tracker.invalid", nil},
		{"нет совпадений", "example.org", nil},
		// Apex поднимается максимум на два уровня: youtube.com третий.
		{"выше двух уровней", "a.b.c.youtube.com", nil},

		// --- пустой ввод ---
		{"пусто", "", nil},
		{"пробелы", "   ", nil},
		{"точка", ".", nil},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ix.Match(c.query)
			if c.want == nil {
				if got != nil {
					t.Fatalf("Match(%q) = %v, ожидалось nil", c.query, got)
				}
				return
			}
			if !slices.Equal(got, c.want) {
				t.Errorf("Match(%q) = %v, ожидалось %v", c.query, got, c.want)
			}
		})
	}
}

// TestIndexKeys — точный состав ключей: @ads, keyword:, безточечная
// TLD-запись и дубль строки не породили лишних доменов.
func TestIndexKeys(t *testing.T) {
	ix := BuildIndex(indexFixture())

	got := make([]string, 0, len(ix.byDomain))
	for d := range ix.byDomain {
		got = append(got, d)
	}
	slices.Sort(got)

	want := []string{
		"cdn.shared.com",
		"shared.com",
		"sub.shared.com",
		"video.example.com",
		"www.shared.com",
		"www.youtube.com",
		"youtube.com",
	}
	if !slices.Equal(got, want) {
		t.Errorf("ключи индекса = %v, ожидалось %v", got, want)
	}
}

// TestIndexEmpty — пустой вход даёт пустой индекс, nil-указатель безопасен.
func TestIndexEmpty(t *testing.T) {
	for name, ix := range map[string]*Index{
		"nil files":    BuildIndex(nil),
		"пустая карта": BuildIndex(map[string]string{}),
	} {
		if ix == nil {
			t.Fatalf("%s: BuildIndex вернул nil", name)
		}
		if got := ix.Match("youtube.com"); got != nil {
			t.Errorf("%s: Match(youtube.com) = %v, ожидалось nil", name, got)
		}
	}

	var nilIx *Index
	if got := nilIx.Match("youtube.com"); got != nil {
		t.Errorf("nil *Index: Match = %v, ожидалось nil", got)
	}
}

// TestIndexDeterministic — порядок слагов не зависит от рандомизации
// итерации по map: несколько сборок обязаны отвечать одинаково.
func TestIndexDeterministic(t *testing.T) {
	queries := []string{
		"youtube.com", "www.youtube.com", "video.example.com",
		"shared.com", "www.shared.com", "x.sub.shared.com",
	}
	want := make([][]string, len(queries))
	base := BuildIndex(indexFixture())
	for i, q := range queries {
		want[i] = base.Match(q)
	}

	for i := 0; i < 10; i++ {
		ix := BuildIndex(indexFixture())
		for j, q := range queries {
			if got := ix.Match(q); !slices.Equal(got, want[j]) {
				t.Fatalf("сборка %d: Match(%q) = %v, в первой сборке %v",
					i, q, got, want[j])
			}
		}
	}
}

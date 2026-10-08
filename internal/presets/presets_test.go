package presets

// Тесты на встроенные данные (data/*.yaml) и на конвертацию апстрима
// (import.go). Для конвертации используется маленькая фикстура с
// include-циклом, @ads-записями и безточечными доменами.

import (
	"os"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// --- встроенные данные ---

func TestListNotEmpty(t *testing.T) {
	list := List()
	if len(list) == 0 {
		t.Fatal("List() пуст: пресеты не сгенерированы — запусти go run ./scripts/presets-import")
	}
	if len(list) < 30 {
		t.Errorf("List() вернул %d пресетов, ожидалось >= 30", len(list))
	}

	seen := map[string]bool{}
	for i, p := range list {
		if p.Slug == "" || p.Title == "" {
			t.Errorf("пресет #%d: пустой slug/title: %+v", i, p)
		}
		if seen[p.Slug] {
			t.Errorf("дубль слага %q в List()", p.Slug)
		}
		seen[p.Slug] = true
		if p.Slug == metaSlug {
			t.Error("meta.yaml не должен попадать в List()")
		}
		if p.Method == "" {
			t.Errorf("%s: пустой method", p.Slug)
		}
		if p.Source.Empty() {
			t.Errorf("%s: пустой source", p.Slug)
		}
	}
}

func TestListSortedBySlug(t *testing.T) {
	list := List()
	for i := 1; i < len(list); i++ {
		if list[i-1].Slug > list[i].Slug {
			t.Fatalf("List() не отсортирован: %q перед %q", list[i-1].Slug, list[i].Slug)
		}
	}
}

func TestGetYouTube(t *testing.T) {
	p, ok := Get("youtube")
	if !ok {
		t.Fatal("Get(youtube) не найден")
	}
	if p.Slug != "youtube" {
		t.Errorf("slug=%q, ожидался youtube", p.Slug)
	}
	if p.Title != "YouTube" {
		t.Errorf("title=%q", p.Title)
	}
	// Витринный apex обязан быть первым: с него идут ASN-резолв и SNI.
	if len(p.Domains) == 0 || p.Domains[0] != "youtube.com" {
		t.Fatalf("витринный домен не первый: %v", firstN(p.Domains, 3))
	}
	if p.Large {
		t.Error("youtube не должен быть large")
	}
	if p.Method != DefaultMethod {
		t.Errorf("method=%q, ожидался %q", p.Method, DefaultMethod)
	}
	if len(p.Domains) > MaxDomains {
		t.Errorf("youtube: %d доменов, кап %d", len(p.Domains), MaxDomains)
	}
	for _, d := range p.Domains {
		if d == "ads.youtube.com" {
			t.Error("@ads-запись ads.youtube.com попала в пресет")
		}
		if !strings.Contains(d, ".") {
			t.Errorf("безточечный домен %q попал в пресет", d)
		}
	}
	if !contains(p.Domains, "youtu.be") {
		t.Errorf("youtu.be отсутствует: %v", p.Domains)
	}
	if !sortedAsc(p.Domains[1:]) {
		t.Errorf("хвост пресета не отсортирован: %v", p.Domains)
	}
}

func TestGetRublock(t *testing.T) {
	if !Has(RublockSlug) {
		t.Skip("нет rublock.yaml: зеркало не скачано (runtime-данные, не коммитятся)")
	}
	p, ok := Get(RublockSlug)
	if !ok {
		t.Fatal("Get(rublock) не найден")
	}
	if !p.Large {
		t.Error("rublock должен иметь large:true")
	}
	if p.Method != DefaultMethod {
		t.Errorf("method=%q, ожидался %q", p.Method, DefaultMethod)
	}
	if p.Source.Repo != SourceRublock {
		t.Errorf("source.repo=%q, ожидался %q", p.Source.Repo, SourceRublock)
	}
	// Блоклист РКН без капа — это сотни тысяч доменов, а не 50.
	if len(p.Domains) <= MaxDomains {
		t.Errorf("rublock: %d доменов — похоже, применён кап", len(p.Domains))
	}
	if !sortedAsc(p.Domains) {
		t.Error("rublock: домены не отсортированы")
	}
}

func TestGetUnknown(t *testing.T) {
	if _, ok := Get("no-such-preset-xyz"); ok {
		t.Error("Get() на несуществующий слаг вернул ok=true")
	}
	if _, ok := Get(""); ok {
		t.Error("Get(\"\") вернул ok=true")
	}
	if _, ok := Get(metaSlug); ok {
		t.Error("Get(meta) не должен возвращать пресет")
	}
}

func TestGetIsCaseInsensitive(t *testing.T) {
	if _, ok := Get("YouTube"); !ok {
		t.Error("Get(YouTube) не найден — слаг должен быть регистронезависимым")
	}
}

func TestHas(t *testing.T) {
	if !Has("youtube") {
		t.Error("Has(youtube)=false")
	}
	if Has(metaSlug) {
		t.Error("Has(meta)=true")
	}
	if Has("nope") {
		t.Error("Has(nope)=true")
	}
}

func TestMetaData(t *testing.T) {
	m, ok := MetaData()
	if !ok {
		t.Fatal("MetaData(): meta.yaml не найден")
	}
	if m.Upstream.Repo != SourceV2Fly {
		t.Errorf("meta.upstream.repo=%q, ожидался %q", m.Upstream.Repo, SourceV2Fly)
	}
	if len(m.Upstream.Commit) < 40 {
		t.Errorf("meta.upstream.commit=%q — не полный sha", m.Upstream.Commit)
	}
	if m.GeneratedAt == "" {
		t.Error("meta.generated_at пуст")
	}
	if m.Rublock <= 0 {
		t.Error("meta.rublock_entries должен быть > 0")
	}
	if m.PresetCount != len(m.Presets) {
		t.Errorf("meta.preset_count=%d, индекс содержит %d", m.PresetCount, len(m.Presets))
	}
	if len(m.Presets) == 0 {
		t.Fatal("meta.presets (индекс) пуст — перегенерируй пресеты")
	}
	for _, pm := range m.Presets {
		if pm.Slug == "" || pm.Title == "" || pm.Method == "" {
			t.Errorf("битая строка индекса: %+v", pm)
		}
		if pm.Count <= 0 {
			t.Errorf("%s: count=%d в индексе", pm.Slug, pm.Count)
		}
		if pm.Source.Empty() {
			t.Errorf("%s: пустой source в индексе", pm.Slug)
		}
	}
}

// TestListDoesNotMaterializeLarge — List() строит список по индексу из
// meta.yaml и не должен разбирать rublock.yaml (1.7 млн доменов).
func TestListDoesNotMaterializeLarge(t *testing.T) {
	for _, p := range List() {
		if !p.Large {
			if len(p.Domains) == 0 {
				t.Errorf("%s: обычный пресет без доменов", p.Slug)
			}
			continue
		}
		if p.Domains != nil {
			t.Errorf("%s: Large-пресет materializован в List() (%d доменов)", p.Slug, len(p.Domains))
		}
	}
}

// TestDomainsHelperForLarge — домены Large-пресета доступны через
// Get()/Domains().
func TestDomainsHelperForLarge(t *testing.T) {
	if !Has(RublockSlug) {
		t.Skip("нет rublock.yaml: зеркало не скачано (runtime-данные, не коммитятся)")
	}
	d, ok := Domains(RublockSlug)
	if !ok {
		t.Fatal("Domains(rublock) не найден")
	}
	if len(d) <= MaxDomains {
		t.Errorf("Domains(rublock)=%d — похоже, применён кап", len(d))
	}
	if _, ok := Domains("nope"); ok {
		t.Error("Domains(nope) вернул ok=true")
	}
}

// TestIndexMatchesFiles — индекс в meta.yaml не расходится с файлами.
func TestIndexMatchesFiles(t *testing.T) {
	m, ok := MetaData()
	if !ok {
		t.Fatal("MetaData(): meta.yaml не найден")
	}
	indexed := map[string]int{}
	for _, pm := range m.Presets {
		indexed[pm.Slug] = pm.Count
	}
	for _, p := range List() {
		want, ok := indexed[p.Slug]
		if !ok {
			t.Errorf("пресет %q есть в data/, но отсутствует в индексе meta.yaml", p.Slug)
			continue
		}
		if p.Large {
			continue
		}
		if len(p.Domains) != want {
			t.Errorf("%s: файл содержит %d доменов, индекс обещает %d", p.Slug, len(p.Domains), want)
		}
	}
	if len(indexed) != len(List()) {
		t.Errorf("в индексе %d пресетов, в data/ %d", len(indexed), len(List()))
	}
}

func TestPresetSourceString(t *testing.T) {
	if (PresetSource{}).String() != "unknown" {
		t.Error("пустой source должен печататься как unknown")
	}
	s := PresetSource{Repo: "a/b", Commit: "0123456789abcdef0123", Date: "2026-01-01"}
	got := s.String()
	if !strings.Contains(got, "0123456789ab") || strings.Contains(got, "0123456789abcdef") {
		t.Errorf("String()=%q — ожидался короткий sha", got)
	}
}

// TestCatalogCoverage — каждый заявленный стартовый слаг есть в
// сгенерированных данных (ловит забытый апстрим-список).
func TestCatalogCoverage(t *testing.T) {
	want := []string{
		"anthropic", "category-proxy-tunnels", "category-vpnservices",
		"cloudflare", "disney", "discord", "dropbox", "facebook", "github",
		"gitlab", "google-deepmind", "hbo", "instagram", "linkedin",
		"netflix", "nordvpn", "onedrive", "openai", "paypal", "perplexity",
		"primevideo", "protonmail", "reddit", "signal", "spotify",
		"telegram", "tiktok", "twitch", "twitter", "whatsapp", "xai",
		"youtube", RublockSlug,
	}
	for _, slug := range want {
		if _, ok := Get(slug); !ok {
			t.Errorf("пресет %q отсутствует в data/", slug)
		}
	}
}

// TestShowcaseMatchesCatalog — первый домен обязан совпадать с явно
// заданным витринным apex'ом из каталога (никаких "youtube.ae" из
// фолбэка). Первый домен идёт в ServiceOverride.Primary: с него
// резолвится ASN и делается SNI-проба.
func TestShowcaseMatchesCatalog(t *testing.T) {
	for _, c := range catalog {
		if c.Showcase == "" {
			t.Errorf("в каталоге %q не задан витринный домен", c.Slug)
			continue
		}
		p, ok := Get(c.Slug)
		if !ok {
			t.Errorf("пресет %q отсутствует в data/", c.Slug)
			continue
		}
		if len(p.Domains) == 0 {
			t.Errorf("%s: пустой список доменов", c.Slug)
			continue
		}
		if p.Domains[0] != c.Showcase {
			t.Errorf("%s: витринный домен %q, ожидался %q", c.Slug, p.Domains[0], c.Showcase)
		}
		if !sortedAsc(p.Domains[1:]) {
			t.Errorf("%s: хвост списка не отсортирован", c.Slug)
		}
	}
}

// --- конвертация апстрима: golden-фикстура ---

// upstreamFixture — мини-репо на реальных слагах каталога: цикл
// telegram <-> reddit, @ads/@telemetry, безточечные домены,
// keyword/regexp, full:, inline-комментарий, регистровый дубль.
func upstreamFixture() map[string]string {
	return map[string]string{
		"telegram": `# telegram
include:reddit
include:missing-include-target
keyword:track
regexp:^ads[0-9]+\.example\.com$
com-example.com @ads
bare-no-dot
full:exact.example.com
   full:   spaced.example.com   # inline-комментарий
plain.example.com
domain:with-dot.example.net
domain:still-no-dot
telemetry.demo.example @telemetry
cn-only.example.com @!cn
DEMO.COM
plain.example.com
`,
		"reddit": `include:telegram # цикл назад
red.example.com
`,
		// Второй пресет для проверки капа (витринного nordvpn.com нет,
		// поэтому сработает фолбэк).
		"nordvpn": manyDomains(60, "nordvpn"),
	}
}

func manyDomains(n int, base string) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		if i%15 == 0 {
			b.WriteString("# блок " + base + "\n")
		}
		b.WriteString(strconvItoa(i) + "." + base + ".example\n")
	}
	return b.String()
}

func strconvItoa(i int) string {
	if i == 0 {
		return "0"
	}
	var buf []byte
	for i > 0 {
		buf = append([]byte{byte('0' + i%10)}, buf...)
		i /= 10
	}
	return string(buf)
}

func testSource() PresetSource {
	return PresetSource{
		Repo:   SourceV2Fly,
		Commit: "0000000000000000000000000000000000000000",
		Date:   "2026-01-01T00:00:00Z",
	}
}

func TestParseUpstreamGolden(t *testing.T) {
	list, st := ParseUpstream(upstreamFixture(), testSource())

	tg := findPreset(t, list, "telegram")

	// Витринных telegram.org/reddit.com/nordvpn.com в фикстуре нет ->
	// фолбэк обязан отработать и записать предупреждения (контракт:
	// warn, а не молчаливый выбор).
	for _, slug := range []string{"telegram", "reddit", "nordvpn"} {
		if !hasFallback(st, slug) {
			t.Errorf("нет предупреждения о фолбэке для %s: %v", slug, st.ShowcaseFallback)
		}
	}

	// Порядок: витринный домен (фолбэк demo.com) первым, остальное — по
	// возрастанию. include:reddit развёрнут, цикл telegram<->reddit
	// остановлен.
	want := []string{
		"demo.com", // витринный (фолбэк)
		"cn-only.example.com",
		"exact.example.com",
		"plain.example.com",
		"red.example.com",      // из include:reddit
		"spaced.example.com",   // full: с лишними пробелами
		"with-dot.example.net", // domain: с точкой
	}
	if len(tg.Domains) != len(want) {
		t.Fatalf("telegram: %v\nожидалось %v", tg.Domains, want)
	}
	for i := range want {
		if tg.Domains[i] != want[i] {
			t.Fatalf("telegram[%d]=%q, ожидался %q\nполный список: %v", i, tg.Domains[i], want[i], tg.Domains)
		}
	}
	if tg.Method != DefaultMethod {
		t.Errorf("method=%q, ожидался %q", tg.Method, DefaultMethod)
	}
	if tg.Source != testSource() {
		t.Errorf("source=%+v, ожидался %+v", tg.Source, testSource())
	}
	if tg.Large {
		t.Error("telegram не должен быть large")
	}

	// Отброшенные классы записей.
	for _, banned := range []string{
		"bare-no-dot",            // domain: без точки
		"still-no-dot",           // domain: без точки
		"track",                  // keyword:
		"com-example.com",        // @ads
		"telemetry.demo.example", // @telemetry
	} {
		if contains(tg.Domains, banned) {
			t.Errorf("запись %q не должна была попасть в пресет", banned)
		}
	}
	if st.Keyword != 2 {
		t.Errorf("stats.Keyword=%d, ожидалось 2 (telegram + reddit обходят файл дважды)", st.Keyword)
	}
	if st.Regexp != 2 {
		t.Errorf("stats.Regexp=%d, ожидалось 2", st.Regexp)
	}
	if st.Ads != 4 {
		t.Errorf("stats.Ads=%d, ожидалось 4 (@ads + @telemetry, два обхода)", st.Ads)
	}
	if st.DotlessDomain != 4 {
		t.Errorf("stats.DotlessDomain=%d, ожидалось 4 (два обхода)", st.DotlessDomain)
	}
	if st.Full != 4 {
		t.Errorf("stats.Full=%d, ожидалось 4 (два обхода)", st.Full)
	}
	if st.Includes != 2 {
		t.Errorf("stats.Includes=%d, ожидалось 2 (telegram->reddit, reddit->telegram)", st.Includes)
	}
	if st.IncludeCycles != 2 {
		t.Errorf("stats.IncludeCycles=%d, ожидалось 2 (по одному циклу с каждой стороны)", st.IncludeCycles)
	}
	if len(st.MissingIncludes) != 2 || st.MissingIncludes[0] != "missing-include-target" {
		t.Errorf("stats.MissingIncludes=%v", st.MissingIncludes)
	}
	// Dedupe: "plain.example.com" идёт дважды в каждом обходе.
	if st.Duplicates != 2 {
		t.Errorf("stats.Duplicates=%d, ожидалось 2 (по одному на обход)", st.Duplicates)
	}
	if st.Invalid != 0 {
		t.Errorf("stats.Invalid=%d, ожидалось 0", st.Invalid)
	}
}

func TestParseUpstreamCap(t *testing.T) {
	list, st := ParseUpstream(upstreamFixture(), testSource())

	p := findPreset(t, list, "nordvpn")
	if len(p.Domains) != MaxDomains {
		t.Errorf("nordvpn: %d доменов, ожидался кап %d", len(p.Domains), MaxDomains)
	}
	if st.Truncated != 10 {
		t.Errorf("stats.Truncated=%d, ожидалось 10", st.Truncated)
	}
	if !sortedAsc(p.Domains) {
		t.Error("nordvpn не отсортирован")
	}

	// Обрезка обязана быть префиксом отсортированного списка.
	var want []string
	for _, line := range strings.Split(manyDomains(60, "nordvpn"), "\n") {
		if line = stripComment(line); line != "" {
			want = append(want, line)
		}
	}
	sort.Strings(want)
	for i := 0; i < MaxDomains && i < len(want); i++ {
		if p.Domains[i] != want[i] {
			t.Fatalf("nordvpn[%d]=%q, ожидался %q (обрезанный отсортированный список)", i, p.Domains[i], want[i])
		}
	}
}

func TestParseUpstreamExplicitShowcaseFirst(t *testing.T) {
	files := upstreamFixture()
	// Апстрим, где витринный домен есть, но не в начале списка.
	files["youtube"] = "aaa.youtube.example\nzzz.youtube.example\nyoutube.com\n"
	list, st := ParseUpstream(files, testSource())

	y := findPreset(t, list, "youtube")
	if len(y.Domains) != 3 {
		t.Fatalf("youtube: %v", y.Domains)
	}
	if y.Domains[0] != "youtube.com" {
		t.Errorf("витринный домен youtube=%q, ожидался youtube.com", y.Domains[0])
	}
	if y.Domains[1] != "aaa.youtube.example" || y.Domains[2] != "zzz.youtube.example" {
		t.Errorf("остальные не отсортированы: %v", y.Domains)
	}
	// youtube.com найден -> предупреждений про него быть не должно.
	if hasFallback(st, "youtube") {
		t.Errorf("ложное предупреждение о витринном домене youtube: %v", st.ShowcaseFallback)
	}
	for _, slug := range []string{"telegram", "reddit", "nordvpn"} {
		if !hasFallback(st, slug) {
			t.Errorf("нет предупреждения о фолбэке для %s: %v", slug, st.ShowcaseFallback)
		}
	}
}

func TestParseUpstreamDeterministic(t *testing.T) {
	f := upstreamFixture()
	a, _ := ParseUpstream(f, testSource())
	b, _ := ParseUpstream(f, testSource())
	ra, _ := yaml.Marshal(a)
	rb, _ := yaml.Marshal(b)
	if string(ra) != string(rb) {
		t.Error("ParseUpstream не детерминирован")
	}
}

func TestParseUpstreamMissingListIsWarn(t *testing.T) {
	// Пустой вход: ни одного списка. Падения быть не должно.
	list, st := ParseUpstream(map[string]string{}, testSource())
	if len(list) != 0 {
		t.Errorf("получено %d пресетов из пустого входа", len(list))
	}
	if len(st.MissingLists) != len(catalog) {
		t.Errorf("stats.MissingLists=%d, ожидалось %d", len(st.MissingLists), len(catalog))
	}
	if len(st.Skipped) != len(catalog) {
		t.Errorf("stats.Skipped=%d, ожидалось %d", len(st.Skipped), len(catalog))
	}
	for _, slug := range st.MissingLists {
		if !contains(Slugs(), slug) {
			t.Errorf("MissingLists содержит неизвестный слаг %q", slug)
		}
	}
}

func TestParseRublock(t *testing.T) {
	body := strings.Join([]string{
		"# комментарий",
		"",
		`"quoted.ru`, // кавычка — особенность апстрима
		"b.example.com",
		"a.example.com",
		"B.EXAMPLE.COM", // дубль после lower
		"  spaced.example.com  ",
		"nodots",             // не домен
		"not a domain.com x", // пробел внутри
		"http://weird.com",   // URL
		"xn--80ak6aa92e.com", // punycode
		"пример.рф",          // IDN
		"trailing.example.com.",
		"sub.пример.рф",
	}, "\n")

	p, st := ParseRublock(body, testSource(), 0)

	if p.Slug != RublockSlug {
		t.Errorf("slug=%q, ожидался %q", p.Slug, RublockSlug)
	}
	if !p.Large {
		t.Error("rublock должен быть large")
	}
	if p.Method != DefaultMethod {
		t.Errorf("method=%q", p.Method)
	}
	if !sortedAsc(p.Domains) {
		t.Errorf("не отсортировано: %v", p.Domains)
	}

	// Сортировка побайтовая: non-ASCII (кириллица) уходит в конец.
	want := []string{
		"a.example.com",
		"b.example.com",
		"quoted.ru",
		"spaced.example.com",
		"sub.пример.рф",
		"trailing.example.com",
		"xn--80ak6aa92e.com",
		"пример.рф",
	}
	if len(p.Domains) != len(want) {
		t.Fatalf("получено %v, ожидалось %v", p.Domains, want)
	}
	for i := range want {
		if p.Domains[i] != want[i] {
			t.Fatalf("Domains[%d]=%q, ожидался %q\nполный список: %v", i, p.Domains[i], want[i], p.Domains)
		}
	}

	if st.Duplicates != 1 {
		t.Errorf("stats.Duplicates=%d, ожидался 1", st.Duplicates)
	}
	if st.Comments != 1 {
		t.Errorf("stats.Comments=%d, ожидался 1", st.Comments)
	}
	if st.Invalid != 3 {
		t.Errorf("stats.Invalid=%d, ожидалось 3 (nodots, пробел, URL)", st.Invalid)
	}
	if st.Truncated != 0 {
		t.Errorf("stats.Truncated=%d, ожидалось 0 (max=0 = без капа)", st.Truncated)
	}
}

func TestParseRublockCap(t *testing.T) {
	var lines []string
	for i := 0; i < 10; i++ {
		lines = append(lines, strconvItoa(i)+".example.com")
	}
	p, st := ParseRublock(strings.Join(lines, "\n"), testSource(), 3)
	if len(p.Domains) != 3 {
		t.Errorf("рублок с капом 3: получено %d", len(p.Domains))
	}
	if st.Truncated != 7 {
		t.Errorf("stats.Truncated=%d, ожидалось 7", st.Truncated)
	}
	for _, d := range p.Domains {
		if d == "5.example.com" {
			t.Error("кап должен срезать хвост")
		}
	}
}

func TestSanitizeDomain(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"Example.COM", "example.com", true},
		{" example.com ", "example.com", true},
		{`"example.com"`, "example.com", true},
		{"example.com.", "example.com", true},
		{".example.com", "example.com", true},
		{"a.b.c.example.com", "a.b.c.example.com", true},
		{"under_score.example.com", "under_score.example.com", true},
		{"пример.рф", "пример.рф", true},
		{"xn--e1afmkfd.xn--p1ai", "xn--e1afmkfd.xn--p1ai", true},
		{"example", "", false},
		{"", "", false},
		{".", "", false},
		{"a..b", "", false},
		{"-bad.example.com", "", false},
		{"bad-.example.com", "", false},
		{"a b.example.com", "", false},
		{"http://example.com", "", false},
		{"a/b.example.com", "", false},
		{"ex*ample.com", "", false},
	}
	for _, tc := range cases {
		got, ok := sanitizeDomain(tc.in)
		if ok != tc.ok || got != tc.want {
			t.Errorf("sanitizeDomain(%q)=(%q,%v), ожидалось (%q,%v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestHasAdsAttr(t *testing.T) {
	cases := []struct {
		attrs []string
		want  bool
	}{
		{nil, false},
		{[]string{"@cn"}, false},
		{[]string{"@!cn"}, false},
		{[]string{"@ads"}, true},
		{[]string{"@ADS"}, true},
		{[]string{"@telemetry"}, true},
		{[]string{"@cn", "@ads"}, true},
	}
	for _, tc := range cases {
		if got := hasAdsAttr(tc.attrs); got != tc.want {
			t.Errorf("hasAdsAttr(%v)=%v, ожидалось %v", tc.attrs, got, tc.want)
		}
	}
}

func TestParseSpec(t *testing.T) {
	cases := []struct {
		in    string
		value string
		kind  specKind
	}{
		{"full:a.com", "a.com", kindFull},
		{"domain:a.com", "a.com", kindDomain},
		{"keyword:ads", "ads", kindKeyword},
		{"regexp:^a", "^a", kindRegexp},
		{"include:other", "other", kindInclude},
		{"bare.com", "bare.com", kindDomain},
		{"full: spaced.com", "spaced.com", kindFull},
	}
	for _, tc := range cases {
		v, k := parseSpec(tc.in)
		if v != tc.value || k != tc.kind {
			t.Errorf("parseSpec(%q)=(%q,%v), ожидалось (%q,%v)", tc.in, v, k, tc.value, tc.kind)
		}
	}
}

func TestStripComment(t *testing.T) {
	cases := map[string]string{
		"a.com # note":      "a.com",
		"a.com":             "a.com",
		"# whole line":      "",
		"  full:x.com  # y": "full:x.com",
		"":                  "",
		"   ":               "",
	}
	for in, want := range cases {
		if got := stripComment(in); got != want {
			t.Errorf("stripComment(%q)=%q, ожидалось %q", in, got, want)
		}
	}
}

// TestDataDirOnlyPresets — в data/ не должно быть посторонних файлов.
func TestDataDirOnlyPresets(t *testing.T) {
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", dataDir, err)
	}
	known := map[string]bool{"meta.yaml": true}
	// Ручные пресеты вне генератора (rublock-ip: static_url на ip.lst).
	known["rublock-ip.yaml"] = true
	for _, s := range Slugs() {
		known[s+".yaml"] = true
	}
	for _, e := range entries {
		if e.IsDir() {
			t.Errorf("в data/ есть подкаталог %q", e.Name())
			continue
		}
		if !known[e.Name()] {
			t.Errorf("лишний файл в data/: %q", e.Name())
		}
	}
}

// TestNoLeftoverTmpFiles — генератор пишет через .tmp; залипшие хвосты
// не должны попадать в репозиторий.
func TestNoLeftoverTmpFiles(t *testing.T) {
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("остался временный файл %q", e.Name())
		}
	}
}

// --- бенчмарки: rublock внутри embed стоит дорого, это должно быть
// видно в CI, а не на проде. ---

func BenchmarkGetRublockCold(b *testing.B) {
	// Сбрасываем кэш, чтобы мерить разбор YAML с нуля.
	for b.Loop() {
		cacheMu.Lock()
		cache = map[string]*Preset{}
		cacheMu.Unlock()
		_, _ = Get(RublockSlug)
	}
}

func BenchmarkGetRublockCached(b *testing.B) {
	_, _ = Get(RublockSlug)
	for b.Loop() {
		_, _ = Get(RublockSlug)
	}
}

func BenchmarkList(b *testing.B) {
	for b.Loop() {
		_ = List()
	}
}

// --- helpers ---

func findPreset(t *testing.T, list []Preset, slug string) Preset {
	t.Helper()
	for _, p := range list {
		if p.Slug == slug {
			return p
		}
	}
	t.Fatalf("пресет %q не найден среди %d пресетов", slug, len(list))
	return Preset{}
}

func sortedAsc(in []string) bool {
	for i := 1; i < len(in); i++ {
		if in[i-1] > in[i] {
			return false
		}
	}
	return true
}

// hasFallback — есть ли предупреждение о фолбэке витринного домена
// для конкретного слага.
func hasFallback(st Stats, slug string) bool {
	for _, w := range st.ShowcaseFallback {
		if strings.HasPrefix(w, slug+":") {
			return true
		}
	}
	return false
}

func firstN(in []string, n int) []string {
	if len(in) < n {
		return in
	}
	return in[:n]
}

func TestCronExpr(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"", "", false},
		{"manual", "", false},
		{"disabled", "", false},
		{"every 6h", "0 */6 * * *", false},
		{"every 1h", "0 */1 * * *", false},
		{"every 30m", "*/30 * * * *", false},
		{"daily at 03:00", "0 3 * * *", false},
		{"0 3 * * *", "0 3 * * *", false},
		{"every 0h", "", true},
		{"every 2m", "", true},
		{"daily at 25:00", "", true},
		{"nonsense", "", true},
	}
	for _, c := range cases {
		got, err := CronExpr(c.in)
		if c.wantErr && err == nil {
			t.Errorf("CronExpr(%q): want error", c.in)
		}
		if !c.wantErr && (err != nil || got != c.want) {
			t.Errorf("CronExpr(%q)=%q,%v want %q", c.in, got, err, c.want)
		}
	}
}

func TestDiskOverlay(t *testing.T) {
	old := DiskDir
	t.Cleanup(func() { DiskDir = old; FlushAll() })
	dir := t.TempDir()
	DiskDir = dir
	FlushAll()
	if _, ok := Get("fromdisk"); ok {
		t.Fatal("unexpected hit before mirror")
	}
	raw := "slug: fromdisk\ntitle: From Disk\ndomains:\n  - example.com\n  - example.org\nmethod: whois\n"
	if err := os.WriteFile(dir+"/fromdisk.yaml", []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	p, ok := Get("fromdisk")
	if !ok || len(p.Domains) != 2 || p.Method != "whois" {
		t.Fatalf("disk overlay miss: %+v %v", p, ok)
	}
	// Обновление зеркала видно только после FlushAll (кэш).
	if err := os.WriteFile(dir+"/fromdisk.yaml", []byte(raw+"# touch\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	FlushAll()
	if _, ok := Get("fromdisk"); !ok {
		t.Fatal("disk overlay miss after flush")
	}
}

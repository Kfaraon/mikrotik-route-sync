package presets

// import.go — конвертация апстрим-источников в пресеты.
//
// Две независимые части:
//   - ParseUpstream — v2fly/domain-list-community, файлы из data/*;
//   - ParseRublock  — плоский список antifilter.download/list/domains.lst.
//
// Всё детерминировано: порядок доменов зависит только от содержимого
// входа, map-итерация никогда не влияет на результат.

import (
	"sort"
	"strings"
	"unicode"
)

// MaxDomains — кап на количество доменов в обычном пресете.
// Список, который не влезает, обрезается с предупреждением.
const MaxDomains = 50

// DefaultMethod — метод сбора для всех генерируемых пресетов.
//
// Значение намеренно берётся из набора config.knownMethods
// (cdn/asn/dynamic/whois/static_url): пресет — это статический список
// доменов, поэтому при применении он попадает в ServiceOverride.Method
// и обязан пройти ValidateMethods.
const DefaultMethod = "static_url"

// RublockSlug — слаг пресета с antifilter-блоклистом.
const RublockSlug = "rublock"

// SourceV2Fly / SourceRublock — идентификаторы источников в PresetSource.Repo.
const (
	SourceV2Fly   = "v2fly/domain-list-community"
	SourceRublock = "antifilter.download"
)

// adsLikeAttrs — атрибуты, означающие «реклама/трекинг»: такие записи
// в пресет не попадают. Upstream переименовал @ads в @telemetry,
// поэтому учитываются оба написания.
var adsLikeAttrs = map[string]bool{
	"ads":       true,
	"telemetry": true,
}

// catalogEntry — описание пресета, который мы хотим получить.
type catalogEntry struct {
	Slug  string
	Title string
	// Showcase — витринный apex-домен, который должен встать первым
	// (аналог ServiceOverride.Primary: первый домен идёт на ASN-резолв
	// и SNI-пробу). Пусто — выбирается детерминированным фолбэком.
	Showcase string
}

// catalog — целевой набор пресетов. Порядок фиксирован и определяет
// порядок генерации; отсутствующий апстрим-список даёт warn и пропуск.
var catalog = []catalogEntry{
	{Slug: "anthropic", Title: "Anthropic (Claude)", Showcase: "anthropic.com"},
	{Slug: "cloudflare", Title: "Cloudflare", Showcase: "cloudflare.com"},
	{Slug: "disney", Title: "Disney+", Showcase: "disneyplus.com"},
	{Slug: "discord", Title: "Discord", Showcase: "discord.com"},
	{Slug: "dropbox", Title: "Dropbox", Showcase: "dropbox.com"},
	{Slug: "facebook", Title: "Facebook", Showcase: "facebook.com"},
	{Slug: "github", Title: "GitHub", Showcase: "github.com"},
	{Slug: "gitlab", Title: "GitLab", Showcase: "gitlab.com"},
	{Slug: "google-deepmind", Title: "Google DeepMind / Gemini", Showcase: "deepmind.com"},
	{Slug: "hbo", Title: "HBO Max", Showcase: "hbo.com"},
	{Slug: "instagram", Title: "Instagram", Showcase: "instagram.com"},
	{Slug: "linkedin", Title: "LinkedIn", Showcase: "linkedin.com"},
	{Slug: "netflix", Title: "Netflix", Showcase: "netflix.com"},
	{Slug: "nordvpn", Title: "NordVPN", Showcase: "nordvpn.com"},
	{Slug: "onedrive", Title: "OneDrive", Showcase: "onedrive.com"},
	{Slug: "openai", Title: "OpenAI (ChatGPT)", Showcase: "openai.com"},
	{Slug: "paypal", Title: "PayPal", Showcase: "paypal.com"},
	{Slug: "perplexity", Title: "Perplexity AI", Showcase: "perplexity.ai"},
	{Slug: "primevideo", Title: "Amazon Prime Video", Showcase: "primevideo.com"},
	{Slug: "protonmail", Title: "Proton Mail", Showcase: "proton.me"},
	{Slug: "reddit", Title: "Reddit", Showcase: "reddit.com"},
	{Slug: "signal", Title: "Signal", Showcase: "signal.org"},
	{Slug: "spotify", Title: "Spotify", Showcase: "spotify.com"},
	{Slug: "telegram", Title: "Telegram", Showcase: "telegram.org"},
	{Slug: "tiktok", Title: "TikTok", Showcase: "tiktok.com"},
	{Slug: "twitch", Title: "Twitch", Showcase: "twitch.tv"},
	{Slug: "twitter", Title: "Twitter (X)", Showcase: "twitter.com"},
	{Slug: "whatsapp", Title: "WhatsApp", Showcase: "whatsapp.com"},
	{Slug: "xai", Title: "xAI (Grok)", Showcase: "x.ai"},
	{Slug: "youtube", Title: "YouTube", Showcase: "youtube.com"},
	{Slug: "category-proxy-tunnels", Title: "Proxy / tunnel services", Showcase: "tailscale.com"},
	{Slug: "category-vpnservices", Title: "VPN services", Showcase: "nordvpn.com"},
}

// Stats — счётчики конвертации. Заполняются ParseUpstream/ParseRublock
// и печатаются генератором в stdout.
type Stats struct {
	Files            int      // файлов апстрима прочитано
	Includes         int      // раскрытых include:
	IncludeDup       int      // include:, уже разобранный раньше (дедуп)
	IncludeCycles    int      // include:, ведущий в цикл
	Full             int      // принято full:
	Domain           int      // принято domain:/bare
	DotlessDomain    int      // отброшено domain: без точки
	Keyword          int      // отброшено keyword:
	Regexp           int      // отброшено regexp:
	Ads              int      // отброшено по @ads/@telemetry
	Comments         int      // строк-комментариев и inline-комментариев
	Blank            int      // пустых строк
	Invalid          int      // непохожих на домен значений
	Duplicates       int      // повторов доменов внутри пресета
	Truncated        int      // срезано капом MaxDomains
	MissingLists     []string // запрошенные, но отсутствующие апстрим-списки
	MissingIncludes  []string // include: на несуществующий файл
	ShowcaseFallback []string // пресеты, где витринный домен не найден
	Skipped          []string // пресеты, пропущенные целиком
}

// ParseUpstream конвертирует снимок data/* из v2fly/domain-list-community
// в пресеты.
//
// files — карта "имя списка (без пути и расширения)" → содержимое файла.
// src — источник (repo/commit/date), проставляемый всем пресетам.
//
// Правила конвертации:
//   - full:X            → принять X;
//   - domain:X / X      → принять, если X содержит точку;
//   - domain:X без точки→ отбросить (DotlessDomain);
//   - keyword:/regexp:  → отбросить с подсчётом;
//   - include:X         → развернуть рекурсивно, с защитой от циклов;
//   - @ads / @telemetry → отбросить с подсчётом.
//
// Отсутствующий апстирм-список не является ошибкой: он попадает в
// Stats.MissingLists/Skipped, а в caller попадает warn.
func ParseUpstream(files map[string]string, src PresetSource) ([]Preset, Stats) {
	var st Stats
	p := &parser{files: files, stats: &st}
	out := make([]Preset, 0, len(catalog))

	for _, c := range catalog {
		if _, ok := files[c.Slug]; !ok {
			st.MissingLists = append(st.MissingLists, c.Slug)
			st.Skipped = append(st.Skipped, c.Slug)
			continue
		}
		st.Files++

		p.reset()
		raw := p.resolve(c.Slug)

		domains, dup := dedupe(raw)
		st.Duplicates += dup

		showcase := c.Showcase
		switch {
		case showcase == "":
			showcase = heuristicShowcase(c.Slug, domains)
			st.ShowcaseFallback = append(st.ShowcaseFallback,
				c.Slug+": витринный домен не задан, взят "+displayOrNone(showcase))
		case !contains(domains, showcase):
			showcase = heuristicShowcase(c.Slug, domains)
			st.ShowcaseFallback = append(st.ShowcaseFallback,
				c.Slug+": "+c.Showcase+" не найден, взят "+displayOrNone(showcase))
		}

		domains = orderDomains(domains, showcase)
		if len(domains) > MaxDomains {
			st.Truncated += len(domains) - MaxDomains
			domains = domains[:MaxDomains]
		}

		out = append(out, Preset{
			Slug:    c.Slug,
			Title:   c.Title,
			Domains: domains,
			Method:  DefaultMethod,
			Source:  src,
		})
	}

	st.sortAll()
	return out, st
}

// ParseRublock конвертирует плоский список antifilter.download
// (по одному домену в строке) в пресет RublockSlug.
//
// Кап не применяется: max <= 0 означает «оставить как есть».
// max > 0 обрезает список с фиксацией в Stats.Truncated (используется
// только в тестах).
func ParseRublock(body string, src PresetSource, max int) (Preset, Stats) {
	var st Stats
	seen := make(map[string]bool)
	raw := make([]string, 0, 1<<16)

	for _, line := range strings.Split(body, "\n") {
		s := strings.TrimSpace(line)
		if s == "" {
			if line != "" && strings.TrimSpace(line) == "" {
				st.Blank++
			}
			continue
		}
		if strings.HasPrefix(s, "#") {
			st.Comments++
			continue
		}
		d, ok := sanitizeDomain(s)
		if !ok {
			st.Invalid++
			continue
		}
		if seen[d] {
			st.Duplicates++
			continue
		}
		seen[d] = true
		raw = append(raw, d)
	}

	domains := raw
	sort.Strings(domains)
	if max > 0 && len(domains) > max {
		st.Truncated += len(domains) - max
		domains = domains[:max]
	}

	st.sortAll()
	return Preset{
		Slug:    RublockSlug,
		Title:   "Antifilter blocklist (РКН)",
		Domains: domains,
		Method:  DefaultMethod,
		Large:   true,
		Source:  src,
	}, st
}

// parser — разбор одного апстрим-дерева include:.
type parser struct {
	files map[string]string
	stats *Stats
	stack map[string]bool // сейчас раскрывается (поиск цикла)
	done  map[string]bool // уже раскрыт (дедуп include)
}

func (p *parser) reset() {
	p.stack = make(map[string]bool)
	p.done = make(map[string]bool)
}

// resolve раскрывает список slug, включая все include:, и возвращает
// нормализованные значения доменов (без дедупа и сортировки).
func (p *parser) resolve(slug string) []string {
	p.stack[slug] = true
	defer delete(p.stack, slug)

	var out []string
	content, ok := p.files[slug]
	if !ok {
		p.stats.MissingIncludes = append(p.stats.MissingIncludes, slug)
		return nil
	}

	for _, line := range strings.Split(content, "\n") {
		s := stripComment(line)
		if s == "" {
			if strings.TrimSpace(line) == "" {
				p.stats.Blank++
			} else {
				p.stats.Comments++
			}
			continue
		}

		fields := strings.Fields(s)
		if len(fields) == 0 {
			continue
		}
		spec, attrs := splitSpec(fields)

		if hasAdsAttr(attrs) {
			p.stats.Ads++
			continue
		}

		value, kind := parseSpec(spec)
		switch kind {
		case kindInclude:
			target := strings.TrimSpace(value)
			if target == "" {
				p.stats.Invalid++
				continue
			}
			if p.stack[target] {
				p.stats.IncludeCycles++
				continue
			}
			if p.done[target] {
				p.stats.IncludeDup++
				continue
			}
			if _, ok := p.files[target]; !ok {
				p.stats.MissingIncludes = append(p.stats.MissingIncludes, target)
				continue
			}
			p.stats.Includes++
			out = append(out, p.resolve(target)...)
		case kindFull:
			d, ok := sanitizeDomain(value)
			if !ok {
				p.stats.Invalid++
				continue
			}
			p.stats.Full++
			out = append(out, d)
		case kindDomain:
			if !strings.Contains(value, ".") {
				p.stats.DotlessDomain++
				continue
			}
			d, ok := sanitizeDomain(value)
			if !ok {
				p.stats.Invalid++
				continue
			}
			p.stats.Domain++
			out = append(out, d)
		case kindKeyword:
			p.stats.Keyword++
		case kindRegexp:
			p.stats.Regexp++
		}
	}

	p.done[slug] = true
	return out
}

type specKind int

const (
	kindDomain specKind = iota
	kindFull
	kindKeyword
	kindRegexp
	kindInclude
)

// parseSpec разбирает спецификацию записи: "full:x", "domain:x",
// "keyword:x", "regexp:x", "include:x" либо голое "x" (= domain:x).
// Для keyword/regexp/include берётся только первый токен — остальное
// значение там бессмысленно.
func parseSpec(spec string) (value string, kind specKind) {
	for _, pfx := range []struct {
		name string
		kind specKind
	}{
		{"full:", kindFull},
		{"domain:", kindDomain},
		{"keyword:", kindKeyword},
		{"regexp:", kindRegexp},
		{"include:", kindInclude},
	} {
		if !strings.HasPrefix(spec, pfx.name) {
			continue
		}
		value = strings.TrimSpace(spec[len(pfx.name):])
		switch pfx.kind {
		case kindKeyword, kindRegexp, kindInclude:
			if i := strings.IndexAny(value, " \t"); i >= 0 {
				value = value[:i]
			}
		}
		return value, pfx.kind
	}
	if i := strings.IndexAny(spec, " \t"); i >= 0 {
		spec = spec[:i]
	}
	return spec, kindDomain
}

// splitSpec отделяет атрибуты (@cn, @!cn, @ads) от значения записи.
// Апстрим допускает "full:  example.com", поэтому не-атрибутные
// хвосты склеиваются обратно в спецификацию.
func splitSpec(fields []string) (spec string, attrs []string) {
	parts := make([]string, 0, len(fields))
	for _, f := range fields {
		if len(parts) > 0 && strings.HasPrefix(f, "@") {
			attrs = append(attrs, f)
			continue
		}
		parts = append(parts, f)
	}
	return strings.Join(parts, " "), attrs
}

// hasAdsAttr — есть ли среди атрибов рекламно-трекинговый.
// "@!cn" нормализуется в "cn" и рекламным не считается.
func hasAdsAttr(attrs []string) bool {
	for _, a := range attrs {
		name := strings.ToLower(strings.TrimPrefix(strings.TrimPrefix(a, "@"), "!"))
		if adsLikeAttrs[name] {
			return true
		}
	}
	return false
}

// stripComment убирает "# ..." (в т.ч. inline-комментарий после
// пробела) и возвращает оставшуюся часть строки.
func stripComment(line string) string {
	if i := strings.Index(line, "#"); i >= 0 {
		line = line[:i]
	}
	return strings.TrimSpace(line)
}

// sanitizeDomain нормализует домен и проверяет его форму.
// Возвращает ok=false, если значение доменом быть не может.
//
// Разрешены буквы/цифры Unicode: блоклист РКН на ~0.5% состоит из
// кириллических IDN (xn-- в апстриме отсутствует), а RouterOS address-list
// хранит их как есть, резолвер сам уходит в punycode.
func sanitizeDomain(v string) (string, bool) {
	// Апстрим antifilter изредка оборачивает запись в кавычки.
	v = strings.TrimSpace(v)
	v = strings.Trim(v, `"`)
	v = strings.TrimSpace(v)
	v = strings.ToLower(v)
	// Отбрасываем псевдонимы DNS-зоны и хостовые руты без FQDN.
	v = strings.TrimSuffix(v, ".")
	v = strings.TrimPrefix(v, ".")

	if v == "" || len(v) > 253 || !strings.Contains(v, ".") {
		return "", false
	}
	for _, label := range strings.Split(v, ".") {
		if label == "" || len(label) > 63 {
			return "", false
		}
		if label[0] == '-' || label[len(label)-1] == '-' {
			return "", false
		}
		for _, r := range label {
			switch {
			case unicode.IsLetter(r), unicode.IsDigit(r), r == '-', r == '_':
			default:
				return "", false
			}
		}
	}
	return v, true
}

// dedupe убирает повторы, сохраняя порядок первого появления.
func dedupe(in []string) ([]string, int) {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	dup := 0
	for _, d := range in {
		if seen[d] {
			dup++
			continue
		}
		seen[d] = true
		out = append(out, d)
	}
	return out, dup
}

// orderDomains ставит витринный домен первым, остальные —
// лексикографически. Мутирует и возвращает тот же срез.
func orderDomains(domains []string, showcase string) []string {
	sort.Strings(domains)
	if i := indexOf(domains, showcase); i > 0 {
		copy(domains[1:i+1], domains[0:i])
		domains[0] = showcase
	}
	return domains
}

// heuristicShowcase — детерминированный фолбэк витринного домена:
//  1. "<имя>.com", если такой apex есть;
//  2. apex, у которого первая метка == имя пресета (при нескольких —
//     сначала .com, затем лексикографически);
//  3. apex, чья первая метка — префикс имени пресета (или наоборот);
//  4. лексикографически минимальный apex;
//  5. первый домен в отсортированном списке.
func heuristicShowcase(slug string, domains []string) string {
	base := strings.TrimPrefix(slug, "category-")
	if base == "" {
		base = slug
	}

	var apexes []string
	for _, d := range domains {
		if strings.Count(d, ".") == 1 {
			apexes = append(apexes, d)
		}
	}
	if len(apexes) == 0 {
		if len(domains) == 0 {
			return ""
		}
		return domains[0]
	}
	sort.Strings(apexes)

	if contains(apexes, base+".com") {
		return base + ".com"
	}

	labelMatch := func(d string) bool {
		return d[:strings.Index(d, ".")] == base
	}
	if d := pick(apexes, labelMatch); d != "" {
		return d
	}

	prefixMatch := func(d string) bool {
		label := d[:strings.Index(d, ".")]
		return strings.HasPrefix(label, base) || strings.HasPrefix(base, label)
	}
	if d := pick(apexes, prefixMatch); d != "" {
		return d
	}
	return apexes[0]
}

// pick возвращает первый по приоритету элемент, удовлетворяющий match:
// с TLD .com, иначе лексикографически минимальный.
func pick(sorted []string, match func(string) bool) string {
	for _, d := range sorted {
		if strings.HasSuffix(d, ".com") && match(d) {
			return d
		}
	}
	for _, d := range sorted {
		if match(d) {
			return d
		}
	}
	return ""
}

func displayOrNone(s string) string {
	if s == "" {
		return "нет"
	}
	return s
}

func contains(hay []string, needle string) bool {
	if needle == "" {
		return false
	}
	return indexOf(hay, needle) >= 0
}

func indexOf(hay []string, needle string) int {
	for i, v := range hay {
		if v == needle {
			return i
		}
	}
	return -1
}

// sortAll делает порядок полей-срезов в Stats детерминированным.
func (s *Stats) sortAll() {
	sort.Strings(s.MissingLists)
	sort.Strings(s.Skipped)
	sort.Strings(s.MissingIncludes)
	sort.Strings(s.ShowcaseFallback)
}

// Slugs — список слагов пресетов, которые ParseUpstream может вернуть
// (без rublock). Нужен генератору для чистки устаревших файлов.
func Slugs() []string {
	out := make([]string, 0, len(catalog)+1)
	for _, c := range catalog {
		out = append(out, c.Slug)
	}
	out = append(out, RublockSlug)
	sort.Strings(out)
	return out
}

// MaxDomainsOrLimit — хелпер для тестов и логов.
func MaxDomainsOrLimit(large bool) int {
	if large {
		return 0
	}
	return MaxDomains
}

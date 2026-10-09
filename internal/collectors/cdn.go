package collectors

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/Kfaraon/mikrotik-route-sync/internal/config"
)

// GoogleASN — ASN Google (официальные диапазоны подмешиваются в dynamic
// только для сервисов с этим ASN).
const GoogleASN = 15169

// CDN URL-источники (PROMPT II.2), только IPv4.
// Каталог вынесен в конфиг (config.cdn_sources); здесь — рантайм-снимок,
// который перестраивается при загрузке/перезагрузке конфига (SetCDNSources).
// Примечание: Akamai (AS20940) намеренно исключён — для него используется
// метод "asn".
type cdnSource struct {
	asn  int
	name string
	url  string
	kind string
}

// cdnCatalog — неизменяемый снимок каталога: быстрый lookup по ASN и по
// имени сервиса-алиаса без блокировок.
type cdnCatalog struct {
	byASN     map[int]cdnSource
	byService map[string]cdnSource
}

var cdnCatalogPtr atomic.Pointer[cdnCatalog]

func init() {
	// Дефолтный каталог: работает даже без явного SetCDNSources (тесты, CLI).
	SetCDNSources(config.DefaultCDNSources())
}

// SetCDNSources атомарно публикует новый каталог CDN-источников из конфига.
func SetCDNSources(list []config.CDNSourceConfig) {
	cat := &cdnCatalog{
		byASN:     make(map[int]cdnSource, len(list)),
		byService: make(map[string]cdnSource, len(list)),
	}
	for _, s := range list {
		src := cdnSource{asn: s.ASN, name: s.Name, url: s.URL, kind: s.Parser}
		cat.byASN[s.ASN] = src
		cat.byService[strings.ToLower(strings.TrimSpace(s.Service))] = src
	}
	cdnCatalogPtr.Store(cat)
}

func currentCDNCatalog() *cdnCatalog {
	if cat := cdnCatalogPtr.Load(); cat != nil {
		return cat
	}
	// Страховка: init не отработал (не должно случаться).
	cat := &cdnCatalog{byASN: map[int]cdnSource{}, byService: map[string]cdnSource{}}
	cdnCatalogPtr.Store(cat)
	return cat
}

// CDNSource возвращает название CDN и URL официального списка для ASN
// (используется автоопределением источника в core).
func CDNSource(asn int) (name, url string, ok bool) {
	src, ok := currentCDNCatalog().byASN[asn]
	if !ok {
		return "", "", false
	}
	return src.name, src.url, true
}

// CDNSourceByName возвращает ASN и источник по имени сервиса-алиаса
// (cloudflare/aws/google/fastly). Нужно сервисам без доменов: их ASN
// определяется каталогом, а не резолвом.
func CDNSourceByName(service string) (asn int, name, url string, ok bool) {
	src, ok := currentCDNCatalog().byService[strings.ToLower(strings.TrimSpace(service))]
	if !ok {
		return 0, "", "", false
	}
	return src.asn, src.name, src.url, true
}

// CDNCollector — официальные публичные списки IPv4-диапазонов CDN-провайдеров.
type CDNCollector struct {
	asn  int
	http *HTTP
}

// NewCDNCollector создаёт CDN-коллектор для ASN.
func NewCDNCollector(asn int, h *HTTP) *CDNCollector {
	return &CDNCollector{asn: asn, http: h}
}

func (c *CDNCollector) Name() string { return "cdn" }

func (c *CDNCollector) Collect(ctx context.Context, service string, opts Options) (*Result, error) {
	src, ok := currentCDNCatalog().byASN[c.asn]
	if !ok {
		return nil, fmt.Errorf("no CDN source for ASN %d (use 'asn' method instead)", c.asn)
	}

	lines, err := c.fetchSource(ctx, src.kind, src.url)
	if err != nil {
		return nil, err
	}

	out := filterPrefixes(parsePrefixLines(lines), opts)
	if len(out) == 0 {
		return nil, fmt.Errorf("cdn: no prefixes fetched for AS%d", c.asn)
	}
	return &Result{
		Prefixes: out,
		Source:   src.url,
		Method:   "cdn",
	}, nil
}

// fetchSource парсит конкретный формат CDN-ответа, возвращает строки CIDR.
func (c *CDNCollector) fetchSource(ctx context.Context, kind, u string) ([]string, error) {
	switch kind {
	case "text":
		return c.http.FetchLines(ctx, u)

	case "google":
		var data struct {
			Prefixes []struct {
				IPv4Prefix string `json:"ipv4Prefix"`
			} `json:"prefixes"`
		}
		if err := c.http.FetchJSON(ctx, u, &data); err != nil {
			return nil, err
		}
		var out []string
		for _, p := range data.Prefixes {
			if p.IPv4Prefix != "" {
				out = append(out, p.IPv4Prefix)
			}
		}
		return out, nil

	case "aws":
		// Фильтр только по service=CLOUDFRONT* (PROMPT II.2).
		var data struct {
			Prefixes []struct {
				IPPrefix string `json:"ip_prefix"`
				Service  string `json:"service"`
				Region   string `json:"region"`
			} `json:"prefixes"`
		}
		if err := c.http.FetchJSON(ctx, u, &data); err != nil {
			return nil, err
		}
		var out []string
		for _, p := range data.Prefixes {
			if !strings.HasPrefix(strings.ToUpper(p.Service), "CLOUDFRONT") {
				continue
			}
			if p.IPPrefix != "" {
				out = append(out, p.IPPrefix)
			}
		}
		return out, nil

	case "fastly":
		var data struct {
			Prefixes []string `json:"prefixes"`
			IPv4     []string `json:"addresses"`
		}
		if err := c.http.FetchJSON(ctx, u, &data); err != nil {
			return nil, err
		}
		if len(data.Prefixes) > 0 {
			return data.Prefixes, nil
		}
		return data.IPv4, nil
	}

	return nil, fmt.Errorf("cdn: unknown source kind %q", kind)
}

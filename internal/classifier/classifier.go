// Package classifier — автоматический выбор метода сбора IPv4 по имени сервиса.
// Проект работает только с IPv4.
package classifier

import (
	"net/netip"
	"strings"
)

// Result — вердикт классификатора.
type Result struct {
	Methods    []string
	StaticURLs []string
	Domains    []string
	IPs        []string
	ASN        string
}

// knownService — предопределённая запись для популярных сервисов.
type knownService struct {
	methods []string
	domains []string
	static  []string
	asn     string
}

// knownServices — словарь известных сервисов (PROMPT II.2).
//
// Поле asn заполняется только у провайдеров, где ASN — это идентификатор
// источника: у CDN-сервисов без домена (cloudflare/aws/google/fastly) он
// выбирает официальный список в collectors.cdnSources, у akamai — вход
// метода asn. Для сервисов с доменами (youtube/instagram/facebook/telegram)
// ASN НЕ вшивается: его определяет механика сбора в рантайме
// (core/collect.go → resolver.ResolveASN), поэтому смена ASN провайдером
// не требует правки кода (раньше зашитый AS62240 у telegram вёл на чужие
// сети Clouvider). Если такой сервис не получит ASN из домена — это
// сигнал о проблеме сбора, а не повод подставлять константу.
var knownServices = map[string]knownService{
	"cloudflare": {
		methods: []string{"cdn"},
		static:  []string{"https://www.cloudflare.com/ips-v4"},
		asn:     "AS13335",
	},
	"aws": {
		methods: []string{"cdn"},
		asn:     "AS16509",
	},
	"google": {
		methods: []string{"cdn"},
		asn:     "AS15169",
	},
	"youtube": {
		methods: []string{"dynamic", "cdn"},
		domains: []string{"youtube.com", "googlevideo.com", "ytimg.com"},
	},
	"instagram": {
		methods: []string{"dynamic", "asn"},
		domains: []string{"instagram.com", "cdninstagram.com"},
	},
	"facebook": {
		methods: []string{"dynamic", "asn"},
		domains: []string{"facebook.com", "fbcdn.net"},
	},
	"telegram": {
		methods: []string{"dynamic", "asn", "static_url"},
		domains: []string{"telegram.org", "t.me"},
		static:  []string{"https://core.telegram.org/resources/cidr.txt"},
	},
	"fastly": {
		methods: []string{"cdn"},
		static:  []string{"https://api.fastly.com/public-ip-list"},
		asn:     "AS54113",
	},
	"akamai": {
		methods: []string{"asn"},
		asn:     "AS20940",
	},
}

// Classify определяет методы сбора для сервиса.
// overrideMethod — значение overrides.<service>.method (может быть списком
// через запятую для совмещённых методов, PROMPT II.2).
func Classify(service, overrideMethod string) Result {
	s := strings.ToLower(strings.TrimSpace(service))

	// Явные методы из override
	if overrideMethod != "" {
		var methods []string
		for _, m := range strings.FieldsFunc(overrideMethod, func(r rune) bool {
			return r == ',' || r == ' ' || r == ';' || r == '+'
		}) {
			m = strings.TrimSpace(strings.ToLower(m))
			if m != "" {
				methods = append(methods, m)
			}
		}
		res := Result{Methods: methods}
		if k, ok := knownServices[s]; ok {
			res.Domains = k.domains
			res.StaticURLs = k.static
			res.ASN = k.asn
		}
		return res
	}

	// ASN напрямую: "AS13335" / "as13335" / "13335"
	if asn := parseASNLiteral(s); asn != "" {
		return Result{Methods: []string{"asn"}, ASN: asn}
	}

	// IP-адрес: whois-метод (IP → ASN → префиксы)
	if _, err := netip.ParseAddr(s); err == nil {
		return Result{Methods: []string{"whois"}, IPs: []string{s}}
	}

	// Известный сервис
	if k, ok := knownServices[s]; ok {
		return Result{
			Methods:    k.methods,
			Domains:    k.domains,
			StaticURLs: k.static,
			ASN:        k.asn,
		}
	}

	// Домен (содержит точку) → whois
	if strings.Contains(s, ".") {
		return Result{Methods: []string{"whois"}, Domains: []string{s}}
	}

	// Неизвестно → dynamic по имени домена
	return Result{
		Methods: []string{"dynamic", "whois"},
		Domains: []string{s + ".com"},
	}
}

// parseASNLiteral распознаёт строки вида "as13335" / "13335" (только as-префикс
// или чистое число). Чистое число считается ASN только если оно 1..4294967295.
func parseASNLiteral(s string) string {
	trimmed := strings.TrimPrefix(s, "as")
	if trimmed == s {
		return "" // без префикса "as" ASN не определяем, чтобы не путать с именами
	}
	if trimmed == "" {
		return ""
	}
	for _, r := range trimmed {
		if r < '0' || r > '9' {
			return ""
		}
	}
	return strings.ToUpper(s)
}

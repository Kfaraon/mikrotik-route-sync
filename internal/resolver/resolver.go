package resolver

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
)

// Fetcher — минимальный интерфейс HTTP-загрузки (реализуется collectors.HTTP).
// Определяется здесь, чтобы resolver не зависел от пакета collectors
// (предотвращение цикла импортов).
type Fetcher interface {
	Fetch(ctx context.Context, url string) ([]byte, error)
}

// Cache — кэш результатов резолвинга (реализуется storage.Cache).
type Cache interface {
	GetASN(ip string) (int, bool)
	SetASN(ip string, asn int, ttl time.Duration) error
}

// Resolver резолвит домены и определяет их ASN.
type Resolver struct {
	dnsResolver string
	fallbacks   []string
	fetch       Fetcher
	cache       Cache
	ttl         time.Duration
}

// DNSFallbacks — публичные DNS на случай недоступности основного резолвера.
var DNSFallbacks = []string{"8.8.8.8:53", "1.1.1.1:53"}

// NewResolver создаёт резолвер.
// dnsResolver — адрес вида "1.1.1.1:53" (пусто — системный DNS).
// fetch/cache могут быть nil (тогда без внешних проверок и кэша).
func NewResolver(dnsResolver string, fetch Fetcher, cache Cache, ttl time.Duration) *Resolver {
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	return &Resolver{
		dnsResolver: dnsResolver,
		fallbacks:   append([]string(nil), DNSFallbacks...),
		fetch:       fetch,
		cache:       cache,
		ttl:         ttl,
	}
}

// WithFallbacks задаёт резервные DNS (пустой список — дефолтные публичные).
// Дубли основного резолвера отбрасываются.
func (r *Resolver) WithFallbacks(fb ...string) *Resolver {
	if len(fb) == 0 {
		fb = DNSFallbacks
	}
	out := make([]string, 0, len(fb))
	for _, f := range fb {
		f = strings.TrimSpace(f)
		if f == "" || f == r.dnsResolver {
			continue
		}
		out = append(out, f)
	}
	r.fallbacks = out
	return r
}

// ParseASN парсит строку вида "AS12345" или "12345" в числовой ASN.
func ParseASN(asn string) (int, error) {
	asn = strings.TrimSpace(asn)
	asn = strings.TrimPrefix(strings.ToUpper(asn), "AS")
	n, err := strconv.Atoi(asn)
	if err != nil || n <= 0 || n > 4294967295 {
		return 0, fmt.Errorf("invalid ASN %q", asn)
	}
	return n, nil
}

// ResolveDomain резолвит домен (A-записи) и возвращает IPv4-адреса.
// IPv6 не поддерживается проектом.
func (r *Resolver) ResolveDomain(ctx context.Context, domain string) ([]string, error) {
	ips, err := r.lookupIP(ctx, domain)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", domain, err)
	}
	out := make([]string, 0, len(ips))
	for _, ip := range ips {
		if v4 := ip.To4(); v4 != nil {
			out = append(out, v4.String())
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no IPv4 addresses found for %s", domain)
	}
	return out, nil
}

// ResolveASN определяет IPv4-ASN для домена через DNS + IP→ASN.
// Эвристика при неопределённости — ASN с наибольшим количеством IP.
func (r *Resolver) ResolveASN(ctx context.Context, domain string) (int, error) {
	ips, err := r.ResolveDomain(ctx, domain)
	if err != nil {
		return 0, err
	}

	asnCount := make(map[int]int)
	for _, ipStr := range ips {
		asn, err := r.ASNByIP(ctx, ipStr)
		if err != nil {
			continue
		}
		asnCount[asn]++
	}

	bestASN, maxCount := 0, 0
	for asn, count := range asnCount {
		if count > maxCount {
			bestASN = asn
			maxCount = count
		}
	}
	if bestASN == 0 {
		return 0, fmt.Errorf("cannot determine ASN for %s", domain)
	}
	return bestASN, nil
}

// ASNByIP определяет ASN для IPv4-адреса через DNS-запрос к Team Cymru
// (origin.asn.cymru.com) с кэшированием. IPv6 не поддерживается.
func (r *Resolver) ASNByIP(ctx context.Context, ipStr string) (int, error) {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return 0, fmt.Errorf("invalid IP %q", ipStr)
	}
	v4 := ip.To4()
	if v4 == nil {
		return 0, fmt.Errorf("IPv6 is not supported: %q", ipStr)
	}

	if r.cache != nil {
		if asn, ok := r.cache.GetASN(ipStr); ok && asn > 0 {
			return asn, nil
		}
	}

	parts := strings.Split(v4.String(), ".")
	query := strings.Join([]string{parts[3], parts[2], parts[1], parts[0]}, ".") + ".origin.asn.cymru.com"

	answers, err := r.lookupTXT(ctx, query)
	if err != nil || len(answers) == 0 {
		return 0, fmt.Errorf("no ASN found for %s", ipStr)
	}

	// Ответ: "12345 | 1.2.3.0/24 | US | arin | 2023-01-01"
	asnStr := strings.TrimSpace(strings.Split(answers[0], "|")[0])
	asn, err := strconv.Atoi(asnStr)
	if err != nil || asn <= 0 {
		return 0, fmt.Errorf("parse ASN %q: invalid", asnStr)
	}

	if r.cache != nil {
		_ = r.cache.SetASN(ipStr, asn, r.ttl)
	}
	return asn, nil
}

// lookupTXT выполняет TXT-запрос через настроенный резолвер (или системный).
func (r *Resolver) lookupTXT(ctx context.Context, name string) ([]string, error) {
	if r.dnsResolver == "" && len(r.fallbacks) == 0 {
		return net.DefaultResolver.LookupTXT(ctx, name)
	}
	return lookupWithFallback(ctx, r.resolvers(), func(ctx context.Context, res *net.Resolver) ([]string, error) {
		return res.LookupTXT(ctx, name)
	})
}

// lookupIP резолвит домен, используя указанный DNS-резолвер.
func (r *Resolver) lookupIP(ctx context.Context, domain string) ([]net.IP, error) {
	if r.dnsResolver == "" && len(r.fallbacks) == 0 {
		return net.LookupIP(domain)
	}

	addr, err := lookupWithFallback(ctx, r.resolvers(), func(ctx context.Context, res *net.Resolver) ([]net.IPAddr, error) {
		return res.LookupIPAddr(ctx, domain)
	})
	if err != nil {
		return nil, err
	}

	out := make([]net.IP, 0, len(addr))
	for _, ip := range addr {
		out = append(out, ip.IP)
	}
	return out, nil
}

// LookupCNAME возвращает каноническое имя домена (идёт по CNAME-цепочке).
// Используется диагностикой; IP-резолв (lookupIP) цепочку проходит сам.
func (r *Resolver) LookupCNAME(ctx context.Context, domain string) (string, error) {
	if r.dnsResolver == "" && len(r.fallbacks) == 0 {
		return net.DefaultResolver.LookupCNAME(ctx, domain)
	}
	return lookupWithFallback(ctx, r.resolvers(), func(ctx context.Context, res *net.Resolver) (string, error) {
		return res.LookupCNAME(ctx, domain)
	})
}

// resolvers — цепочка резолверов: основной (или системный), затем резервные.
// nil в списке означает системный резолвер по умолчанию.
func (r *Resolver) resolvers() []*net.Resolver {
	out := make([]*net.Resolver, 0, len(r.fallbacks)+1)
	if r.dnsResolver == "" {
		out = append(out, nil)
	} else {
		out = append(out, r.resolverFor(r.dnsResolver))
	}
	for _, fb := range r.fallbacks {
		out = append(out, r.resolverFor(fb))
	}
	return out
}

// lookupWithFallback пробует резолверы по очереди, возвращает первый успех.
// Контекстная отмена прерывает перебор сразу.
func lookupWithFallback[T any](ctx context.Context, rss []*net.Resolver, fn func(context.Context, *net.Resolver) (T, error)) (T, error) {
	var zero T
	var lastErr error
	for _, res := range rss {
		if res == nil {
			res = net.DefaultResolver
		}
		out, err := fn(ctx, res)
		if err == nil {
			return out, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return zero, ctx.Err()
		}
	}
	return zero, lastErr
}

// resolverFor создаёт net.Resolver с кастомным сервером.
func (r *Resolver) resolverFor(addr string) *net.Resolver {
	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			d := net.Dialer{Timeout: 10 * time.Second}
			return d.DialContext(ctx, network, addr)
		},
	}
}

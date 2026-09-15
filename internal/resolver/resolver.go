package resolver

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/Kfaraon/mikrotik-route-sync/internal/collectors"
)

// Resolver резолвит домены и определяет их ASN.
type Resolver struct {
	dnsResolver string
	http        *collectors.HTTP
}

// NewResolver создаёт резолвер.
func NewResolver(dnsResolver string, http *collectors.HTTP) *Resolver {
	return &Resolver{
		dnsResolver: dnsResolver,
		http:        http,
	}
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

// ResolveASN определяет ASN для домена через DNS + WHOIS/RDAP.
//
// Алгоритм:
// 1. Резолвим домен (A + AAAA)
// 2. Для каждого IP определяем ASN
// 3. Возвращаем наиболее частый ASN (эвристика)
func (r *Resolver) ResolveASN(ctx context.Context, domain string) (int, error) {
	ips, err := r.lookupIP(ctx, domain)
	if err != nil {
		return 0, fmt.Errorf("resolve %s: %w", domain, err)
	}
	if len(ips) == 0 {
		return 0, fmt.Errorf("no IPs found for %s", domain)
	}

	// Считаем ASN для каждого IP
	asnCount := make(map[int]int)
	for _, ip := range ips {
		asn, err := r.ipToASN(ctx, ip)
		if err != nil {
			continue
		}
		asnCount[asn]++
	}

	// Возвращаем ASN с максимальным количеством
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

// GetASPrefixes возвращает анонсированные префиксы для ASN.
//
// Использует BGPView как основной источник,
// с автоматическим fallback на RIPEstat при ошибке.
func (r *Resolver) GetASPrefixes(ctx context.Context, asn int) ([]netip.Prefix, error) {
	// Используем ASNCollector с автоматическим fallback
	collector := collectors.NewASNCollector(asn, r.http)
	result, err := collector.Collect(ctx, "", collectors.Options{})
	if err != nil {
		return nil, fmt.Errorf("get prefixes for AS%d: %w", asn, err)
	}

	return result.Prefixes, nil
}

// lookupIP резолвит домен, используя указанный DNS-резолвер.
func (r *Resolver) lookupIP(ctx context.Context, domain string) ([]net.IP, error) {
	if r.dnsResolver == "" {
		return net.LookupIP(domain)
	}

	// Кастомный DNS-резолвер
	resolver := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			d := net.Dialer{Timeout: 10 * time.Second}
			return d.DialContext(ctx, network, r.dnsResolver)
		},
	}

	ips, err := resolver.LookupIPAddr(ctx, domain)
	if err != nil {
		return nil, err
	}

	out := make([]net.IP, 0, len(ips))
	for _, ip := range ips {
		out = append(out, ip.IP)
	}

	return out, nil
}

// ipToASN определяет ASN для IP-адреса.
//
// Использует DNS-запрос к whois-серверам (упрощённо).
// В продакшене лучше использовать RDAP API.
func (r *Resolver) ipToASN(ctx context.Context, ip net.IP) (int, error) {
	// Пример для IPv4: запрос к Cymru
	var query string
	if ip.To4() != nil {
		// IPv4: обратный порядок
		parts := strings.Split(ip.String(), ".")
		query = strings.Join([]string{parts[3], parts[2], parts[1], parts[0]}, ".") + ".origin.asn.cymru.com"
	} else {
		// IPv6: каждый ниббл в обратном порядке
		query = reverseIPv6(ip) + ".origin6.asn.cymru.com"
	}

	answers, err := net.DefaultResolver.LookupTXT(ctx, query)
	if err != nil || len(answers) == 0 {
		return 0, fmt.Errorf("no ASN found for %s", ip)
	}

	// Ответ: "12345 | 1.2.3.0/24 | US | arin | 2023-01-01"
	asnStr := strings.TrimSpace(strings.Split(answers[0], "|")[0])
	asn, err := strconv.Atoi(asnStr)
	if err != nil {
		return 0, fmt.Errorf("parse ASN %q: %w", asnStr, err)
	}

	return asn, nil
}

// reverseIPv6 преобразует IPv6-адрес в обратный формат для DNS.
func reverseIPv6(ip net.IP) string {
	ip = ip.To16()
	if ip == nil {
		return ""
	}

	buf := make([]byte, 0, len(ip)*4)
	for i := len(ip) - 1; i >= 0; i-- {
		buf = append(buf, hexDigit(ip[i]&0x0f), '.', hexDigit(ip[i]>>4), '.')
	}

	return string(buf[:len(buf)-1])
}

func hexDigit(b byte) byte {
	if b < 10 {
		return '0' + b
	}
	return 'a' + b - 10
}

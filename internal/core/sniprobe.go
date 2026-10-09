package core

// SNI-проба: доказывает, что адреса реально обслуживают домен сервиса.
// После сбора делаем TLS-handshake на 443 с SNI=домен (только ClientHello,
// без HTTP-запросов): успех с валидным сертификатом = адрес обслуживает
// домен = сервис заработает через VPN.
//
// Результат — варнинг, а не блок: некоторые CDN рвут прямые handshake
// без правильного маршрута, возможны ложные срабатывания. Проверка
// best-effort, бюджет — единицы секунд на сервис (параллельно).

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"
)

// SNIVerdict — итог пробы по одному домену.
type SNIVerdict struct {
	Domain  string   `json:"domain"`
	Checked int      `json:"checked"`
	OK      int      `json:"ok"`
	Failed  []string `json:"failed,omitempty"`
}

// Passed — все проверенные адреса обслуживают домен.
// Пустой вердикт (нечего проверять) считается пройденным, чтобы не
// дублировать сигналы покрытия.
func (v *SNIVerdict) Passed() bool {
	return v == nil || len(v.Failed) == 0
}

// SNIVerifier — параметры SNI-пробы.
type SNIVerifier struct {
	// Timeout — бюджет на один IP (дефолт 5s).
	Timeout time.Duration
	// Roots — доверенные CA (nil — системные). Поле для тестов.
	Roots *x509.CertPool
	// Dial — подмена dial для тестов (nil — обычный TCP-dial).
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
}

// Verify выполняет параллельные handshake на 443 для всех IP.
func (v *SNIVerifier) Verify(ctx context.Context, domain string, ips []string) *SNIVerdict {
	vd := &SNIVerdict{Domain: domain}
	timeout := v.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	// Лимит одновременных handshake: по умолчанию 10, можно переопределить через env или конфиг.
	// Используем буферизированный канал как семафор — проще и быстрее errgroup.
	const maxConcurrentSNIPprobes = 10
	sem := make(chan struct{}, maxConcurrentSNIPprobes)

	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, ip := range ips {
		ip := ip
		vd.Checked++
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			if err := v.probeIP(ctx, domain, ip, timeout); err != nil {
				mu.Lock()
				vd.Failed = append(vd.Failed, ip)
				mu.Unlock()
				return
			}
			mu.Lock()
			vd.OK++
			mu.Unlock()
		}()
	}
	wg.Wait()
	return vd
}

// probeIP — один handshake: TCP + TLS с SNI=домен и проверкой сертификата
// на этот домен. Ошибки на английском (код/логи), наружу уходят только IP.
func (v *SNIVerifier) probeIP(ctx context.Context, domain, ip string, timeout time.Duration) error {
	pctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	dial := v.Dial
	if dial == nil {
		dial = (&net.Dialer{Timeout: timeout}).DialContext
	}
	conn, err := dial(pctx, "tcp", net.JoinHostPort(ip, "443"))
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	if dc, ok := conn.(interface{ SetDeadline(time.Time) error }); ok {
		_ = dc.SetDeadline(time.Now().Add(timeout))
	}
	tc := tls.Client(conn, &tls.Config{
		ServerName: domain,
		RootCAs:    v.Roots,
		MinVersion: tls.VersionTLS12,
	})
	if err := tc.HandshakeContext(pctx); err != nil {
		return err
	}
	_ = tc.Close()
	return nil
}

// sniCacheEntry — кэшированные вердикты домена (позитив — час, негатив —
// 10 минут, чтобы не долбить недоступные адреса на каждом синке).
type sniCacheEntry struct {
	verdicts []*SNIVerdict
	expires  time.Time
}

const (
	sniCachePositiveTTL = time.Hour
	sniCacheNegativeTTL = 10 * time.Minute
	// sniAlertThrottle — алерт о провале SNI не чаще раза в сутки на сервис.
	sniAlertThrottle = 24 * time.Hour
)

// verifyServiceSNI — SNI-проба доменов сервиса с кэшем. Домены без резолва
// пропускаются молча (о них уже сигналит контроль покрытия).
func (s *Syncer) verifyServiceSNI(ctx context.Context, domains []string) []*SNIVerdict {
	var out []*SNIVerdict
	v := &SNIVerifier{}
	for _, d := range domains {
		if ctx.Err() != nil {
			break
		}
		s.sniMu.Lock()
		e, ok := s.sniCache[d]
		s.sniMu.Unlock()
		if ok && time.Now().Before(e.expires) {
			out = append(out, e.verdicts...)
			continue
		}
		var vds []*SNIVerdict
		if ips, err := s.resolver().ResolveDomain(ctx, d); err == nil && len(ips) > 0 {
			vds = []*SNIVerdict{v.Verify(ctx, d, ips)}
		}
		ttl := sniCachePositiveTTL
		if !allSNIPassed(vds) {
			ttl = sniCacheNegativeTTL
		}
		s.sniMu.Lock()
		s.sniCache[d] = sniCacheEntry{verdicts: vds, expires: time.Now().Add(ttl)}
		s.sniMu.Unlock()
		out = append(out, vds...)
	}
	return out
}

func allSNIPassed(vds []*SNIVerdict) bool {
	for _, v := range vds {
		if !v.Passed() {
			return false
		}
	}
	return true
}

// sniFailedDomains — непройденные вердикты с проверенными адресами.
func sniFailedDomains(vds []*SNIVerdict) []*SNIVerdict {
	var out []*SNIVerdict
	for _, v := range vds {
		if v != nil && !v.Passed() && v.Checked > 0 {
			out = append(out, v)
		}
	}
	return out
}

// primarySNIFailed — провал primary-домена (первый проверяемый в списке:
// домен самого сервиса, остальные — aux-инфраструктура вида googlevideo.com
// с частыми ложными срабатываниями). Только primary будит warn-лог и
// Telegram-алерт; aux-провалы остаются видимыми в Verification (API/UI),
// но не шумят. Пустые вердикты (нечего проверять) пропускаются — primary
// определяется по первому реально проверенному домену.
func primarySNIFailed(vds []*SNIVerdict) []*SNIVerdict {
	for _, v := range vds {
		if v == nil || v.Checked == 0 {
			continue
		}
		if !v.Passed() {
			return []*SNIVerdict{v}
		}
		return nil
	}
	return nil
}

// sniAlertMessage — Telegram-алерт о провале SNI-пробы (русский).
func sniAlertMessage(name string, failed []*SNIVerdict) string {
	parts := make([]string, 0, len(failed))
	for _, v := range failed {
		parts = append(parts, fmt.Sprintf("%s: %s", v.Domain, strings.Join(v.Failed, ", ")))
	}
	return fmt.Sprintf("%s: SNI-проба провалена — адреса не обслуживают домен, "+
		"сервис может не работать через VPN: %s", name, strings.Join(parts, "; "))
}

// maybeAlertSNI — троттled алерт о провале SNI (не чаще раза в сутки).
func (s *Syncer) maybeAlertSNI(ctx context.Context, name string, vds []*SNIVerdict) {
	if len(sniFailedDomains(vds)) == 0 {
		return
	}
	now := time.Now()
	s.sniMu.Lock()
	if last, ok := s.sniAlerted[name]; ok && now.Sub(last) < sniAlertThrottle {
		s.sniMu.Unlock()
		return
	}
	s.sniMu.Unlock()
	// H10: метку ставим ТОЛЬКО после успешной отправки — иначе сбой Send
	// подавлял бы повторный алерт на сутки (проблема оставалась незамеченной).
	if n := s.getNotify(); n != nil {
		if err := n.Send(context.WithoutCancel(ctx), sniAlertMessage(name, sniFailedDomains(vds))); err != nil {
			s.log.Warn("telegram sni alert not delivered", "service", name, "err", err)
			return
		}
		s.sniMu.Lock()
		s.sniAlerted[name] = now
		s.sniMu.Unlock()
	}
}

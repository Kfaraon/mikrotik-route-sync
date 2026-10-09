// Package web — встроенный Web UI + REST API + WebSocket (PROMPT VI, VII).
//
// Безопасность:
//   - Basic Auth + session cookie + CSRF-токен для изменяющих запросов
//   - allowed_cidrs для всех входящих, кромe /healthz
//   - Security headers (CSP, X-Frame-Options: DENY, nosniff, HSTS при HTTPS)
//   - rate limiting (собственная реализация на golang.org/x/time/rate)
//   - ограничение тела запроса 1 МБ
package web

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Kfaraon/mikrotik-route-sync/internal/addresslist"
	"github.com/Kfaraon/mikrotik-route-sync/internal/config"
	"github.com/Kfaraon/mikrotik-route-sync/internal/core"
	"github.com/Kfaraon/mikrotik-route-sync/internal/logging"
	"github.com/Kfaraon/mikrotik-route-sync/internal/notifier"
	"github.com/Kfaraon/mikrotik-route-sync/internal/presets"
	"github.com/Kfaraon/mikrotik-route-sync/internal/scheduler"
	"github.com/Kfaraon/mikrotik-route-sync/internal/version"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/gorilla/websocket"
	"golang.org/x/time/rate"
	"gopkg.in/yaml.v3"
)

//go:embed templates/* static/*
var webFS embed.FS

// ============================================================================
// WebSocket Hub
// ============================================================================

type wsClient struct {
	conn *websocket.Conn
	send chan []byte
}

func (c *wsClient) writeLoop(done chan struct{}) {
	for {
		select {
		case msg, ok := <-c.send:
			if !ok {
				_ = c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			_ = c.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if err := c.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				return
			}
		case <-done:
			return
		}
	}
}

type Hub struct {
	mu         sync.RWMutex
	clients    map[*wsClient]bool
	reaperDone chan struct{}
}

func NewHub() *Hub {
	h := &Hub{clients: map[*wsClient]bool{}, reaperDone: make(chan struct{})}
	go h.reaperLoop()
	return h
}

func (h *Hub) Stop() {
	close(h.reaperDone)
}

// reaperLoop периодически чистит мёртвых клиентов: тех, у кого send канал
// забит (backpressure) или соединение закрыто. Запускается каждые 30 сек.
func (h *Hub) reaperLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-h.reaperDone:
			return
		case <-ticker.C:
			h.mu.Lock()
			for c := range h.clients {
				// Канал забит под завязку — клиент не успевает читать,
				// удаляем. Проверяем длину, ничего не пишем: лишние
				// фреймы живым клиентам не нужны.
				if len(c.send) == cap(c.send) {
					delete(h.clients, c)
					close(c.send)
					_ = c.conn.Close()
				}
			}
			h.mu.Unlock()
		}
	}
}

func (h *Hub) Broadcast(event string, data any) {
	payload, err := json.Marshal(map[string]any{"event": event, "data": data, "time": time.Now().Format(time.RFC3339)})
	if err != nil {
		return
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	for c := range h.clients {
		select {
		case c.send <- payload:
		default:
			// Клиент не успевает читать — помечаем на удаление репером.
			// Не удаляем здесь под RLock, чтобы не удерживать блокировку.
		}
	}
}

var wsUpgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool {
		return checkOriginHost(r, r.Header.Get("Origin"))
	},
}

// checkOriginHost строго сравнивает Origin с Host (без префиксных
// совпадений вида http://host.evil.com). Пустой Origin — не браузер.
// Схему здесь не смотрим: её проверяет Server.handleWS до апгрейда.
func checkOriginHost(r *http.Request, origin string) bool {
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

// sameOrigin — то же плюс сверка схемы (за TLS-терминирующим прокси
// смотрим X-Forwarded-Proto от доверенного).
func (s *Server) sameOrigin(r *http.Request, origin string) bool {
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	if !strings.EqualFold(u.Host, r.Host) {
		return false
	}
	wantHTTPS := s.httpsNow(r)
	if u.Scheme == "https" && !wantHTTPS {
		return false
	}
	if u.Scheme == "http" && wantHTTPS {
		return false
	}
	return true
}

// handleWS — апгрейд с проверкой Origin (включая схему), затем хаб.
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	if !s.sameOrigin(r, r.Header.Get("Origin")) {
		http.Error(w, "cross-origin websocket rejected", http.StatusForbidden)
		return
	}
	s.hub.handleWS(w, r)
}

func (h *Hub) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	client := &wsClient{conn: conn, send: make(chan []byte, 64)}
	done := make(chan struct{})

	h.mu.Lock()
	h.clients[client] = true
	h.mu.Unlock()

	go client.writeLoop(done)

	welcome, _ := json.Marshal(map[string]any{"event": "welcome", "time": time.Now()})
	client.send <- welcome

	go func() {
		defer close(done)
		conn.SetReadLimit(4096)
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				break
			}
		}
	}()

	<-done
	h.mu.Lock()
	delete(h.clients, client)
	h.mu.Unlock()
	_ = conn.Close()
}

// ============================================================================
// Sessions & CSRF
// ============================================================================

type session struct {
	username string
	csrf     string
	expires  time.Time
}

type sessionStore struct {
	mu   sync.RWMutex
	data map[string]*session
	ttl  time.Duration
	stop chan struct{}
}

func newSessionStore(ttl time.Duration) *sessionStore {
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	s := &sessionStore{data: map[string]*session{}, ttl: ttl, stop: make(chan struct{})}
	go s.gcLoop()
	return s
}

// Stop останавливает фоновую чистку (вызывается при shutdown сервера,
// иначе time.Ticker и горутина живут дольше процесса-владельца).
func (st *sessionStore) Stop() {
	select {
	case <-st.stop:
	default:
		close(st.stop)
	}
}

func (st *sessionStore) gcLoop() {
	t := time.NewTicker(10 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-st.stop:
			return
		case now := <-t.C:
			st.mu.Lock()
			for k, v := range st.data {
				if now.After(v.expires) {
					delete(st.data, k)
				}
			}
			st.mu.Unlock()
		}
	}
}

var hexFallbackCounter atomic.Uint64

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// rand.Read на Linux практически не отказывает; fallback обязан
		// оставаться уникальным (повтор session ID перезатрёт чужую сессию),
		// непредсказуемость тут уже не гарантируется.
		return strconv.FormatInt(time.Now().UnixNano(), 16) +
			"-" + strconv.FormatUint(hexFallbackCounter.Add(1), 16)
	}
	return hex.EncodeToString(b)
}

// maxSessions — предел записей: клиенты без cookie (curl, мониторинг)
// создавали бы новую сессию на каждый запрос без ограничений.
const maxSessions = 10000

// evictLocked вычищает просроченные, а при переполнении — лишние.
// Вызывать под st.mu.
func (st *sessionStore) evictLocked() {
	if len(st.data) < maxSessions {
		return
	}
	now := time.Now()
	for k, v := range st.data {
		if now.After(v.expires) {
			delete(st.data, k)
		}
	}
	for k := range st.data {
		if len(st.data) < maxSessions {
			break
		}
		delete(st.data, k)
	}
}

// reuseOrCreate возвращает живую сессию пользователя вместо новой:
// клиенты с Basic-auth без cookie не плодят записи на каждый запрос.
func (st *sessionStore) reuseOrCreate(username string) (string, *session) {
	st.mu.Lock()
	defer st.mu.Unlock()
	now := time.Now()
	for id, s := range st.data {
		if s.username == username && now.Before(s.expires) {
			return id, s
		}
	}
	id := randomHex(16)
	s := &session{username: username, csrf: randomHex(16), expires: now.Add(st.ttl)}
	st.data[id] = s
	st.evictLocked()
	return id, s
}

func (st *sessionStore) get(id string) (*session, bool) {
	st.mu.RLock()
	defer st.mu.RUnlock()
	s, ok := st.data[id]
	if !ok || time.Now().After(s.expires) {
		return nil, false
	}
	return s, true
}

// ============================================================================
// IP allow list & rate limiting
// ============================================================================

type ipChecker struct {
	nets           []*net.IPNet
	trustedProxies []*net.IPNet
	enabled        bool
}

func newIPChecker(cfg *config.WebConfig) (*ipChecker, error) {
	c := &ipChecker{enabled: len(cfg.AllowedCIDRs) > 0}
	for _, s := range cfg.AllowedCIDRs {
		_, n, err := net.ParseCIDR(s)
		if err != nil {
			return nil, fmt.Errorf("allowed_cidrs %q: %w", s, err)
		}
		c.nets = append(c.nets, n)
	}
	for _, s := range cfg.TrustedProxies {
		_, n, err := net.ParseCIDR(s)
		if err != nil {
			return nil, fmt.Errorf("trusted_proxies %q: %w", s, err)
		}
		c.trustedProxies = append(c.trustedProxies, n)
	}
	return c, nil
}

func (c *ipChecker) allowedIP(ip net.IP) bool {
	for _, n := range c.nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

func (c *ipChecker) clientIP(r *http.Request) net.IP {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return nil
	}
	for _, tp := range c.trustedProxies {
		if tp.Contains(ip) {
			if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
				// Справа налево до первого недоверенного: каждый прокси
				// дописывает адрес в конец, левые элементы клиент подделывает.
				// Если вся цепочка доверенная — верим самому левому.
				parts := strings.Split(xff, ",")
				leftmost := net.ParseIP(strings.TrimSpace(parts[0]))
				for i := len(parts) - 1; i >= 0; i-- {
					p := net.ParseIP(strings.TrimSpace(parts[i]))
					if p == nil {
						continue
					}
					untrusted := true
					for _, inner := range c.trustedProxies {
						if inner.Contains(p) {
							untrusted = false
							break
						}
					}
					if untrusted {
						return p
					}
				}
				if leftmost != nil {
					return leftmost
				}
			}
			break
		}
	}
	return ip
}

type ipLimiter struct {
	mu          sync.Mutex
	limiters    map[string]*ipEntry
	rate        rate.Limit
	burst       int
	evictTicker *time.Ticker
	evictDone   chan struct{}
}

type ipEntry struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

// maxLimiters — cap карты: без эвикции каждый уникальный IP (спуф XFF
// за allowed_cidrs не проходит, но лимит всё равно нужен) рос бы вечно.
const maxLimiters = 4096

// limiterTTL — записи без обращений дольше TTL вычищаются при переполнении.
const limiterTTL = 10 * time.Minute

func newIPLimiter(rps int) *ipLimiter {
	if rps <= 0 {
		rps = 50
	}
	l := &ipLimiter{limiters: map[string]*ipEntry{}, rate: rate.Limit(rps), burst: rps * 2}
	l.StartEvictTicker(5 * time.Minute)
	return l
}

func (l *ipLimiter) allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e, ok := l.limiters[ip]; ok {
		e.lastSeen = time.Now()
		return e.limiter.Allow()
	}
	if len(l.limiters) >= maxLimiters {
		l.evictLocked()
	}
	l.limiters[ip] = &ipEntry{limiter: rate.NewLimiter(l.rate, l.burst), lastSeen: time.Now()}
	return l.limiters[ip].limiter.Allow()
}

// evictLocked вычищает давно не обращавшиеся записи; если все свежи
// (флуд уникальными IP) — режет произвольно до половины cap.
func (l *ipLimiter) evictLocked() {
	now := time.Now()
	for ip, e := range l.limiters {
		if now.Sub(e.lastSeen) > limiterTTL {
			delete(l.limiters, ip)
		}
	}
	if len(l.limiters) >= maxLimiters {
		n := 0
		for ip := range l.limiters {
			delete(l.limiters, ip)
			if n++; n >= maxLimiters/2 {
				break
			}
		}
	}
}

// StartEvictTicker запускает периодическую чистку неактивных лимитеров.
// interval — период чистки (рекомендуется 5 мин). Безопасно вызывать многократно:
// предыдущий тикер останавливается, его горутина завершается.
func (l *ipLimiter) StartEvictTicker(interval time.Duration) {
	if interval <= 0 {
		interval = 5 * time.Minute
	}

	l.mu.Lock()
	// Останавливаем предыдущий тикер и будим его горутину (она владеет
	// локальными копиями ticker/done, поэтому читать поля в select нельзя).
	if l.evictTicker != nil {
		l.evictTicker.Stop()
	}
	if l.evictDone != nil {
		close(l.evictDone)
	}
	ticker := time.NewTicker(interval)
	done := make(chan struct{})
	l.evictTicker = ticker
	l.evictDone = done
	l.mu.Unlock()

	go func() {
		for {
			select {
			case <-ticker.C:
				l.mu.Lock()
				l.evictLocked()
				l.mu.Unlock()
			case <-done:
				return
			}
		}
	}()
}

// StopEvictTicker останавливает тикер чистки.
func (l *ipLimiter) StopEvictTicker() {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.evictTicker != nil {
		l.evictTicker.Stop()
		l.evictTicker = nil
	}
	if l.evictDone != nil {
		close(l.evictDone)
		l.evictDone = nil
	}
}

// ============================================================================
// Server
// ============================================================================

type Server struct {
	cfg      *config.Config
	syncer   *core.Syncer
	log      *slog.Logger
	hub      *Hub
	tmpl     *template.Template
	ips      *ipChecker
	limiter  *ipLimiter
	sessions *sessionStore
	httpSrv  *http.Server

	// Кэш состояния MikroTik для live-индикатора в шапке страниц.
	mtMu         sync.Mutex
	mtOK         bool
	mtKnown      bool
	mtAt         time.Time
	mtRefreshing bool
	pingFn       func(context.Context) error // подмена в тестах

	// Дедупликация тяжёлых refresh-запросов (зеркала качают десятки МБ).
	refreshMu sync.Mutex
	refreshOn map[string]bool
}

func NewServer(cfg *config.Config, syncer *core.Syncer, log *slog.Logger) (*Server, error) {
	if log == nil {
		log = slog.Default()
	}

	funcs := template.FuncMap{
		// Обёртка с чтением: прямой вызов в шаблоне без блокировки —
		// concurrent map iteration при параллельной мутации.
		"effectiveSchedule": func(service string) string {
			cfg.RLock()
			defer cfg.RUnlock()
			return cfg.EffectiveSchedule(service)
		},
		"maskSecret": Mask,
	}
	tmpl, err := template.New("").Funcs(funcs).ParseFS(webFS, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}

	ips, err := newIPChecker(&cfg.Web)
	if err != nil {
		return nil, err
	}

	s := &Server{
		cfg:      cfg,
		syncer:   syncer,
		log:      log,
		hub:      NewHub(),
		tmpl:     tmpl,
		ips:      ips,
		limiter:  newIPLimiter(50),
		sessions: newSessionStore(cfg.Web.SessionTimeout.Duration()),
	}

	// Каталог runtime-зеркала пресетов: единая формула MirrorDir.
	if dir := presets.MirrorDir(cfg.CachePath); dir != "" {
		presets.DiskDir = dir
		if err := presets.RebuildUpstreamIndex(presets.DiskDir); err != nil {
			log.Debug("upstream presets index not built (mirror not downloaded?)", "err", err)
		}
	}

	syncer.SetEventHook(func(event string, data any) {
		s.hub.Broadcast(event, data)
	})

	router := chi.NewRouter()
	router.Use(middleware.Recoverer)
	// БЕЗ глобального Compress: /ws апгрейд под gzip держится на
	// недокументированном совпадении (см. handleWS). Сжатие — точечно
	// на страницах, API и статике ниже.
	router.Use(s.securityHeaders)
	router.Use(s.cidrMiddleware)
	router.Use(s.rateLimitMiddleware)
	router.Use(s.maxBodyMiddleware)

	router.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "data": map[string]any{"status": "alive"}})
	})

	router.Get("/readyz", func(w http.ResponseWriter, r *http.Request) {
		// Дешёвая готовность процесса без auth: тяжёлый ping роутера здесь
		// позволял неавторизованным клиентам держать 10-секундные проверки.
		// Глубокое состояние — в /api/v1/status (под авторизацией).
		writeJSON(w, http.StatusOK, apiEnvelope{OK: true, Data: map[string]any{
			"ready": true,
			"cache": s.syncer.Cache() != nil,
		}})
	})

	router.Route("/api/v1", func(r chi.Router) {
		r.Use(s.authMiddleware)
		r.Use(s.csrfMiddleware)
		r.Use(middleware.Compress(5))

		r.Get("/status", s.apiStatus)
		r.Get("/progress", s.apiProgress)
		r.Get("/services", s.apiServices)
		r.Post("/services", s.apiAddService)
		r.Put("/services/{name}", s.apiUpdateService)
		r.Delete("/services/{name}", s.apiDeleteService)
		r.Post("/services/sync", s.apiSyncMany)
		r.Post("/services/{name}/sync", s.apiSyncOne)
		r.Post("/services/{name}/dry-run", s.apiDryRun)
		r.Post("/services/{name}/recommend", s.apiRecommendService)
		r.Post("/services/{name}/suggest-domains", s.apiSuggestDomains)
		r.Post("/services/{name}/blockcheck", s.apiBlockcheck)
		r.Post("/services/{name}/refresh-preset", s.apiRefreshPreset)
		r.Get("/presets", s.apiPresets)
		r.Get("/presets/match", s.apiPresetsMatch)
		r.Post("/presets/refresh", s.apiPresetsRefresh)
		r.Post("/presets/create-from-upstream", s.apiPresetsCreateFromUpstream)
		r.Get("/schedules", s.apiSchedules)
		r.Put("/schedules/{service}", s.apiUpdateSchedule)
		r.Get("/logs", s.apiLogs)
		r.Get("/history", s.apiHistory)
		r.Get("/address-list", s.apiAddressList)
		r.Get("/address-list/{service}", s.apiServiceAddressList)
		// /ws — отдельно вне Compress (см. ниже).
	})

	pages := router.With(s.authMiddleware, middleware.Compress(5), noCache)
	pages.Get("/", s.pageDashboard)
	pages.Get("/services", s.pageServices)
	pages.Get("/address-list", s.pageAddressList)
	pages.Get("/schedules", s.pageSchedules)
	pages.Get("/settings", s.pageSettings)
	pages.Get("/logs", s.pageLogs)
	pages.Get("/partials/status", s.partialStatus)

	pages.Route("/actions", func(r chi.Router) {
		r.Use(s.csrfMiddleware)
		r.Post("/sync-all", s.actionSyncAll)
		r.Post("/sync-selected", s.actionSyncSelected)
		r.Post("/service-add", s.actionAddService)
		r.Post("/service-delete", s.actionDeleteService)
		r.Post("/schedule", s.actionUpdateSchedule)
		r.Post("/settings", s.actionUpdateSettings)
		r.Post("/cdn-sources", s.actionUpdateCDNSources)
		r.Post("/logs-clear", s.actionLogsClear)
	})

	static, _ := fs.Sub(webFS, "static")
	router.With(middleware.Compress(5)).Handle("/static/*", noCache(http.StripPrefix("/static/", http.FileServer(http.FS(static)))))

	// WebSocket отдельно от сжатия: gzip-обёртка и Hijack сочетаются лишь
	// случайно (chi#compress). Auth сохранён, Origin проверяет handleWS.
	router.With(s.authMiddleware).Get("/api/v1/ws", s.handleWS)

	s.httpSrv = &http.Server{
		Addr:              cfg.Web.Listen,
		Handler:           router,
		ReadTimeout:       10 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      0,
		IdleTimeout:       120 * time.Second,
	}
	return s, nil
}

// ============================================================================
// Middleware
// ============================================================================

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.cfg.RLock()
		secOn := s.cfg.Web.SecurityHeaders
		s.cfg.RUnlock()
		if secOn {
			w.Header().Set("Content-Security-Policy",
				"default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self' ws: wss:; frame-ancestors 'none'")
			w.Header().Set("X-Frame-Options", "DENY")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			w.Header().Set("Referrer-Policy", "same-origin")
			// За TLS-терминирующим прокси r.TLS пуст — смотрим ещё
			// X-Forwarded-Proto от доверенного прокси.
			if s.httpsNow(r) {
				w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
			}
		}
		next.ServeHTTP(w, r)
	})
}

// httpsNow — соединение защищено: прямое TLS или https за доверенным прокси.
// X-Forwarded-Proto от недоверенного источника игнорируется, иначе клиент
// мог бы сам себе включить Secure-cookie/HSTS.
func (s *Server) httpsNow(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	if !strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		return false
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	for _, tp := range s.ips.trustedProxies {
		if tp.Contains(ip) {
			return true
		}
	}
	return false
}

func (s *Server) cidrMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" || !s.ips.enabled {
			next.ServeHTTP(w, r)
			return
		}
		ip := s.ips.clientIP(r)
		if ip == nil || !s.ips.allowedIP(ip) {
			s.log.Warn("web access denied by allowed_cidrs", "remote", r.RemoteAddr)
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) rateLimitMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := ""
		if cip := s.ips.clientIP(r); cip != nil {
			ip = cip.String()
		}
		if ip != "" && !s.limiter.allow(ip) {
			http.Error(w, "too many requests", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) maxBodyMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Лимит тела для всех методов: 2МБ — влезает легитимный PUT domains
		// на 5000 доменов (~1.3МБ), DELETE/GET с телом тоже ограничены.
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.cfg.RLock()
		authOn := s.cfg.Web.Auth.Enabled
		authUser := s.cfg.Web.Auth.Username
		authPass := s.cfg.Web.Auth.Password
		s.cfg.RUnlock()
		if !authOn {
			next.ServeHTTP(w, r)
			return
		}

		if cookie, err := r.Cookie("mrs_session"); err == nil {
			if sess, ok := s.sessions.get(cookie.Value); ok {
				r = withSession(r, cookie.Value, sess)
				next.ServeHTTP(w, r)
				return
			}
		}

		user, pass, ok := r.BasicAuth()
		userOK := subtle.ConstantTimeCompare([]byte(user), []byte(authUser)) == 1
		passOK := subtle.ConstantTimeCompare([]byte(pass), []byte(authPass)) == 1
		if !ok || !userOK || !passOK {
			if ok {
				s.log.Warn("web auth failed", "user", user, "remote", r.RemoteAddr)
			}
			w.Header().Set("WWW-Authenticate", `Basic realm="mikrotik-route-sync", charset="UTF-8"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		// Cookie при Basic НЕ выдаём (устранение fixation): браузер и curl
		// шлют Basic сами на каждый запрос, ветка basicAuth выше covered.
		// Сессию учитываем для дедупликации (reuseOrCreate), cookie-ветка
		// ниже оставлена для совместимости (старые сессии).
		_, sess := s.sessions.reuseOrCreate(user)
		next.ServeHTTP(w, withBasicAuth(r, sess))
	})
}

// ============================================================================
// Session context
// ============================================================================

type ctxKey int

const sessionCtxKey ctxKey = 1

type sessionInfo struct {
	id        string
	sess      *session
	basicAuth bool
}

func withSession(r *http.Request, id string, sess *session) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), sessionCtxKey, &sessionInfo{id: id, sess: sess}))
}

func withBasicAuth(r *http.Request, sess *session) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), sessionCtxKey, &sessionInfo{sess: sess, basicAuth: true}))
}

func sessionFrom(r *http.Request) *sessionInfo {
	v, _ := r.Context().Value(sessionCtxKey).(*sessionInfo)
	return v
}

func (s *Server) csrfMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.cfg.RLock()
		csrfOn := s.cfg.Web.CSRFEnabled
		authOn := s.cfg.Web.Auth.Enabled
		s.cfg.RUnlock()
		if !csrfOn || !authOn {
			next.ServeHTTP(w, r)
			return
		}
		if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}

		if si := sessionFrom(r); si != nil && si.basicAuth {
			// Программные клиенты (curl) шлют Basic без cookie и без Origin.
			// Браузерный кросс-сайт всегда добавляет Origin (запрещённый
			// заголовок, подменить из JS нельзя) — чужой режем. Пустой
			// Origin бывает и у браузеров (form POST/sandbox, вырезан
			// прокси) — тогда смотрим Referer; нет обоих — программный.
			origin := r.Header.Get("Origin")
			if origin == "" {
				if ref := r.Header.Get("Referer"); ref != "" && !s.sameOrigin(r, ref) {
					s.log.Warn("cross-origin basic-auth request rejected (referer)", "remote", r.RemoteAddr)
					http.Error(w, `{"ok":false,"error":"cross-origin request rejected"}`, http.StatusForbidden)
					return
				}
				s.log.Debug("basic-auth without origin/referer (programmatic?)", "remote", r.RemoteAddr)
			} else if !s.sameOrigin(r, origin) {
				s.log.Warn("cross-origin basic-auth request rejected", "remote", r.RemoteAddr, "origin", origin)
				http.Error(w, `{"ok":false,"error":"cross-origin request rejected"}`, http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
			return
		}

		cookie, err := r.Cookie("mrs_session")
		if err != nil {
			http.Error(w, `{"ok":false,"error":"no session"}`, http.StatusUnauthorized)
			return
		}
		sess, ok := s.sessions.get(cookie.Value)
		if !ok {
			http.Error(w, `{"ok":false,"error":"session expired"}`, http.StatusUnauthorized)
			return
		}

		token := r.Header.Get("X-CSRF-Token")
		if token == "" {
			_ = r.ParseForm()
			token = r.FormValue("csrf_token")
		}
		if subtle.ConstantTimeCompare([]byte(token), []byte(sess.csrf)) != 1 {
			s.log.Warn("csrf validation failed", "remote", r.RemoteAddr)
			http.Error(w, `{"ok":false,"error":"csrf token invalid"}`, http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ============================================================================
// Helpers
// ============================================================================

type apiEnvelope struct {
	OK    bool   `json:"ok"`
	Data  any    `json:"data,omitempty"`
	Error string `json:"error,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeOK(w http.ResponseWriter, data any) {
	writeJSON(w, http.StatusOK, apiEnvelope{OK: true, Data: data})
}

// noCache запрещает кэширование страниц и статики в браузере: go:embed-ассеты
// меняются только при пересборке, и устаревший в кэше JS приводит к рассинхрону
// интерфейса (браузер показывает старую версию при новом бэкенде).
func noCache(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		next.ServeHTTP(w, r)
	})
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, apiEnvelope{OK: false, Error: msg})
}

// ============================================================================
// Статус MikroTik для live-индикатора
// ============================================================================

const mtHealthTTL = 5 * time.Second

// mikrotikHealth возвращает последнее известное состояние роутера и, если
// кэш устарел, запускает обновление: устаревший кэш обновляется в фоне
// (рендер не блокируется), холодный — синхронно, чтобы первый рендер сразу
// получил честный цвет, а не серый «неизвестен».
func (s *Server) mikrotikHealth(ctx context.Context) (ok, known bool) {
	s.mtMu.Lock()
	ok, known = s.mtOK, s.mtKnown
	switch {
	case known && time.Since(s.mtAt) < mtHealthTTL:
		s.mtMu.Unlock()
		return ok, known
	case s.mtRefreshing:
		s.mtMu.Unlock()
		return ok, known
	case known:
		s.mtRefreshing = true
		s.mtMu.Unlock()
		go s.mtRefresh(context.WithoutCancel(ctx))
		return ok, known
	}
	s.mtRefreshing = true
	s.mtMu.Unlock()
	s.mtRefresh(ctx)
	s.mtMu.Lock()
	ok, known = s.mtOK, s.mtKnown
	s.mtMu.Unlock()
	return ok, known
}

// mtRefresh выполняет ping и сохраняет результат в кэш.
func (s *Server) mtRefresh(ctx context.Context) {
	var err error
	if s.pingFn != nil {
		err = s.pingFn(ctx)
	} else {
		cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		err = s.syncer.PingMikroTik(cctx)
		cancel()
	}
	now := time.Now()
	s.mtMu.Lock()
	s.mtOK = err == nil
	s.mtKnown = true
	s.mtAt = now
	s.mtRefreshing = false
	s.mtMu.Unlock()
}

// dotState — класс и подсказка для точки статуса в шапке (рендерится в HTML,
// чтобы при переходе между страницами индикатор не мигал серым).
func (s *Server) dotState(r *http.Request) (class, title string) {
	ok, known := s.mikrotikHealth(r.Context())
	switch {
	case !known:
		return "live-dot", "MikroTik: статус неизвестен"
	case ok:
		return "live-dot live-on", "MikroTik: подключение есть"
	default:
		return "live-dot live-off", "MikroTik: нет подключения"
	}
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, name string, data map[string]any) {
	if data == nil {
		data = map[string]any{}
	}
	csrf := ""
	if si := sessionFrom(r); si != nil {
		csrf = si.sess.csrf
	}
	data["CSRF"] = csrf
	data["Version"] = version.Version
	s.cfg.RLock()
	data["Timezone"] = s.cfg.Timezone
	s.cfg.RUnlock()
	data["DotClass"], data["DotTitle"] = s.dotState(r)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, name, data); err != nil {
		s.log.Error("template render error", "template", name, "err", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
	}
}

// ============================================================================
// Pages
// ============================================================================

func (s *Server) currentUser(r *http.Request) string {
	if si := sessionFrom(r); si != nil {
		return si.sess.username
	}
	return "anonymous"
}

func (s *Server) statusData(r *http.Request) map[string]any {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	// Снапшот читаемого: дальше сеть без блокировок.
	s.cfg.RLock()
	prefix := s.cfg.Firewall.CommentPrefix
	addressList := s.cfg.Firewall.AddressList
	serviceCount := len(s.cfg.Services)
	active := make(map[string]bool, len(s.cfg.Services))
	for _, sv := range s.cfg.Services {
		active[sv] = true
	}
	s.cfg.RUnlock()

	managed := 0
	if entries, err := s.syncer.ListGlobalEntries(ctx); err == nil {
		for _, e := range entries {
			// Только активные сервисы: записи удалённых (purge=false)
			// остаются на роутере, но управляемыми уже не считаются.
			if svc, ok := addresslist.ServiceFromComment(e.Comment, prefix); ok && !e.IsDynamic() && active[svc] {
				managed++
			}
		}
	}

	return map[string]any{
		"MikroTikStatus": map[bool]string{true: "ok", false: "error"}[s.syncer.PingMikroTik(ctx) == nil],
		"AddressList":    addressList,
		"ServiceCount":   serviceCount,
		"ManagedEntries": managed,
		"Version":        version.Version,
		"Uptime":         time.Since(s.syncer.StartTime()).Truncate(time.Second).String(),
		"LastSync":       s.syncer.LastSync().Format(time.RFC3339),
		"Degraded":       s.syncer.DegradedServices(),
	}
}

func (s *Server) pageDashboard(w http.ResponseWriter, r *http.Request) {
	// Без statusData: шаблону нужны только CSRF/Dot*, всё остальное
	// досчитывает JS через API (иначе лишние обращения к MikroTik).
	s.cfg.RLock()
	svcs := append([]string(nil), s.cfg.Services...)
	s.cfg.RUnlock()
	s.render(w, r, "dashboard.html", map[string]any{
		"Services": svcs,
	})
}

func (s *Server) partialStatus(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "status.html", s.statusData(r))
}

type serviceRow struct {
	Name      string
	List      string
	Comment   string
	Schedule  string
	Routes    int
	LastSync  string
	Overrides bool
	// Текущий override для редактора источника (UI): пусто — авто.
	Method  string
	Domains []string
	// Пресет: слаг, витринный домен и последний снимок (для refresh).
	Preset    string
	Primary   string
	Generated []string
}

func (s *Server) pageServices(w http.ResponseWriter, r *http.Request) {
	// Без serviceRows: шаблон их не читает, строки грузит JS через
	// /api/v1/services (иначе обращение к MikroTik на каждый рендер).
	s.render(w, r, "services.html", map[string]any{})
}

func (s *Server) serviceRows(r *http.Request) []serviceRow {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	// Снапшот статики под RLock: дальше сеть (ListServiceEntries) и история
	// без блокировок (их мьютексы свои).
	s.cfg.RLock()
	names := append([]string(nil), s.cfg.Services...)
	list := s.cfg.Firewall.AddressList
	prefix := s.cfg.Firewall.CommentPrefix
	statics := make([]serviceRow, 0, len(names))
	for _, name := range names {
		row := serviceRow{
			Name:      name,
			List:      list,
			Comment:   addresslist.CommentFor(prefix, name),
			Schedule:  s.cfg.EffectiveSchedule(name),
			Overrides: hasOverrideLocked(s.cfg, name),
		}
		if ov, ok := s.cfg.Overrides[name]; ok {
			row.Method = ov.Method
			row.Domains = append([]string(nil), ov.Domains...)
			row.Preset = ov.Preset
			row.Primary = ov.Primary
			row.Generated = append([]string(nil), ov.Generated...)
		}
		statics = append(statics, row)
	}
	s.cfg.RUnlock()

	rows := make([]serviceRow, 0, len(statics))
	for _, row := range statics {
		if entries, err := s.syncer.ListServiceEntries(ctx, row.Name); err == nil {
			row.Routes = len(entries)
		}
		if rec, ok := s.syncer.History().Last(row.Name); ok {
			status := "успех"
			if rec.Status == "failed" || rec.Error != "" {
				status = "ошибка"
			}
			row.LastSync = rec.Time.Format("2006-01-02 15:04:05") + " (" + status + ")"
		}
		rows = append(rows, row)
	}
	return rows
}

// hasOverrideLocked — проверка override, вызывающий держит чтение конфига.
func hasOverrideLocked(cfg *config.Config, name string) bool {
	_, ok := cfg.Overrides[name]
	return ok
}

type scheduleRow struct {
	Name       string
	Effective  string
	ServiceOv  string
	Group      string
	GroupSched string
}

func (s *Server) pageSchedules(w http.ResponseWriter, r *http.Request) {
	// Всё читаемое — под RLock, рендер после (без вложенности).
	s.cfg.RLock()
	names := append([]string(nil), s.cfg.Services...)
	global := s.cfg.Schedules.Global
	type schedStatic struct {
		name, effective, serviceOv, group, groupSched string
	}
	statics := make([]schedStatic, 0, len(names))
	for _, name := range names {
		st := schedStatic{name: name, effective: s.cfg.EffectiveSchedule(name)}
		if sv, ok := s.cfg.Schedules.Services[name]; ok {
			st.serviceOv = sv.Schedule
		}
		for gname, g := range s.cfg.Schedules.Groups {
			for _, gs := range g.Services {
				if gs == name {
					st.group = gname
					st.groupSched = g.Schedule
				}
			}
		}
		statics = append(statics, st)
	}
	s.cfg.RUnlock()
	rows := make([]scheduleRow, 0, len(statics))
	for _, st := range statics {
		rows = append(rows, scheduleRow{Name: st.name, Effective: st.effective,
			ServiceOv: st.serviceOv, Group: st.group, GroupSched: st.groupSched})
	}
	s.render(w, r, "schedules.html", map[string]any{
		"Global": global,
		"Rows":   rows,
	})
}

// ============================================================================
// Settings
// ============================================================================

type settingSpec struct {
	Group string
	Key   string
	Title string
	Type  string
	Help  string
}

var settingsSpec = []settingSpec{
	{"Общее", "timezone", "Часовой пояс", "text",
		"IANA-имя (Europe/Moscow). Используется планировщиком для cron-расписаний. Применяется сразу."},
	{"Общее", "cache_path", "Путь к базе bbolt", "text",
		"Файл кэша, снапшотов и истории (cache.db). Изменение требует перезапуска приложения."},
	{"Логирование", "logging.level", "Уровень логирования", "level",
		"debug | info | warn | error. Применяется немедленно без перезапуска."},
	{"Логирование", "logging.file", "Файл лога", "text",
		"Путь к JSON-логу с ротацией (lumberjack). Пусто — только stdout. Смена пути требует перезапуска."},
	{"Логирование", "logging.max_size_mb", "Размер файла, МБ", "number",
		"Порог ротации одного лог-файла в мегабайтах (по умолчанию 10)."},
	{"Логирование", "logging.max_files", "Число архивов", "number",
		"Сколько ротированных файлов хранить (по умолчанию 5)."},
	{"Логирование", "logging.max_total_mb", "Максимум всего, МБ", "number",
		"Ориентировочный суммарный лимит каталога логов; фактически ≈ max_size_mb × (max_files+1)."},
	{"Логирование", "logging.compress", "Сжимать архивы", "bool",
		"gzip-сжатие ротированных логов (по умолчанию true)."},
	{"Логирование", "logging.also_stdout", "Дублировать в stdout", "bool",
		"Писать логи одновременно в файл и стандартный вывод (для docker logs / systemd)."},
	{"MikroTik", "mikrotik.host", "Адрес роутера", "text",
		"IP или hostname RouterOS (например 192.168.88.1). Применяется сразу — клиент пересоздаётся."},
	{"MikroTik", "mikrotik.port", "Порт REST API", "number",
		"443 — только с use_ssl:true; 80 — с use_ssl:false. Комбинация use_ssl:false + port:443 отклоняется валидацией. Применяется сразу."},
	{"MikroTik", "mikrotik.username", "API-пользователь", "text",
		"Отдельный пользователь RouterOS с минимальными правами (read, write, api, rest-api, policy)."},
	{"MikroTik", "mikrotik.password", "Пароль API", "secret",
		"Хранится в config.yaml (0600). Альтернатива — ENV MRS_MIKROTIK_PASSWORD. Чтобы изменить, введите новый пароль целиком (замаскированное значение сохраняется как есть)."},
	{"MikroTik", "mikrotik.use_ssl", "HTTPS (SSL)", "bool",
		"Подключаться к REST API по https вместо http. Применяется сразу."},
	{"MikroTik", "mikrotik.verify_ssl", "Проверять TLS-сертификат", "bool",
		"true — безопасно по умолчанию. false допустим только для самоподписанных сертификатов в доверенной домашней сети (в лог попадёт предупреждение)."},
	{"MikroTik", "mikrotik.timeout", "Таймаут запросов", "duration",
		"Go-формат длительности: 30s, 1m. Применяется сразу."},
	{"MikroTik", "mikrotik.rate_limit", "Лимит запросов/сек", "number",
		"Ограничение частоты запросов к RouterOS REST API (по умолчанию 20)."},
	{"Firewall Address List", "firewall.address_list", "Глобальный address-list", "text",
		"Единственный Firewall Address List для всех сервисов (например TO-VPN). Обязателен. Все сервисы пишут в него; изоляция — по комментарию. Имя: буквы/цифры/_.- (до 63). Применяется сразу."},
	{"Firewall Address List", "firewall.comment_prefix", "Префикс комментария", "text",
		"AUTO-пометка управляемых записей: AUTO:<service>. Формируется автоматически, изоляция сервисов по точному совпадению. Применяется сразу."},
	{"Firewall Address List", "firewall.ignore_dynamic", "Игнорировать dynamic-записи", "bool",
		"Всегда безопасно true: динамические (address dynamic) записи never трогаются программой."},
	{"Firewall Address List", "firewall.manage_disabled", "Создавать записи disabled", "bool",
		"false (рекомендуется) — новые записи всегда disabled=false. true — поле disabled не задаётся явно."},
	{"Telegram", "telegram.enabled", "Включить Telegram", "bool",
		"Уведомления и команды бота. Нотификатор и бот пересоздаются сразу."},
	{"Telegram", "telegram.bot_token", "Токен бота", "secret",
		"Выдаётся @BotFather. ENV MRS_TELEGRAM_BOT_TOKEN имеет приоритет."},
	{"Telegram", "telegram.chat_id", "Chat ID для уведомлений", "text",
		"Сюда шлются отчёты. Узнать свой ID: напишите боту и посмотрите предупреждение в логе ('unauthorized telegram access')."},
	{"Telegram", "telegram.authorized_chat_ids", "Авторизованные chat_id", "list",
		"Список через запятую. Только эти chat_id могут управлять ботом; остальным бот не отвечает."},
	{"Telegram", "telegram.rate_limit", "Сообщений/сек на чат", "number",
		"Антифлуд бота (по умолчанию 1)."},
	{"Telegram", "telegram.weekly_report.enabled", "Еженедельный отчёт", "bool",
		"Сводка по синхронизациям за неделю в chat_id. Поле резервируется в конфиге — отчёт пока не отправляется."},
	{"Telegram", "telegram.weekly_report.schedule", "Расписание отчёта", "text",
		"Cron в часовом поясе timezone, например '0 9 * * 1' — понедельник в 09:00. Работает вместе с enabled; отчёт пока не отправляется."},
	{"Telegram", "telegram.buttons.enabled", "Inline-кнопки бота", "bool",
		"Набор кнопок под командами бота (заготовка конфига: сейчас кнопки показываются всегда)."},
	{"Telegram", "telegram.buttons.max_selected_services", "Кнопок в групповой операции", "number",
		"Максимум сервисов в одной групповой операции бота (заготовка конфига, ограничение пока не применяется)."},
	{"Веб-интерфейс", "web.enabled", "Включить Web UI", "bool",
		"Включение/выключение сервера интерфейса. Изменение требует перезапуска процесса."},
	{"Веб-интерфейс", "web.listen", "Адрес и порт", "text",
		"Например 127.0.0.1:8080 (только локально) или 0.0.0.0:8080 (внутри Docker). Изменение требует перезапуска."},
	{"Веб-интерфейс", "web.allowed_cidrs", "Допустимые подсети", "list",
		"CIDR через запятую. Запросы из других сетей получают 403 (кроме /healthz)."},
	{"Веб-интерфейс", "web.trusted_proxies", "Доверенные прокси", "list",
		"CIDR reverse-proxy: только от них принимается X-Forwarded-For при проверке allowed_cidrs."},
	{"Веб-интерфейс", "web.auth.enabled", "Включить авторизацию", "bool",
		"Basic Auth + session cookie. Рекомендуется true. При false управление доступно всем, кто достиг порта."},
	{"Веб-интерфейс", "web.auth.username", "Логин", "text",
		"Имя пользователя Web UI."},
	{"Веб-интерфейс", "web.auth.password", "Пароль", "secret",
		"ENV MRS_WEB_PASSWORD имеет приоритет. Чтобы заменить — введите новый пароль целиком."},
	{"Веб-интерфейс", "web.session_timeout", "Время жизни сессии", "duration",
		"Сколько живёт session cookie без повторного ввода пароля (24h)."},
	{"Веб-интерфейс", "web.csrf_enabled", "Защита CSRF", "bool",
		"Требовать токен для изменяющих запросов от cookie-сессий. Держите включённым."},
	{"Веб-интерфейс", "web.security_headers", "Заголовки безопасности", "bool",
		"CSP, X-Frame-Options: DENY, X-Content-Type-Options: nosniff, Referrer-Policy, HSTS при HTTPS."},
	{"Планировщик", "scheduler.parallel", "Параллельные синхронизации", "bool",
		"false — строго последовательно. true — до max_concurrent сервисов одновременно."},
	{"Планировщик", "scheduler.max_concurrent", "Максимум одновременно", "number",
		"Ограничение параллельности синхронизаций (3)."},
	{"Планировщик", "scheduler.reload_interval", "Перечитывать расписания", "duration",
		"Период пересборки расписаний из конфига (1m). Применяется после перезапуска планировщика."},
	{"Планировщик", "scheduler.cache_ttl", "TTL кэша", "duration",
		"Сколько переиспользовать результаты ASN/BGP (24h). Применяется сразу для новых сборов."},
	{"Планировщик", "scheduler.cache_purge", "Очистка кэша", "text",
		"Формат 'every 1h' — период удаления протухших записей bbolt."},
	{"Планировщик", "schedules.global", "Глобальное расписание", "text",
		"По умолчанию для сервисов без override: every 6h, daily at 03:00, cron '0 3 * * *', manual, disabled. По сервису/группе — на странице «Расписания»."},
	{"Безопасность", "safety.max_delete_ratio", "Макс. доля удаления", "float",
		"Safe-diff: если будет удалено больше доли от существующих маршрутов сервиса (0.5 = 50%) — синхронизация прервётся, потребуется --force."},
	{"Безопасность", "safety.require_confirmation_over", "Абсолютный лимит удалений", "number",
		"Удаление более N маршрутов за раз требует --force даже при меньшей доле (100)."},
	{"Безопасность", "safety.min_prefix_v4", "Минимальная маска IPv4", "number",
		"Префиксы шире (например /7) отклоняются валидацией. Разрешённый диапазон 8..32 (8)."},
	{"Безопасность", "safety.allow_host_routes", "Разрешить /32", "bool",
		"Хост-маршруты из dynamic/whois-сбора. По умолчанию true — dynamic отдаёт только /32, без них метод пуст."},
	{"Безопасность", "safety.max_asn_prefixes", "Лимит префиксов ASN/whois", "number",
		"Защита от захвата чужих сетей мелкими сервисами (100). Переопределяется в overrides.<svc>.max_asn_prefixes."},
	{"Retry", "retry.max_attempts", "Число попыток", "number",
		"Максимум попыток для сетевых ошибок и 5xx (3). 4xx не повторяются, кроме 429."},
	{"Retry", "retry.base_delay", "Базовая задержка", "duration",
		"Старт экспоненциальной задержки base×2^n (1s)."},
	{"Retry", "retry.max_delay", "Максимум задержки", "duration",
		"Потолок backoff (30s)."},
	{"Retry", "retry.jitter", "Джиттер", "bool",
		"Случайная добавка к задержке, чтобы ретраи не совпадали по фазе."},
	{"Внешние API", "external.http_timeout", "Таймаут HTTP", "duration",
		"Таймаут запросов к bgp.tools/RIPEstat/CDN/спискам (15s). Применяется сразу."},
	{"Внешние API", "external.max_response_mb", "Макс. ответ, МБ", "number",
		"Ограничение размера тела ответа (io.LimitReader), 50. Дамп таблицы bgp.tools ~30-40 МБ читается потоково и под этот лимит не попадает."},
	{"Внешние API", "external.bgp_tools_contact", "Контакт для bgp.tools", "text",
		"email или 'имя проекта + контакт' — подставляется в User-Agent. bgp.tools требует описательный UA вместо дефолтного и банит анонимные скрипты. ENV MRS_BGP_TOOLS_CONTACT."},
	{"Внешние API", "external.akamai_api_key", "Ключ Akamai", "secret",
		"ENV MRS_AKAMAI_API_KEY. Akamai собирается методом asn; ключ не обязателен."},
	{"Внешние API", "external.urlscan_api_key", "Ключ urlscan.io", "secret",
		"ENV MRS_URLSCAN_API_KEY. Бесплатный ключ для подбора смежных доменов. Без него доступен только ограниченный анонимный режим."},
	{"Внешние API", "external.rdap_timeout", "Таймаут RDAP", "duration",
		"Проверка принадлежности префиксов ASN через RDAP (10s)."},
	{"Внешние API", "external.resolver", "DNS-резолвер", "text",
		"ip:port (1.1.1.1:53) для A-записей и Cymru-запросов ASN. Пусто — системный DNS. Применяется сразу."},
	{"Снапшоты", "snapshots.enabled", "Снимки перед синхронизацией", "bool",
		"Сохранять текущие маршруты сервиса в bbolt перед применением diff (нужно для ручного restore)."},
	{"Снапшоты", "snapshots.ttl", "TTL снапшотов", "duration",
		"Снапшоты старше этого возраста удаляются при cleanup (168h = 7 дней)."},
	{"Снапшоты", "snapshots.max_count", "Максимум на сервис", "number",
		"Лишние (сверх 50) старые снапшоты удаляются при cleanup."},
	{"Подбор доменов", "suggest.v2fly_enabled", "Каталог v2fly", "bool",
		"Встроенные пресеты сервисов (youtube, telegram, …) в выборе при добавлении."},
	{"Подбор доменов", "suggest.ru_enabled", "RU-пресет", "bool",
		"Зеркало antifilter (заблокированные в RU). Список огромный — включается осознанно, см. large-предупреждение."},
	{"Подбор доменов", "suggest.urlscan_mode", "Режим urlscan", "mode",
		"off — кнопка скрыта; key — полный (нужен ключ); anonymous — разовый best-effort с длинным кэшем."},
	{"Подбор доменов", "suggest.sources", "Порядок источников", "list",
		"preset,urlscan через запятую. Ручной ввод доступен всегда."},
	{"Подбор доменов", "presets.update_schedule", "Обновление зеркала", "text",
		"Расписание докачки RU-зеркала (cron/every/daily как у синков). Пусто — только кнопкой."},
}

type settingRow struct {
	Key     string
	Title   string
	Type    string
	Value   string
	Help    string
	Options []string
}

type settingGroup struct {
	Title string
	Rows  []settingRow
}

func (s *Server) settingValue(key string) string {
	v, err := s.cfg.GetPath(key)
	if err != nil || v == nil {
		return ""
	}
	switch t := v.(type) {
	case bool:
		return strconv.FormatBool(t)
	case []string:
		return strings.Join(t, ", ")
	default:
		return fmt.Sprint(v)
	}
}

func (s *Server) settingGroups() []settingGroup {
	// Чтение всего конфига под RLock (вызывающий блокировок не держит).
	s.cfg.RLock()
	defer s.cfg.RUnlock()
	var groups []settingGroup
	idx := map[string]int{}
	for _, sp := range settingsSpec {
		val := s.settingValue(sp.Key)
		if sp.Type == "secret" {
			val = Mask(val)
		}
		row := settingRow{Key: sp.Key, Title: sp.Title, Type: sp.Type, Value: val, Help: sp.Help}
		if sp.Type == "level" {
			row.Options = []string{"debug", "info", "warn", "error"}
		}
		if sp.Type == "mode" {
			row.Options = []string{"off", "key", "anonymous"}
		}
		i, ok := idx[sp.Group]
		if !ok {
			groups = append(groups, settingGroup{Title: sp.Group})
			i = len(groups) - 1
			idx[sp.Group] = i
		}
		groups[i].Rows = append(groups[i].Rows, row)
	}
	return groups
}

func (s *Server) pageSettings(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "settings.html", map[string]any{
		"Groups":     s.settingGroups(),
		"CDNSources": s.cdnSourceRows(),
		"CDNParsers": cdnParsersList,
	})
}

// cdnSourceRow — строка таблицы источников CDN в настройках.
type cdnSourceRow struct {
	Service string
	Name    string
	ASN     int
	URL     string
	Parser  string
}

// cdnParsersList — варианты парсера для выпадающего списка в UI.
var cdnParsersList = []string{"text", "google", "aws", "fastly"}

func (s *Server) cdnSourceRows() []cdnSourceRow {
	s.cfg.RLock()
	defer s.cfg.RUnlock()
	rows := make([]cdnSourceRow, 0, len(s.cfg.CDNSources))
	for _, src := range s.cfg.CDNSources {
		rows = append(rows, cdnSourceRow{
			Service: src.Service, Name: src.Name, ASN: src.ASN,
			URL: src.URL, Parser: src.Parser,
		})
	}
	return rows
}

func (s *Server) pageLogs(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "logs.html", map[string]any{})
}

// ============================================================================
// REST API /api/v1
// ============================================================================

func (s *Server) apiStatus(w http.ResponseWriter, r *http.Request) {
	// Состояние роутера — из кэша с TTL (mikrotikHealth): синхронный ping
	// на каждый полл вкладок держал бы коннекты по 3с.
	mtOK, mtKnown := s.mikrotikHealth(r.Context())
	// Map'ы маршалятся в байты под RLock, в сокет — после (см. apiSchedules).
	s.cfg.RLock()
	raw, err := json.Marshal(apiEnvelope{OK: true, Data: map[string]any{
		"version":     version.Version,
		"uptime":      time.Since(s.syncer.StartTime()).String(),
		"last_sync":   s.syncer.LastSync(),
		"mikrotik_ok": mtKnown && mtOK,
		"services":    append([]string(nil), s.cfg.Services...),
		"degraded":    s.syncer.DegradedServices(),
		"schedules":   s.cfg.Schedules,
	}})
	s.cfg.RUnlock()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
}

func (s *Server) apiServices(w http.ResponseWriter, r *http.Request) {
	rows := s.serviceRows(r)
	writeOK(w, map[string]any{
		"services": rows,
		"count":    len(rows),
	})
}

// apiProgress — активные синхронизации с фазой и процентом (in-memory,
// без обращений к MikroTik); поллится страницей «Сервисы».
func (s *Server) apiProgress(w http.ResponseWriter, r *http.Request) {
	writeOK(w, s.syncer.ProgressSnapshot())
}

func (s *Server) apiAddService(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name      string   `json:"name"`
		Method    string   `json:"method"`
		StaticURL string   `json:"static_url"`
		Domains   []string `json:"domains"`
		Preset    string   `json:"preset"`
		Primary   string   `json:"primary"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if !config.ValidateServiceName(strings.ToLower(req.Name)) {
		writeErr(w, http.StatusBadRequest, "invalid service name")
		return
	}
	ov := config.ServiceOverride{
		Method:    strings.TrimSpace(strings.ToLower(req.Method)),
		StaticURL: strings.TrimSpace(req.StaticURL),
		Domains:   req.Domains,
	}
	if req.Preset != "" {
		p, ok := presets.Get(req.Preset)
		if !ok {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("preset %q unavailable", req.Preset))
			return
		}
		if len(p.Domains) > core.MaxServiceDomains {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("preset %q too large (%d domains, max %d)", req.Preset, len(p.Domains), core.MaxServiceDomains))
			return
		}
		seen := map[string]bool{}
		merged := append([]string(nil), p.Domains...)
		for _, d := range merged {
			seen[strings.ToLower(d)] = true
		}
		for _, d := range req.Domains {
			if !seen[strings.ToLower(d)] {
				merged = append(merged, d)
				seen[strings.ToLower(d)] = true
			}
		}
		ov.Domains = merged
		ov.Preset = p.Slug
		ov.Generated = append([]string(nil), p.Domains...)
		if ov.Method == "" && config.IsValidMethod(p.Method) && (p.Method != "static_url" || p.StaticURL != "") {
			ov.Method = p.Method
		}
		if ov.StaticURL == "" {
			ov.StaticURL = p.StaticURL
		}
		if len(p.Domains) > 0 {
			ov.Primary = p.Domains[0]
		}
	}
	if req.Primary != "" {
		ov.Primary = strings.TrimSpace(req.Primary)
	}
	if len(ov.Domains) > core.MaxServiceDomains {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("too many domains (%d, max %d)", len(ov.Domains), core.MaxServiceDomains))
		return
	}
	if err := s.syncer.AddServiceWithConfig(r.Context(), req.Name, ov); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.auditChange(r, "api", "service_add", req.Name)
	writeOK(w, map[string]string{"service": req.Name})
}

// apiRecommendService — проверка сервиса на обслуживание за CDN.
// body: {"apply": true} — применить рекомендацию (purge + method=cdn).
// apiSuggestDomains — разовый подбор смежных доменов через urlscan
// (советник: только кандидаты, запись — обычным PUT /services/{name}).
func (s *Server) apiSuggestDomains(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if !config.ValidateServiceName(strings.ToLower(name)) {
		writeErr(w, http.StatusBadRequest, "invalid service name")
		return
	}
	var req struct {
		Scans int `json:"scans"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req) // пустое тело = дефолт
	writeOK(w, s.syncer.SuggestDomains(r.Context(), name, req.Scans))
}

// apiRefreshPreset — пересчёт Domains из пресета с сохранением ручных.
// Отвечает diff'ом: UI предлагает dry-run перед применением маршрутов.
func (s *Server) apiRefreshPreset(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if !config.ValidateServiceName(strings.ToLower(name)) {
		writeErr(w, http.StatusBadRequest, "invalid service name")
		return
	}
	var req struct {
		Preset string `json:"preset"`
		Force  bool   `json:"force"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req) // пустое тело = привязанный пресет
	added, removed, err := s.syncer.ApplyPresetRefresh(r.Context(), name, req.Preset, req.Force)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.auditChange(r, "api", "preset_refresh", name)
	writeOK(w, map[string]any{"service": name, "added": added, "removed": removed})
}

// presetsDir — каталог runtime-зеркала (рядом с cache.db).
func (s *Server) presetsDir() string {
	s.cfg.RLock()
	defer s.cfg.RUnlock()
	if s.cfg.CachePath == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(s.cfg.CachePath), "presets")
}

// apiPresets — каталог пресетов + состояние зеркала + доступность подбора.
func (s *Server) apiPresets(w http.ResponseWriter, r *http.Request) {
	s.cfg.RLock()
	v2flyOn := s.cfg.Suggest.V2flyEnabled
	ruOn := s.cfg.Suggest.RuEnabled
	mode := s.cfg.Suggest.UrlscanMode
	sources := append([]string(nil), s.cfg.Suggest.Sources...)
	s.cfg.RUnlock()
	list := presets.List()
	if !v2flyOn || !ruOn {
		filtered := list[:0]
		for _, p := range list {
			if p.Slug == "rublock" && !ruOn {
				continue
			}
			if p.Slug != "rublock" && !v2flyOn {
				continue
			}
			filtered = append(filtered, p)
		}
		list = filtered
	}
	// Домены больших пресетов в каталог не тащим (rublock — мегабайты).
	for i := range list {
		if list[i].Large {
			list[i].Domains = nil
		}
	}
	var mirror any
	if dir := s.presetsDir(); dir != "" {
		if m := presets.MirrorInfo(dir); m != nil {
			mirror = m
		}
	}
	writeOK(w, map[string]any{
		"presets": list,
		"mirror":  mirror,
		"suggest": map[string]any{
			"v2fly":   v2flyOn,
			"ru":      ruOn,
			"mode":    mode,
			"sources": sources,
		},
	})
}

// apiPresetsRefresh — ручная докачка зеркал (?source=ru|upstream|all).
func (s *Server) apiPresetsRefresh(w http.ResponseWriter, r *http.Request) {
	dir := s.presetsDir()
	if dir == "" {
		writeErr(w, http.StatusBadRequest, "cache_path not configured")
		return
	}
	source := strings.ToLower(r.URL.Query().Get("source"))
	if source == "" {
		source = "all"
	}
	if source != "all" && source != "ru" && source != "upstream" {
		writeErr(w, http.StatusBadRequest, "source must be ru|upstream|all")
		return
	}
	// Дедуплика по операциям (не по строке source): "all" занимает обе,
	// иначе конкурентные all+ru качают RU-зеркало дважды.
	need := map[string]bool{}
	switch source {
	case "all":
		need["ru"] = true
		need["upstream"] = true
	default:
		need[source] = true
	}
	s.refreshMu.Lock()
	if s.refreshOn == nil {
		s.refreshOn = map[string]bool{}
	}
	for op := range need {
		if s.refreshOn[op] {
			s.refreshMu.Unlock()
			writeErr(w, http.StatusTooManyRequests, "refresh of "+op+" already running")
			return
		}
	}
	for op := range need {
		s.refreshOn[op] = true
	}
	s.refreshMu.Unlock()
	defer func() {
		s.refreshMu.Lock()
		for op := range need {
			delete(s.refreshOn, op)
		}
		s.refreshMu.Unlock()
	}()
	out := map[string]any{}
	if need["ru"] {
		st, err := presets.RefreshMirror(r.Context(), dir)
		if err != nil {
			writeErr(w, http.StatusBadGateway, "ru: "+err.Error())
			return
		}
		out["ru"] = st
	}
	if need["upstream"] {
		st, err := presets.RefreshUpstream(r.Context(), dir)
		if err != nil {
			writeErr(w, http.StatusBadGateway, "upstream: "+err.Error())
			return
		}
		out["upstream"] = st
	}
	s.auditChange(r, "api", "presets_refresh", fmt.Sprintf("%v", out))
	writeOK(w, out)
}

// apiPresetsCreateFromUpstream — создаёт пресет из upstream-файла зеркала
// (кнопка «создать из списка» в подсказке невендоренного совпадения).
// Сохраняет результат в DiskDir как <slug>.yaml — кастомный пресет,
// который Generator не трогает, он живёт до ручного удаления файла.
// body: { "slug": "geolocation-cn" }
func (s *Server) apiPresetsCreateFromUpstream(w http.ResponseWriter, r *http.Request) {
	dir := s.presetsDir()
	if dir == "" {
		writeErr(w, http.StatusBadRequest, "cache_path not configured")
		return
	}
	var req struct {
		Slug string `json:"slug"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Slug == "" {
		writeErr(w, http.StatusBadRequest, "slug required")
		return
	}
	slug := strings.ToLower(strings.TrimSpace(req.Slug))
	if !presets.ValidSlug(slug) {
		writeErr(w, http.StatusBadRequest, "invalid slug: only a-z0-9_- allowed, no '!'")
		return
	}
	// Полная карта upstream: без соседних файлов include: не раскрываются
	// (у category-companies из 125 записей теряются 116).
	files := presets.LoadUpstreamFiles(dir)
	if files == nil {
		writeErr(w, http.StatusNotFound, "upstream mirror not found (скачайте зеркало)")
		return
	}
	if _, ok := files[slug]; !ok {
		writeErr(w, http.StatusNotFound, "upstream file not found: "+slug)
		return
	}
	src := presets.PresetSource{
		Repo:   presets.SourceV2Fly,
		Commit: "runtime-mirror",
		Date:   time.Now().UTC().Format(time.RFC3339),
	}
	p, st, err := presets.ParseSingleUpstream(files, slug, src)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// Сохраняем в DiskDir (кастомные пресеты).
	if err := os.MkdirAll(dir, 0o755); err != nil {
		writeErr(w, http.StatusInternalServerError, "mkdir: "+err.Error())
		return
	}
	dst := filepath.Join(dir, slug+".yaml")
	rawYaml, err := yaml.Marshal(p)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "marshal: "+err.Error())
		return
	}
	// Атомарная запись через tmp+rename.
	tmp, err := os.CreateTemp(dir, ".preset-*.tmp")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "tmp: "+err.Error())
		return
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(rawYaml); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		writeErr(w, http.StatusInternalServerError, "write: "+err.Error())
		return
	}
	_ = tmp.Close()
	if err := os.Rename(tmpName, dst); err != nil {
		_ = os.Remove(tmpName)
		writeErr(w, http.StatusInternalServerError, "rename: "+err.Error())
		return
	}
	// Сброс кэша пресетов — List() сразу покажет новый.
	presets.FlushAll()
	// Аудит + ответ с предупреждением о капе, если было.
	s.auditChange(r, "api", "preset_create_from_upstream", slug)
	resp := map[string]any{
		"slug":      p.Slug,
		"title":     p.Title,
		"domains":   len(p.Domains),
		"method":    p.Method,
		"source":    p.Source.String(),
		"truncated": st.Truncated,
		"showcase":  p.Domains[0],
	}
	if st.Truncated > 0 {
		resp["warning"] = fmt.Sprintf("list truncated to %d domains (original had %d)", presets.MaxDomains, len(p.Domains)+st.Truncated)
	}
	writeOK(w, resp)
}

// apiPresetsMatch — какие пресеты содержат домен (инвертированный индекс
// полного зеркала v2fly). Пусто — зеркало не скачано или совпадений нет.
func (s *Server) apiPresetsMatch(w http.ResponseWriter, r *http.Request) {
	domain := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("domain")))
	if domain == "" {
		writeErr(w, http.StatusBadRequest, "domain required")
		return
	}
	type match struct {
		Slug     string `json:"slug"`
		Title    string `json:"title"`
		Vendored bool   `json:"vendored"`
	}
	var out []match
	var rest []match
	for _, slug := range presets.MatchUpstream(domain) {
		m := match{Slug: slug}
		if p, ok := presets.Get(slug); ok {
			m.Title = p.Title
			m.Vendored = true
			// Вендоренные — первыми: только их можно реально применить
			// (слаг с "!" не пройдёт ValidateServiceName и Get).
			out = append(out, m)
		} else {
			m.Title = slug
			rest = append(rest, m)
		}
		if len(out)+len(rest) >= 8 {
			break
		}
	}
	out = append(out, rest...)
	writeOK(w, map[string]any{"domain": domain, "matches": out})
}

// apiBlockcheck — какие домены сервиса есть в RU-блоклисте.
// Потоковое чтение зеркала (1.7M строк в память не грузим), итог кэшируем.
func (s *Server) apiBlockcheck(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if !config.ValidateServiceName(strings.ToLower(name)) {
		writeErr(w, http.StatusBadRequest, "invalid service name")
		return
	}
	s.cfg.RLock()
	ov, _ := s.cfg.Overrides[name]
	inServices := false
	for _, p := range s.cfg.Services {
		if p == name {
			inServices = true
			break
		}
	}
	s.cfg.RUnlock()
	if !inServices {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("service %s not found", name))
		return
	}
	domains := append([]string(nil), ov.Domains...)
	if len(domains) == 0 {
		writeOK(w, map[string]any{"service": name, "blocked": []string{}, "total": 0})
		return
	}
	dir := s.presetsDir()
	if dir == "" {
		writeErr(w, http.StatusBadRequest, "cache_path not configured")
		return
	}
	version := ""
	if m := presets.MirrorInfo(dir); m != nil {
		version = m.UpdatedAt
	}
	blocked, cached, err := presets.CheckBlockedCached(filepath.Join(dir, "rublock.yaml"), version, domains)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "blocklist unavailable (зеркало не скачано?): "+err.Error())
		return
	}
	writeOK(w, map[string]any{
		"service": name, "blocked": blocked,
		"blocked_count": len(blocked), "total": len(domains), "cached": cached,
	})
}

func (s *Server) apiRecommendService(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	var req struct {
		Apply bool `json:"apply"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req) // пустое тело = только проверка

	ov := s.syncer.GetServiceOverride(name)
	adv := s.syncer.AdviseCDN(r.Context(), name, ov)

	resp := map[string]any{"advice": adv}
	if req.Apply {
		// Предел как у одиночного синка (purge перебирает записи).
		actx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 10*time.Minute)
		purged, err := s.syncer.ApplyCDNRecommendation(actx, name, adv)
		cancel()
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		resp["applied"] = true
		resp["purged"] = purged
	}
	writeOK(w, resp)
}

// apiUpdateService — изменить override сервиса (scope: method, domains).
// method == null — не трогать, "" — сбросить в авто; domains == null —
// не трогать, иначе заменить целиком. Ошибки валидации (метод, диапазоны)
// возвращаются текстом 400 для показа в UI.
func (s *Server) apiUpdateService(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	var req struct {
		Method  *string  `json:"method"`
		Domains []string `json:"domains"`
	}
	if r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid request body")
			return
		}
	}
	if req.Method == nil && req.Domains == nil {
		writeErr(w, http.StatusBadRequest, "nothing to update: provide method and/or domains")
		return
	}
	ov, err := s.syncer.UpdateServiceOverride(r.Context(), name, req.Method, req.Domains)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.auditChange(r, "api", "service_update", name)
	writeOK(w, map[string]any{"service": name, "override": ov})
}

func (s *Server) apiDeleteService(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	purge := r.URL.Query().Get("purge") == "1" || r.URL.Query().Get("purge") == "true"
	if err := s.syncer.RemoveService(r.Context(), name, purge); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.auditChange(r, "api", "service_delete", name)
	writeOK(w, map[string]string{"service": name})
}

func (s *Server) apiSyncMany(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Services []string `json:"services"`
		DryRun   bool     `json:"dry_run"`
		Force    bool     `json:"force"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	var services []string
	if len(req.Services) == 0 {
		// Копия под чтением: горутина живёт дольше запроса.
		s.cfg.RLock()
		services = append([]string(nil), s.cfg.Services...)
		s.cfg.RUnlock()
	} else {
		var err error
		services, err = s.configuredServices(req.Services)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	runner := s.syncer
	// Фоновая задача с пределом (как у планировщика/бота, но шире —
	// сервисов много): зависший MikroTik не копит горутины вечно.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 30*time.Minute)
	go func() {
		defer cancel()
		_ = runner.SyncMany(ctx, services, req.DryRun, req.Force)
	}()
	writeOK(w, map[string]any{"started": true, "services": services, "dry_run": req.DryRun})
}

func (s *Server) apiSyncOne(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	var req struct {
		DryRun bool `json:"dry_run"`
		Force  bool `json:"force"`
	}
	if r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid request body")
			return
		}
	}
	tctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	// Без WithoutCancel: обрыв соединения клиентом прерывает ожидание,
	// дедлайн ограничивает висящий роутер (как у планировщика/бота).
	res, err := s.syncer.SyncService(tctx, name, req.DryRun, req.Force)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, core.ErrServiceNotFound) {
			status = http.StatusNotFound
		}
		writeJSON(w, status, apiEnvelope{OK: false, Data: res, Error: err.Error()})
		return
	}
	writeOK(w, res)
}

func (s *Server) apiDryRun(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	// Предел сверху как у одиночного синка; обрыв соединения клиентом
	// тоже прерывает (r.Context) — ждать некого.
	dctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	res, err := s.syncer.SyncService(dctx, name, true, false)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, core.ErrServiceNotFound) {
			status = http.StatusNotFound
		}
		writeJSON(w, status, apiEnvelope{OK: false, Data: res, Error: err.Error()})
		return
	}
	writeOK(w, res)
}

func (s *Server) apiSchedules(w http.ResponseWriter, r *http.Request) {
	// Маршалим под RLock в байты, в сокет — после: зависший клиент
	// не должен держать чтение конфига (писатели голодают).
	s.cfg.RLock()
	raw, err := json.Marshal(apiEnvelope{OK: true, Data: map[string]any{
		"global":   s.cfg.Schedules.Global,
		"groups":   s.cfg.Schedules.Groups,
		"services": s.cfg.Schedules.Services,
		"effective": func() map[string]string {
			eff := map[string]string{}
			for _, svc := range s.cfg.Services {
				eff[svc] = s.cfg.EffectiveSchedule(svc)
			}
			return eff
		}(),
	}})
	s.cfg.RUnlock()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
}

func (s *Server) apiUpdateSchedule(w http.ResponseWriter, r *http.Request) {
	service := chi.URLParam(r, "service")
	var req struct {
		Schedule string `json:"schedule"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := s.applySchedule(r, service, req.Schedule); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeOK(w, map[string]string{"service": service, "schedule": req.Schedule})
}

func (s *Server) applySchedule(r *http.Request, service, spec string) error {
	service = strings.ToLower(strings.TrimSpace(service))
	spec = strings.TrimSpace(spec)
	if !config.ValidateServiceName(service) {
		return fmt.Errorf("invalid service name")
	}
	if spec != "" && !scheduler.ValidSpec(spec) {
		return fmt.Errorf("некорректное расписание %q (ожидается cron, 'every 6h', 'daily at 03:00', manual/disabled/inherit)", spec)
	}
	if err := s.cfg.ReplaceServiceSchedule(service, spec); err != nil {
		return err
	}
	s.auditChange(r, "web", "schedule_change", service)
	if err := s.syncer.ReloadScheduler(); err != nil {
		s.log.Warn("failed to reload scheduler after schedule change", "err", err)
	}
	return nil
}

func (s *Server) apiLogs(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 10000 {
			limit = n
		}
	}
	level := strings.ToLower(r.URL.Query().Get("level"))
	service := r.URL.Query().Get("service")
	var since time.Time
	if v := r.URL.Query().Get("since"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			since = t
		}
	}

	match := func(e map[string]any) bool {
		if level != "" && !strings.EqualFold(fmt.Sprint(e["level"]), level) {
			return false
		}
		if service != "" && fmt.Sprint(e["service"]) != service {
			return false
		}
		if !since.IsZero() {
			if ts, ok := e["time"].(string); ok {
				if t, err := time.Parse(time.RFC3339, ts); err == nil && t.Before(since) {
					return false
				}
			}
		}
		return true
	}

	entries, truncated, err := s.syncer.GetLogs(r.Context(), limit, match)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if truncated {
		w.Header().Set("X-Logs-Truncated", "1")
	}
	// Контракт API — новые первыми (история хранится старыми первыми).
	for i, j := 0, len(entries)-1; i < j; i, j = i+1, j-1 {
		entries[i], entries[j] = entries[j], entries[i]
	}
	writeOK(w, entries)
}

func (s *Server) apiHistory(w http.ResponseWriter, r *http.Request) {
	service := r.URL.Query().Get("service")
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	writeOK(w, s.syncer.History().Records(service, limit))
}

// ============================================================================
// Firewall Address List
// ============================================================================

type addressRow struct {
	Address  string
	List     string
	Comment  string
	Service  string
	Managed  bool
	Dynamic  bool
	Disabled bool
}

func (s *Server) toAddressRows(entries []addresslist.Entry) []addressRow {
	s.cfg.RLock()
	prefix := s.cfg.Firewall.CommentPrefix
	active := make(map[string]bool, len(s.cfg.Services))
	for _, sv := range s.cfg.Services {
		active[sv] = true
	}
	s.cfg.RUnlock()

	rows := make([]addressRow, 0, len(entries))
	for _, e := range entries {
		row := addressRow{
			Address:  e.Address,
			List:     e.List,
			Comment:  e.Comment,
			Dynamic:  e.IsDynamic(),
			Disabled: e.IsDisabled(),
		}
		if svc, ok := addresslist.ServiceFromComment(e.Comment, prefix); ok {
			// Бейдж — только активным: удалённые сервисы видны лишь
			// в логах, а комментарий с AUTO:имя остаётся как есть.
			if active[svc] {
				row.Service = svc
			}
			row.Managed = active[svc] && !e.IsDynamic()
		}
		rows = append(rows, row)
	}
	sortRows(rows)
	return rows
}

func sortRows(rows []addressRow) {
	for i := 1; i < len(rows); i++ {
		for j := i; j > 0 && rowsLess(rows[j], rows[j-1]); j-- {
			rows[j], rows[j-1] = rows[j-1], rows[j]
		}
	}
}

func rowsLess(a, b addressRow) bool {
	if a.Service != b.Service {
		return a.Service < b.Service
	}
	return a.Address < b.Address
}

// ИЗМЕНЕНО: русское сообщение, DEBUG-уровень логирования
func (s *Server) pageAddressList(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	data := map[string]any{}
	entries, err := s.syncer.ListGlobalEntries(ctx)
	if err != nil {
		s.log.Debug("failed to list global address entries (MikroTik unavailable)", "err", err)
		data["Error"] = "MikroTik недоступен. Проверьте подключение к роутеру, правильность адреса, порта и настроек SSL/TLS."
	} else {
		data["Rows"] = s.toAddressRows(entries)
	}
	s.render(w, r, "addresslist.html", data)
}

// ИЗМЕНЕНО: возвращаем пустой список с HTTP 200 при недоступности Микротика
func (s *Server) apiAddressList(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	entries, err := s.syncer.ListGlobalEntries(ctx)
	s.cfg.RLock()
	listName := s.cfg.Firewall.AddressList
	s.cfg.RUnlock()
	if err != nil {
		s.log.Debug("MikroTik is unavailable when listing global entries via API, returning empty list", "err", err)
		writeOK(w, map[string]any{
			"list":          listName,
			"entries":       []addressRow{},
			"mikrotik_down": true,
		})
		return
	}
	writeOK(w, map[string]any{
		"list":    listName,
		"entries": s.toAddressRows(entries),
	})
}

// ИЗМЕНЕНО: возвращаем пустой список с HTTP 200 при недоступности Микротика
func (s *Server) apiServiceAddressList(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "service")
	if !config.ValidateServiceName(name) {
		writeErr(w, http.StatusBadRequest, "invalid service name")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	entries, err := s.syncer.ListServiceEntries(ctx, name)
	s.cfg.RLock()
	listName := s.cfg.Firewall.AddressList
	comment := addresslist.CommentFor(s.cfg.Firewall.CommentPrefix, name)
	s.cfg.RUnlock()
	if err != nil {
		s.log.Debug("MikroTik is unavailable when listing service entries via API, returning empty list",
			"service", name, "err", err)
		writeOK(w, map[string]any{
			"service":       name,
			"list":          listName,
			"comment":       comment,
			"entries":       []addresslist.Entry{},
			"count":         0,
			"mikrotik_down": true,
		})
		return
	}
	writeOK(w, map[string]any{
		"service": name,
		"list":    listName,
		"comment": comment,
		"entries": entries,
		"count":   len(entries),
	})
}

// ============================================================================
// HTMX actions
// ============================================================================

func (s *Server) actionSyncAll(w http.ResponseWriter, r *http.Request) {
	// Копия списка: горутина живёт дольше запроса, слайс конфига — нет.
	s.cfg.RLock()
	services := append([]string(nil), s.cfg.Services...)
	s.cfg.RUnlock()
	if len(services) == 0 {
		_, _ = w.Write([]byte("<p class=\"muted\">Нет сервисов</p>"))
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 30*time.Minute)
	go func() {
		defer cancel()
		_ = s.syncer.SyncMany(ctx, services, false, false)
	}()
	writeOK(w, map[string]any{"started": true, "services": services})
}

func (s *Server) actionSyncSelected(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	var services []string
	var err error
	if services, err = s.configuredServices(r.PostForm["service"]); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(services) == 0 {
		writeErr(w, http.StatusBadRequest, "nothing selected")
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 30*time.Minute)
	go func() {
		defer cancel()
		_ = s.syncer.SyncMany(ctx, services, false, false)
	}()
	writeOK(w, map[string]any{"started": true, "services": services})
}

// maxSyncServices — предел числа сервисов в одном запросе на синк: веер
// горутин (по одной на сервис до семафора) не должен расти без границ.
const maxSyncServices = 1000

// configuredServices нормализует запрошенные имена сервисов и оставляет
// только те, что есть в конфиге. Возвращает ошибку на некорректное имя,
// дубликат, пустой результат или превышение предела — так веер горутин
// ограничен, а синк несуществующего сервиса не пишет AUTO:<name> на роутер.
func (s *Server) configuredServices(requested []string) ([]string, error) {
	if len(requested) > maxSyncServices {
		return nil, fmt.Errorf("too many services: %d (max %d)", len(requested), maxSyncServices)
	}
	s.cfg.RLock()
	known := make(map[string]struct{}, len(s.cfg.Services))
	for _, p := range s.cfg.Services {
		known[p] = struct{}{}
	}
	s.cfg.RUnlock()

	out := make([]string, 0, len(requested))
	seen := make(map[string]struct{}, len(requested))
	for _, raw := range requested {
		name := strings.ToLower(strings.TrimSpace(raw))
		if !config.ValidateServiceName(name) {
			return nil, fmt.Errorf("invalid service name %q", raw)
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		if _, ok := known[name]; !ok {
			return nil, fmt.Errorf("service %s not found", name)
		}
		out = append(out, name)
	}
	return out, nil
}

func (s *Server) actionAddService(w http.ResponseWriter, r *http.Request) {
	name := strings.ToLower(strings.TrimSpace(r.FormValue("name")))
	if !config.ValidateServiceName(name) {
		http.Error(w, "Некорректное имя сервиса", http.StatusBadRequest)
		return
	}
	if err := s.syncer.AddServiceWithConfig(r.Context(), name, config.ServiceOverride{}); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.auditChange(r, "web", "service_add", name)
	w.Header().Set("HX-Refresh", "true")
	writeOK(w, map[string]string{"service": name})
}

func (s *Server) actionDeleteService(w http.ResponseWriter, r *http.Request) {
	name := r.FormValue("name")
	purge := r.FormValue("purge") == "on" || r.FormValue("purge") == "1"
	if err := s.syncer.RemoveService(r.Context(), name, purge); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.auditChange(r, "web", "service_delete", name)
	w.Header().Set("HX-Refresh", "true")
	writeOK(w, map[string]string{"service": name})
}

func (s *Server) actionUpdateSchedule(w http.ResponseWriter, r *http.Request) {
	service := r.FormValue("service")
	spec := r.FormValue("schedule")
	if err := s.applySchedule(r, service, spec); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("HX-Refresh", "true")
	writeOK(w, map[string]string{"service": service, "schedule": spec})
}

func (s *Server) actionUpdateSettings(w http.ResponseWriter, r *http.Request) {
	user := s.currentUser(r)
	if err := r.ParseForm(); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid form body")
		return
	}

	type update struct{ key, value string }
	var updates []update
	for key, values := range r.PostForm {
		if key == "csrf_token" || len(values) == 0 {
			continue
		}
		// Маркер очистки секрета: checkbox clear:<key> в форме.
		if strings.HasPrefix(key, "clear:") {
			continue
		}
		value := values[0]
		if IsSecretPartial(key) && (value == "" || strings.HasPrefix(value, "\u2022")) {
			// Пустое/маска без маркера очистки — оставить как есть.
			if r.PostForm.Get("clear:"+key) == "" {
				continue
			}
			value = ""
		}
		updates = append(updates, update{key, value})
	}

	// Клон для валидации — маршалинг под чтением.
	s.cfg.RLock()
	cand, err := s.cloneConfig()
	s.cfg.RUnlock()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "clone config: "+err.Error())
		return
	}
	var errs []string
	for _, u := range updates {
		if err := cand.Set(u.key, u.value); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", u.key, err))
		}
	}
	if len(errs) == 0 {
		if err := cand.Validate(); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if len(errs) > 0 {
		writeErr(w, http.StatusBadRequest, "не сохранено — ошибки: "+strings.Join(errs, "; "))
		return
	}

	// Применение под записью с откатом старых значений: ранний return
	// оставлял бы полуприменённый конфиг (память != файл). Страховка от
	// паники в reflect-пути: мьютекс не должен остаться захваченным.
	// Разблокировка явная до applyRuntimeChanges (он берёт RLock сам).
	s.cfg.Lock()
	unlocked := false
	defer func() {
		if !unlocked {
			s.cfg.Unlock()
		}
	}()
	type backup struct {
		key string
		old any
	}
	var backed []backup
	applyErr := ""
	for _, u := range updates {
		old, gerr := s.cfg.GetPath(u.key)
		if gerr != nil {
			applyErr = fmt.Sprintf("backup %s: %v", u.key, gerr)
			break
		}
		backed = append(backed, backup{key: u.key, old: old})
		if serr := s.cfg.Set(u.key, u.value); serr != nil {
			applyErr = fmt.Sprintf("apply %s: %v", u.key, serr)
			break
		}
	}
	if applyErr == "" {
		if serr := s.cfg.Save(); serr != nil {
			applyErr = fmt.Sprintf("failed to save config: %v", serr)
		}
	}
	if applyErr != "" {
		for i := len(backed) - 1; i >= 0; i-- {
			_ = s.cfg.Set(backed[i].key, backed[i].old)
		}
		writeErr(w, http.StatusInternalServerError, "не применено, откачено: "+applyErr)
		return
	}
	s.cfg.Unlock()
	unlocked = true

	s.applyRuntimeChanges()

	s.auditChange(r, "web", "settings_update", fmt.Sprintf("%d keys", len(updates)))
	s.log.Info("settings updated", "user", user, "changed_keys", len(updates))
	telegramChanged := false
	for _, u := range updates {
		if strings.HasPrefix(u.key, "telegram.") {
			telegramChanged = true
			break
		}
	}
	resp := map[string]any{"updated": len(updates)}
	if telegramChanged && !s.syncer.TelegramRestartAvailable() {
		resp["warning"] = "настройки Telegram сохранены, но бот в web-режиме не запущен — перезапустите демона для применения"
	}
	w.Header().Set("HX-Refresh", "true")
	writeOK(w, resp)
}

// actionUpdateCDNSources — сохранение таблицы источников CDN из настроек.
func (s *Server) actionUpdateCDNSources(w http.ResponseWriter, r *http.Request) {
	user := s.currentUser(r)
	if err := r.ParseForm(); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid form body")
		return
	}
	services := r.PostForm["cdn_service"]
	names := r.PostForm["cdn_name"]
	asns := r.PostForm["cdn_asn"]
	urls := r.PostForm["cdn_url"]
	parsers := r.PostForm["cdn_parser"]
	n := len(services)
	if len(names) != n || len(asns) != n || len(urls) != n || len(parsers) != n {
		writeErr(w, http.StatusBadRequest, "источники: несогласованный набор полей")
		return
	}
	list := make([]config.CDNSourceConfig, 0, n)
	for i := 0; i < n; i++ {
		svc := strings.TrimSpace(services[i])
		nm := strings.TrimSpace(names[i])
		asnS := strings.TrimSpace(asns[i])
		u := strings.TrimSpace(urls[i])
		// Полностью пустая строка — заготовка UI, пропускаем.
		if svc == "" && nm == "" && asnS == "" && u == "" {
			continue
		}
		asn, err := strconv.Atoi(asnS)
		if err != nil {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("источник %q: ASN должен быть числом", svc))
			return
		}
		list = append(list, config.CDNSourceConfig{
			Service: svc, Name: nm, ASN: asn, URL: u, Parser: strings.TrimSpace(parsers[i]),
		})
	}
	// Пустая таблица = вернуться к встроенным дефолтам (как в setDefaults):
	// иначе CDN-сервисы остались бы без источника.
	if len(list) == 0 {
		list = config.DefaultCDNSources()
	}

	// Клон для валидации — маршалинг под чтением.
	s.cfg.RLock()
	cand, err := s.cloneConfig()
	s.cfg.RUnlock()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "clone config: "+err.Error())
		return
	}
	cand.CDNSources = list
	if err := cand.Validate(); err != nil {
		writeErr(w, http.StatusBadRequest, "не сохранено — ошибки: "+err.Error())
		return
	}

	// Применение под записью с откатом (как в actionUpdateSettings).
	s.cfg.Lock()
	old := s.cfg.CDNSources
	s.cfg.CDNSources = list
	if serr := s.cfg.Save(); serr != nil {
		s.cfg.CDNSources = old
		s.cfg.Unlock()
		writeErr(w, http.StatusInternalServerError, "не применено, откачено: "+serr.Error())
		return
	}
	s.cfg.Unlock()

	s.applyRuntimeChanges()
	s.auditChange(r, "web", "cdn_sources_update", fmt.Sprintf("%d entries", len(list)))
	s.log.Info("cdn sources updated", "user", user, "count", len(list))
	w.Header().Set("HX-Refresh", "true")
	writeOK(w, map[string]any{"updated": len(list)})
}

func (s *Server) cloneConfig() (*config.Config, error) {
	data, err := yaml.Marshal(s.cfg)
	if err != nil {
		return nil, err
	}
	clone := &config.Config{}
	if err := yaml.Unmarshal(data, clone); err != nil {
		return nil, err
	}
	return clone, nil
}

// ИЗМЕНЕНО: добавлен вызов RestartTelegramBot
func (s *Server) applyRuntimeChanges() {
	s.cfg.RLock()
	logLevel := s.cfg.Logging.Level
	telegramCfg := s.cfg.Telegram
	timezone := s.cfg.Timezone
	s.cfg.RUnlock()
	logging.SetLevel(logLevel)

	if err := s.syncer.ApplyConfig(); err != nil {
		s.log.Error("failed to apply config to clients", "err", err)
	}

	// Нотификатор пересоздаётся, если менялись telegram.* настройки.
	s.syncer.SetNotifier(notifier.FromConfig(telegramCfg, timezone, s.log))

	// Перезапуск Telegram-бота при изменении настроек telegram.*
	if err := s.syncer.RestartTelegramBot(); err != nil {
		s.log.Warn("failed to restart telegram bot", "err", err)
	}

	if err := s.syncer.ReloadScheduler(); err != nil {
		s.log.Warn("failed to reload scheduler", "err", err)
	}
}

func (s *Server) actionLogsClear(w http.ResponseWriter, r *http.Request) {
	// Чистим активный файл (а не из конфига: путь могли поменять через UI
	// без рестарта — логгер пишет в старый).
	file := logging.ActivePath()
	if file == "" {
		s.cfg.RLock()
		file = s.cfg.Logging.File
		s.cfg.RUnlock()
	}
	if file == "" {
		http.Error(w, "logging to file is disabled", http.StatusBadRequest)
		return
	}
	// Через механизм ротации: прямое truncate оставляло счётчик размера
	// lumberjack прежним (преждевременный rotate) и меняло права файла.
	if err := logging.RotateLogFile(file); err != nil {
		s.log.Warn("log rotate on clear failed, falling back to truncate", "err", err)
		if terr := os.WriteFile(file, nil, 0o600); terr != nil {
			http.Error(w, terr.Error(), http.StatusInternalServerError)
			return
		}
	}
	s.auditChange(r, "web", "logs_clear", file)
	writeOK(w, map[string]bool{"cleared": true})
}

// ============================================================================
// Lifecycle / audit
// ============================================================================

func (s *Server) auditChange(r *http.Request, source, action, subject string) {
	user := "unknown"
	if r != nil {
		user = s.currentUser(r)
	}
	s.log.Info("web change", "audit_action", action, "user", user, "source", source, "subject", subject)
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.httpSrv.Handler.ServeHTTP(w, r)
}

func (s *Server) Start(ctx context.Context) error {
	if !s.cfg.Web.Enabled {
		s.log.Info("web server disabled by config")
		return nil
	}
	s.log.Info("starting web server", "addr", s.cfg.Web.Listen)

	errCh := make(chan error, 1)
	go func() {
		if err := s.httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	select {
	case <-ctx.Done():
		s.log.Info("shutting down web server")
		s.hub.Stop()
		s.limiter.StopEvictTicker()
		s.sessions.Stop()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return s.httpSrv.Shutdown(shutdownCtx)
	case err := <-errCh:
		s.hub.Stop()
		s.limiter.StopEvictTicker()
		s.sessions.Stop()
		return err
	}
}

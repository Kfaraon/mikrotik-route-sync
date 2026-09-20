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
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Kfaraon/mikrotik-route-sync/internal/config"
	"github.com/Kfaraon/mikrotik-route-sync/internal/core"
	"github.com/Kfaraon/mikrotik-route-sync/internal/logging"
	"github.com/Kfaraon/mikrotik-route-sync/internal/notifier"
	"github.com/Kfaraon/mikrotik-route-sync/internal/version"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/gorilla/websocket"
	"golang.org/x/time/rate"
)

//go:embed templates/* static/*
var webFS embed.FS

// ============================================================================
// WebSocket Hub
// ============================================================================

// wsClient — соединение с последовательной записью.
type wsClient struct {
	conn *websocket.Conn
	send chan []byte
	mu   sync.Mutex
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

// Hub — набор WebSocket-клиентов для live-обновлений.
type Hub struct {
	mu      sync.RWMutex
	clients map[*wsClient]bool
}

// NewHub создаёт пустой hub.
func NewHub() *Hub { return &Hub{clients: map[*wsClient]bool{}} }

// Broadcast отправляет JSON-событие всем клиентам.
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
		}
	}
}

var wsUpgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool {
		origin := r.Header.Get("Origin")
		if origin == "" {
			return true
		}
		return origin == "//"+r.Host || strings.HasPrefix(origin, "http://"+r.Host) || strings.HasPrefix(origin, "https://"+r.Host)
	},
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
}

func newSessionStore(ttl time.Duration) *sessionStore {
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	s := &sessionStore{data: map[string]*session{}, ttl: ttl}
	go s.gcLoop()
	return s
}

func (st *sessionStore) gcLoop() {
	for range time.Tick(10 * time.Minute) {
		now := time.Now()
		st.mu.Lock()
		for k, v := range st.data {
			if now.After(v.expires) {
				delete(st.data, k)
			}
		}
		st.mu.Unlock()
	}
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(b)
}

func (st *sessionStore) create(username string) (string, *session) {
	st.mu.Lock()
	defer st.mu.Unlock()
	id := randomHex(16)
	s := &session{username: username, csrf: randomHex(16), expires: time.Now().Add(st.ttl)}
	st.data[id] = s
	return id, s
}

// get возвращает сессию по id cookie.
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

// clientIP учитывает X-Forwarded-For только от trusted proxies.
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
				first := strings.TrimSpace(strings.Split(xff, ",")[0])
				if parsed := net.ParseIP(first); parsed != nil {
					return parsed
				}
			}
			break
		}
	}
	return ip
}

// ipLimiter — rate limit на IP.
type ipLimiter struct {
	mu       sync.Mutex
	limiters map[string]*rate.Limiter
	rate     rate.Limit
	burst    int
}

func newIPLimiter(rps int) *ipLimiter {
	if rps <= 0 {
		rps = 50
	}
	return &ipLimiter{limiters: map[string]*rate.Limiter{}, rate: rate.Limit(rps), burst: rps * 2}
}

func (l *ipLimiter) allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	lim, ok := l.limiters[ip]
	if !ok {
		lim = rate.NewLimiter(l.rate, l.burst)
		l.limiters[ip] = lim
	}
	return lim.Allow()
}

// ============================================================================
// Server
// ============================================================================

// Server — веб-сервер приложения.
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
}

// NewServer создаёт и настраивает веб-сервер.
func NewServer(cfg *config.Config, syncer *core.Syncer, log *slog.Logger) (*Server, error) {
	if log == nil {
		log = slog.Default()
	}

	funcs := template.FuncMap{
		"effectiveSchedule": cfg.EffectiveSchedule,
		"maskSecret":        Mask,
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

	// Live-события синхронизации -> WebSocket.
	syncer.SetEventHook(func(event string, data any) {
		s.hub.Broadcast(event, data)
	})

	router := chi.NewRouter()
	router.Use(middleware.Recoverer)
	router.Use(s.securityHeaders)
	router.Use(s.cidrMiddleware)
	router.Use(s.rateLimitMiddleware)
	router.Use(s.maxBodyMiddleware)

	// liveness без авторизации (Docker HEALTHCHECK)
	router.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "data": map[string]any{"status": "alive"}})
	})

	// readiness без авторизации: доступность RouterOS API и кэша
	router.Get("/readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		mtErr := s.syncer.PingMikroTik(ctx)
		payload := apiEnvelope{OK: mtErr == nil, Data: map[string]any{
			"mikrotik": mtErr == nil,
			"cache":    s.syncer.Cache() != nil,
		}}
		if mtErr != nil {
			payload.Error = "mikrotik unreachable"
			writeJSON(w, http.StatusServiceUnavailable, payload)
			return
		}
		writeJSON(w, http.StatusOK, payload)
	})

	// API
	router.Route("/api/v1", func(r chi.Router) {
		r.Use(s.authMiddleware)
		r.Use(s.csrfMiddleware)

		r.Get("/status", s.apiStatus)
		r.Get("/services", s.apiServices)
		r.Post("/services", s.apiAddService)
		r.Delete("/services/{name}", s.apiDeleteService)
		r.Post("/services/sync", s.apiSyncMany)
		r.Post("/services/{name}/sync", s.apiSyncOne)
		r.Post("/services/{name}/dry-run", s.apiDryRun)
		r.Get("/schedules", s.apiSchedules)
		r.Put("/schedules/{service}", s.apiUpdateSchedule)
		r.Get("/logs", s.apiLogs)
		r.Get("/history", s.apiHistory)
		r.Get("/ws", s.hub.handleWS)
	})

	// Pages (тоже под auth)
	pages := router.With(s.authMiddleware)
	pages.Get("/", s.pageDashboard)
	pages.Get("/services", s.pageServices)
	pages.Get("/schedules", s.pageSchedules)
	pages.Get("/settings", s.pageSettings)
	pages.Get("/logs", s.pageLogs)
	pages.Get("/partials/status", s.partialStatus)

	// HTMX actions
	pages.Route("/actions", func(r chi.Router) {
		r.Use(s.csrfMiddleware)
		r.Post("/sync-all", s.actionSyncAll)
		r.Post("/sync-selected", s.actionSyncSelected)
		r.Post("/service-add", s.actionAddService)
		r.Post("/service-delete", s.actionDeleteService)
		r.Post("/schedule", s.actionUpdateSchedule)
		r.Post("/settings", s.actionUpdateSettings)
		r.Post("/logs-clear", s.actionLogsClear)
	})

	// Static
	static, _ := fs.Sub(webFS, "static")
	router.Handle("/static/*", http.StripPrefix("/static/", http.FileServer(http.FS(static))))

	s.httpSrv = &http.Server{
		Addr:              cfg.Web.Listen,
		Handler:           router,
		ReadTimeout:       10 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      0, // WebSocket требует открытых долгих записей; per-write deadlines ставятся в hub
		IdleTimeout:       120 * time.Second,
	}
	return s, nil
}

// ============================================================================
// Middleware
// ============================================================================

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.Web.SecurityHeaders {
			w.Header().Set("Content-Security-Policy",
				"default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self' ws: wss:; frame-ancestors 'none'")
			w.Header().Set("X-Frame-Options", "DENY")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			w.Header().Set("Referrer-Policy", "same-origin")
			if r.TLS != nil {
				w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
			}
		}
		next.ServeHTTP(w, r)
	})
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
		if r.Method == http.MethodPost || r.Method == http.MethodPut {
			r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		}
		next.ServeHTTP(w, r)
	})
}

// authMiddleware — Basic Auth + session cookie.
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.cfg.Web.Auth.Enabled {
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
		userOK := subtle.ConstantTimeCompare([]byte(user), []byte(s.cfg.Web.Auth.Username)) == 1
		passOK := subtle.ConstantTimeCompare([]byte(pass), []byte(s.cfg.Web.Auth.Password)) == 1
		if !ok || !(userOK && passOK) {
			if ok {
				s.log.Warn("web auth failed", "user", user, "remote", r.RemoteAddr)
			}
			w.Header().Set("WWW-Authenticate", `Basic realm="mikrotik-route-sync", charset="UTF-8"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		id, sess := s.sessions.create(user)
		http.SetCookie(w, &http.Cookie{
			Name:     "mrs_session",
			Value:    id,
			Path:     "/",
			HttpOnly: true,
			SameSite: http.SameSiteStrictMode,
			Expires:  sess.expires,
		})
		// Request authenticated via Basic header is inherently CSRF-safe
		// (browsers do not attach Authorization to cross-site requests),
		// so csrfMiddleware will skip token check for it.
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

// csrfMiddleware — двойная проверка токена для изменяющих запросов.
func (s *Server) csrfMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.cfg.Web.CSRFEnabled || !s.cfg.Web.Auth.Enabled {
			next.ServeHTTP(w, r)
			return
		}
		if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}

		// Basic-auth (header) requests are CSRF-exempt; cookie sessions must
		// present the matching X-CSRF-Token / csrf_token.
		if si := sessionFrom(r); si != nil && si.basicAuth {
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

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, apiEnvelope{OK: false, Error: msg})
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
	data["Timezone"] = s.cfg.Timezone
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
	return map[string]any{
		"MikroTikStatus": map[bool]string{true: "ok", false: "error"}[s.syncer.PingMikroTik(ctx) == nil],
		"ServiceCount":   len(s.cfg.Services),
		"Version":        version.Version,
		"Uptime":         time.Since(s.syncer.StartTime()).Truncate(time.Second).String(),
		"LastSync":       s.syncer.LastSync().Format(time.RFC3339),
		"Degraded":       s.syncer.DegradedServices(),
	}
}

func (s *Server) pageDashboard(w http.ResponseWriter, r *http.Request) {
	data := s.statusData(r)
	data["Services"] = s.cfg.Services
	s.render(w, r, "dashboard.html", data)
}

func (s *Server) partialStatus(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "status.html", s.statusData(r))
}

type serviceRow struct {
	Name      string
	Schedule  string
	Routes    int
	Overrides bool
}

func (s *Server) pageServices(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "services.html", map[string]any{"Services": s.serviceRows(r)})
}

func (s *Server) serviceRows(r *http.Request) []serviceRow {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	rows := make([]serviceRow, 0, len(s.cfg.Services))
	for _, name := range s.cfg.Services {
		row := serviceRow{
			Name:      name,
			Schedule:  s.cfg.EffectiveSchedule(name),
			Overrides: s.hasOverride(name),
		}
		if routes, err := s.syncer.Backup(ctx, name); err == nil {
			row.Routes = len(routes)
		}
		rows = append(rows, row)
	}
	return rows
}

func (s *Server) hasOverride(name string) bool {
	_, ok := s.cfg.Overrides[name]
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
	rows := make([]scheduleRow, 0, len(s.cfg.Services))
	for _, name := range s.cfg.Services {
		row := scheduleRow{Name: name, Effective: s.cfg.EffectiveSchedule(name)}
		if sv, ok := s.cfg.Schedules.Services[name]; ok {
			row.ServiceOv = sv.Schedule
		}
		for gname, g := range s.cfg.Schedules.Groups {
			for _, gs := range g.Services {
				if gs == name {
					row.Group = gname
					row.GroupSched = g.Schedule
				}
			}
		}
		rows = append(rows, row)
	}
	s.render(w, r, "schedules.html", map[string]any{
		"Global": s.cfg.Schedules.Global,
		"Rows":   rows,
	})
}

// ============================================================================
// Страница настроек: полное описание всех параметров config.yaml
// ============================================================================

// settingSpec — описание одного настраиваемого параметра.
type settingSpec struct {
	Group string // название секции на странице
	Key   string // точечный путь в конфиге
	Title string // русская подпись
	Type  string // text|number|float|bool|duration|secret|list|level
	Help  string // русское описание для подсказки "?"
}

// settingsSpec покрывает все поля конфигурации из config.example.yaml.
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
		"443 для HTTPS, 80 для HTTP. Применяется сразу."},
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
	{"MikroTik", "mikrotik.gateway", "Шлюз маршрутов", "text",
		"Имя интерфейса-шлюза в RouterOS (например wg-cz-vpn), записывается в создаваемые маршруты."},
	{"MikroTik", "mikrotik.routing_table", "Таблица маршрутизации", "text",
		"Routing table RouterOS (main или отдельная)."},
	{"MikroTik", "mikrotik.distance", "Distance", "number",
		"Приоритет маршрута (меньше = предпочтительнее), обычно 2."},
	{"MikroTik", "mikrotik.comment_prefix", "Префикс комментария", "text",
		"Пометка управляемых маршрутов: AUTO:<service>. Изоляция сервисов опирается на точное совпадение комментария."},
	{"MikroTik", "mikrotik.rate_limit", "Лимит запросов/сек", "number",
		"Ограничение частоты запросов к RouterOS REST API (по умолчанию 20)."},

	{"Telegram", "telegram.enabled", "Включить Telegram", "bool",
		"Уведомления и команды бота. Нотификатор пересоздаётся сразу; отдельно запущенному 'app bot' может понадобиться рестарт."},
	{"Telegram", "telegram.bot_token", "Токен бота", "secret",
		"Выдаётся @BotFather. ENV MRS_TELEGRAM_BOT_TOKEN имеет приоритет."},
	{"Telegram", "telegram.chat_id", "Chat ID для уведомлений", "text",
		"Сюда шлются отчёты. Узнать свой ID: напишите боту и посмотрите предупреждение в логе ('unauthorized telegram access')."},
	{"Telegram", "telegram.authorized_chat_ids", "Авторизованные chat_id", "list",
		"Список через запятую. Только эти chat_id могут управлять ботом; остальным бот не отвечает."},
	{"Telegram", "telegram.rate_limit", "Сообщений/сек на чат", "number",
		"Антифлуд бота (по умолчанию 1)."},

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
		"Хост-маршруты из dynamic-сбора. По умолчанию false — одиночные IP не записываются."},
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
}

// settingRow — отрисовка одной строки формы.
type settingRow struct {
	Key     string
	Title   string
	Type    string
	Value   string
	Help    string
	Options []string // только для Type=="level"
}

// settingGroup — секция страницы настроек.
type settingGroup struct {
	Title string
	Rows  []settingRow
}

// settingValue читает текущее значение параметра через GetPath и
// приводит его к строке для формы.
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
		"Groups": s.settingGroups(),
	})
}

func (s *Server) pageLogs(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "logs.html", map[string]any{"LogFile": s.cfg.Logging.File})
}

// ============================================================================
// REST API /api/v1 (envelope ok/data/error)
// ============================================================================

func (s *Server) apiStatus(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	writeOK(w, map[string]any{
		"version":     version.Version,
		"uptime":      time.Since(s.syncer.StartTime()).String(),
		"last_sync":   s.syncer.LastSync(),
		"mikrotik_ok": s.syncer.PingMikroTik(ctx) == nil,
		"services":    s.cfg.Services,
		"degraded":    s.syncer.DegradedServices(),
		"schedules":   s.cfg.Schedules,
	})
}

func (s *Server) apiServices(w http.ResponseWriter, r *http.Request) {
	writeOK(w, map[string]any{
		"services": s.serviceRows(r),
		"count":    len(s.cfg.Services),
	})
}

func (s *Server) apiAddService(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if !config.ValidateServiceName(req.Name) {
		writeErr(w, http.StatusBadRequest, "invalid service name")
		return
	}
	if err := s.syncer.AddServiceWithConfig(r.Context(), req.Name, config.ServiceOverride{}); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.auditChange(r, "api", "service_add", req.Name)
	writeOK(w, map[string]string{"service": req.Name})
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
	services := req.Services
	if len(services) == 0 {
		services = s.cfg.Services
	}
	runner := s.syncer
	ctx := context.WithoutCancel(r.Context())
	go func() {
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
	res, err := s.syncer.SyncService(context.WithoutCancel(r.Context()), name, req.DryRun, req.Force)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, apiEnvelope{OK: false, Data: res, Error: err.Error()})
		return
	}
	writeOK(w, res)
}

func (s *Server) apiDryRun(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	res, err := s.syncer.SyncService(r.Context(), name, true, false)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, apiEnvelope{OK: false, Data: res, Error: err.Error()})
		return
	}
	writeOK(w, res)
}

func (s *Server) apiSchedules(w http.ResponseWriter, r *http.Request) {
	eff := map[string]string{}
	for _, svc := range s.cfg.Services {
		eff[svc] = s.cfg.EffectiveSchedule(svc)
	}
	writeOK(w, map[string]any{
		"global":    s.cfg.Schedules.Global,
		"groups":    s.cfg.Schedules.Groups,
		"services":  s.cfg.Schedules.Services,
		"effective": eff,
	})
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
	if err := s.applySchedule(service, req.Schedule); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeOK(w, map[string]string{"service": service, "schedule": req.Schedule})
}

func (s *Server) applySchedule(service, spec string) error {
	if !config.ValidateServiceName(service) {
		return fmt.Errorf("invalid service name")
	}
	key := "schedules.services." + service + ".schedule"
	if err := s.cfg.Set(key, spec); err != nil {
		return err
	}
	if err := s.cfg.Save(); err != nil {
		return err
	}
	s.auditChange(nil, "web", "schedule_change", service)
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

	entries, err := s.syncer.GetLogs(r.Context(), limit*3)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	filtered := make([]map[string]any, 0, limit)
	for i := len(entries) - 1; i >= 0 && len(filtered) < limit; i-- {
		e := entries[i]
		if level != "" && !strings.EqualFold(fmt.Sprint(e["level"]), level) {
			continue
		}
		if service != "" && fmt.Sprint(e["service"]) != service {
			continue
		}
		if !since.IsZero() {
			if ts, ok := e["time"].(string); ok {
				if t, err := time.Parse(time.RFC3339, ts); err == nil && t.Before(since) {
					continue
				}
			}
		}
		filtered = append(filtered, e)
	}
	writeOK(w, filtered)
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
// HTMX actions (form-encoded)
// ============================================================================

func (s *Server) actionSyncAll(w http.ResponseWriter, r *http.Request) {
	if len(s.cfg.Services) == 0 {
		_, _ = w.Write([]byte("<p class=\"muted\">Нет сервисов</p>"))
		return
	}
	ctx := context.WithoutCancel(r.Context())
	go func() { _ = s.syncer.SyncMany(ctx, s.cfg.Services, false, false) }()
	writeOK(w, map[string]any{"started": true, "services": s.cfg.Services})
}

func (s *Server) actionSyncSelected(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	services := r.PostForm["service"]
	if len(services) == 0 {
		writeErr(w, http.StatusBadRequest, "nothing selected")
		return
	}
	ctx := context.WithoutCancel(r.Context())
	go func() { _ = s.syncer.SyncMany(ctx, services, false, false) }()
	writeOK(w, map[string]any{"started": true, "services": services})
}

func (s *Server) actionAddService(w http.ResponseWriter, r *http.Request) {
	name := r.FormValue("name")
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
	if err := s.applySchedule(service, spec); err != nil {
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

	changed := 0
	var errs []string
	for key, values := range r.PostForm {
		if key == "csrf_token" || len(values) == 0 {
			continue
		}
		value := values[0]
		if IsSecretPartial(key) && (value == "" || strings.HasPrefix(value, "\u2022")) {
			continue
		}
		if err := s.cfg.Set(key, value); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", key, err))
			continue
		}
		changed++
	}

	if len(errs) > 0 {
		// Не сохраняем частично: либо все поля валидны, либо ничего.
		writeErr(w, http.StatusBadRequest, "не сохранено — ошибки: "+strings.Join(errs, "; "))
		return
	}
	if err := s.cfg.Save(); err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to save config: "+err.Error())
		return
	}

	// Авто-применение сохранённых настроек без перезапуска процесса.
	s.applyRuntimeChanges()

	s.log.Info("settings updated", "user", user, "changed_keys", changed)
	w.Header().Set("HX-Refresh", "true")
	writeOK(w, map[string]any{"updated": changed})
}

// applyRuntimeChanges переносит только что сохранённую конфигурацию
// в живые компоненты: уровень логов, клиенты MikroTik/HTTP/резолвер,
// нотификатор Telegram и расписания планировщика.
// Поля, требующие рестарта (web.listen, logging.file, cache_path),
// в подсказках UI помечены явно.
func (s *Server) applyRuntimeChanges() {
	logging.SetLevel(s.cfg.Logging.Level)

	if err := s.syncer.ApplyConfig(); err != nil {
		s.log.Error("failed to apply config to clients", "err", err)
	}

	// Нотификатор пересоздаётся, если менялись telegram.* настройки.
	s.syncer.SetNotifier(notifier.FromConfig(s.cfg.Telegram, s.log))

	if err := s.syncer.ReloadScheduler(); err != nil {
		s.log.Warn("failed to reload scheduler", "err", err)
	}
}

func (s *Server) actionLogsClear(w http.ResponseWriter, r *http.Request) {
	file := s.cfg.Logging.File
	if file == "" {
		http.Error(w, "logging to file is disabled", http.StatusBadRequest)
		return
	}
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
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

// ServeHTTP предоставляет доступ к внутреннему роутеру (для httptest).
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.httpSrv.Handler.ServeHTTP(w, r)
}

// Start запускает HTTP-сервер и работает до отмены ctx (graceful shutdown).
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
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return s.httpSrv.Shutdown(shutdownCtx)
	case err := <-errCh:
		return err
	}
}

package web

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Kfaraon/mikrotik-route-sync/internal/config"
	"github.com/Kfaraon/mikrotik-route-sync/internal/core"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/gorilla/websocket"
	"golang.org/x/time/rate"
)

//go:embed templates/*.html static/*
var assetsFS embed.FS

// ============================== WebSocket Hub ==============================

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin:     func(r *http.Request) bool { return true },
}

// Hub управляет активными WebSocket-соединениями и рассылает события.
type Hub struct {
	mu      sync.RWMutex
	clients map[*websocket.Conn]struct{}
}

func NewHub() *Hub { return &Hub{clients: map[*websocket.Conn]struct{}{}} }

func (h *Hub) HandleWS(w http.ResponseWriter, r *http.Request) {
	c, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		slog.Warn("ws upgrade failed", "err", err)
		return
	}
	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.mu.Unlock()

	go func() {
		defer func() {
			h.mu.Lock()
			delete(h.clients, c)
			h.mu.Unlock()
			_ = c.Close()
		}()
		for {
			if _, _, err := c.ReadMessage(); err != nil {
				return
			}
		}
	}()
}

func (h *Hub) Broadcast(v any) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for c := range h.clients {
		if err := c.WriteJSON(v); err != nil {
			_ = c.Close()
			delete(h.clients, c)
		}
	}
}

// ============================== IP Rate Limiter ==============================

type ipLimiter struct {
	mu      sync.Mutex
	buckets map[string]*rate.Limiter
	limit   rate.Limit
	burst   int
}

func newIPLimiter(rps float64, burst int) *ipLimiter {
	return &ipLimiter{
		buckets: map[string]*rate.Limiter{},
		limit:   rate.Limit(rps),
		burst:   burst,
	}
}

func (l *ipLimiter) allow(ip string) bool {
	l.mu.Lock()
	b, ok := l.buckets[ip]
	if !ok {
		b = rate.NewLimiter(l.limit, l.burst)
		l.buckets[ip] = b
	}
	l.mu.Unlock()
	return b.Allow()
}

// ============================== Server ==============================

// Server — HTTP/WS сервер приложения (Web UI + REST API).
type Server struct {
	cfg        *config.Config
	cfgPath    string
	syncer     *core.Syncer
	log        *slog.Logger
	csrf       string
	tmpl       *template.Template
	hub        *Hub
	httpServer *http.Server
	limiter    *ipLimiter
}

// New создаёт новый экземпляр Server.
func New(cfg *config.Config, s *core.Syncer, log *slog.Logger, cfgPath string) *Server {
	token := make([]byte, 32)
	_, _ = rand.Read(token)

	tmpl := template.Must(template.New("").Funcs(template.FuncMap{
		// Маскирование секретов в шаблонах
		"mask": func(v string) string { return Mask(v) },
		"isSecret": func(key string) bool {
			return IsSecret(key) || IsSecretPartial(key)
		},
		"effectiveSchedule": func(svc string) string {
			return cfg.EffectiveSchedule(svc)
		},
		"json": func(v any) template.JS {
			b, _ := json.Marshal(v)
			return template.JS(b)
		},
		"lower": strings.ToLower,
	}).ParseFS(assetsFS, "templates/*.html"))

	return &Server{
		cfg:     cfg,
		cfgPath: cfgPath,
		syncer:  s,
		log:     log,
		csrf:    base64.RawURLEncoding.EncodeToString(token),
		tmpl:    tmpl,
		hub:     NewHub(),
		limiter: newIPLimiter(10, 20),
	}
}

// ============================== Middleware helpers ==============================

func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (s *Server) allowedIP(r *http.Request) bool {
	if len(s.cfg.Web.AllowedCIDRs) == 0 {
		return true
	}
	ip := net.ParseIP(remoteIP(r))
	if ip == nil {
		return false
	}
	for _, c := range s.cfg.Web.AllowedCIDRs {
		_, n, err := net.ParseCIDR(c)
		if err == nil && n.Contains(ip) {
			return true
		}
	}
	return false
}

// ============================== Middleware ==============================

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		w.Header().Set("Content-Security-Policy",
			"default-src 'self'; script-src 'self' 'unsafe-inline'; "+
				"style-src 'self' 'unsafe-inline'; img-src 'self' data:; "+
				"connect-src 'self' ws: wss:;")
		w.Header().Set("X-XSS-Protection", "0")
		w.Header().Del("Server")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) authAndAllow(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		if !s.allowedIP(r) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if s.cfg.Web.Auth.Enabled {
			u, p, ok := r.BasicAuth()
			if !ok ||
				subtle.ConstantTimeCompare([]byte(u), []byte(s.cfg.Web.Auth.Username)) != 1 ||
				subtle.ConstantTimeCompare([]byte(p), []byte(s.cfg.Web.Auth.Password)) != 1 {
				w.Header().Set("WWW-Authenticate", `Basic realm="mrs"`)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) csrfGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
			if s.cfg.Web.CSRFEnabled {
				tok := r.Header.Get("X-CSRF-Token")
				if tok == "" {
					tok = r.FormValue("csrf_token")
				}
				if subtle.ConstantTimeCompare([]byte(tok), []byte(s.csrf)) != 1 {
					http.Error(w, "csrf", http.StatusForbidden)
					return
				}
			}
			if r.ContentLength > 1<<20 {
				http.Error(w, "too large", http.StatusRequestEntityTooLarge)
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) rateLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.limiter.allow(remoteIP(r)) {
			http.Error(w, "too many requests", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ============================== Router ==============================

func (s *Server) routes() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Compress(5))
	r.Use(securityHeaders)
	r.Use(s.authAndAllow)
	r.Use(s.rateLimit)
	r.Use(s.csrfGuard)

	if sub, err := fs.Sub(assetsFS, "static"); err == nil {
		r.Handle("/static/*", http.StripPrefix("/static/", http.FileServer(http.FS(sub))))
	}

	// Pages
	r.Get("/", s.pageDashboard)
	r.Get("/services", s.pageServices)
	r.Get("/schedules", s.pageSchedules)
	r.Get("/settings", s.pageSettings)
	r.Get("/logs", s.pageLogs)

	// Partials (HTMX)
	r.Get("/partials/status", s.partialStatus)
	r.Get("/partials/services", s.partialServices)
	r.Get("/partials/schedules", s.partialSchedules)
	r.Get("/partials/logs", s.partialLogs)

	// HTMX actions
	r.Post("/actions/sync-all", s.actionSyncAll)
	r.Post("/actions/sync-selected", s.actionSyncSelected)

	// REST API
	r.Route("/api/v1", func(r chi.Router) {
		r.Get("/healthz", s.apiHealthz)
		r.Get("/readyz", s.apiReadyz)
		r.Get("/status", s.apiStatus)
		r.Get("/services", s.apiListServices)
		r.Post("/services/sync", s.apiSyncAll)
		r.Post("/services/{name}/sync", s.apiSyncService)
		r.Post("/services/{name}/dry-run", s.apiDryRun)
		r.Delete("/services/{name}", s.apiDeleteService)
		r.Get("/services/{name}", s.apiGetService)
		r.Get("/schedules", s.apiListSchedules)
		r.Put("/schedules/{service}", s.apiUpdateSchedule)
		r.Get("/logs", s.apiLogs)
		r.Get("/history", s.apiHistory)
		r.Get("/audit", s.apiAudit)
		r.Get("/ws", s.hub.HandleWS)
	})

	return r
}

// ============================== Run / Shutdown ==============================

// Run запускает HTTP-сервер и блокирует до ctx.Done() или фатальной ошибки.
func (s *Server) Run(ctx context.Context) error {
	s.httpServer = &http.Server{
		Addr:              s.cfg.Web.Listen,
		Handler:           s.routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	go func() {
		<-ctx.Done()
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = s.httpServer.Shutdown(c)
	}()

	s.log.Info("web listening", "addr", s.cfg.Web.Listen)
	err := s.httpServer.ListenAndServe()
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

// ============================== Page Handlers ==============================

func (s *Server) pageDashboard(w http.ResponseWriter, r *http.Request) {
	snap, _ := s.syncer.Snapshot(r.Context())
	routeCount := 0
	for _, c := range snap {
		routeCount += c
	}
	status := "ok"
	if err := s.syncer.PingMikroTik(r.Context()); err != nil {
		status = "error"
	}
	s.render(w, "dashboard.html", map[string]any{
		"Timezone":       s.cfg.Timezone,
		"CSRF":           s.csrf,
		"Services":       s.cfg.Services,
		"ServiceCount":   len(s.cfg.Services),
		"RouteCount":     routeCount,
		"MikroTikStatus": status,
	})
}

func (s *Server) pageServices(w http.ResponseWriter, r *http.Request) {
	s.render(w, "services.html", map[string]any{
		"CSRF":     s.csrf,
		"Services": s.cfg.Services,
	})
}

func (s *Server) pageSchedules(w http.ResponseWriter, r *http.Request) {
	s.render(w, "schedules.html", map[string]any{
		"CSRF":     s.csrf,
		"Services": s.cfg.Services,
	})
}

func (s *Server) pageSettings(w http.ResponseWriter, r *http.Request) {
	s.render(w, "settings.html", map[string]any{
		"CSRF": s.csrf,
		"Cfg":  s.cfg,
	})
}

func (s *Server) pageLogs(w http.ResponseWriter, r *http.Request) {
	s.render(w, "logs.html", map[string]any{
		"CSRF":  s.csrf,
		"Lines": "Загрузка…",
	})
}

func (s *Server) render(w http.ResponseWriter, name string, data any) {
	if data == nil {
		data = map[string]any{}
	}
	if err := s.tmpl.ExecuteTemplate(w, name, data); err != nil {
		s.log.Error("template render", "name", name, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

// ============================== Partials ==============================

func (s *Server) partialStatus(w http.ResponseWriter, r *http.Request) {
	snap, _ := s.syncer.Snapshot(r.Context())
	routeCount := 0
	for _, c := range snap {
		routeCount += c
	}
	status := "ok"
	if err := s.syncer.PingMikroTik(r.Context()); err != nil {
		status = "error"
	}
	s.render(w, "status.html", map[string]any{
		"MikroTikStatus": status,
		"ServiceCount":   len(s.cfg.Services),
		"RouteCount":     routeCount,
	})
}

func (s *Server) partialServices(w http.ResponseWriter, r *http.Request) {
	s.render(w, "services.html", map[string]any{"Services": s.cfg.Services})
}

func (s *Server) partialSchedules(w http.ResponseWriter, r *http.Request) {
	s.render(w, "schedules.html", map[string]any{"Services": s.cfg.Services})
}

func (s *Server) partialLogs(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(w, "Логи доступны в stdout/файле. Tail через /api/v1/ws.\n")
}

// ============================== HTMX Actions ==============================

func (s *Server) actionSyncAll(w http.ResponseWriter, r *http.Request) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		s.syncer.SyncMany(ctx, s.cfg.Services, false)
		s.hub.Broadcast(map[string]string{"type": "sync_done"})
	}()
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) actionSyncSelected(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	selected := r.Form["service"]
	if len(selected) == 0 {
		http.Redirect(w, r, "/services", http.StatusSeeOther)
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		s.syncer.SyncMany(ctx, selected, false)
		s.hub.Broadcast(map[string]string{"type": "sync_done"})
	}()
	http.Redirect(w, r, "/services", http.StatusSeeOther)
}

// ============================== REST API ==============================

func (s *Server) writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) apiHealthz(w http.ResponseWriter, _ *http.Request) {
	s.writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) apiReadyz(w http.ResponseWriter, r *http.Request) {
	if err := s.syncer.PingMikroTik(r.Context()); err != nil {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"status": "not_ready", "error": err.Error(),
		})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func (s *Server) apiStatus(w http.ResponseWriter, r *http.Request) {
	mtOK := s.syncer.PingMikrotik(r.Context()) == nil
	snap, _ := s.syncer.Snapshot(r.Context())
	routeCount := 0
	for _, c := range snap {
		routeCount += c
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"mikrotik":    mtOK,
		"time":        time.Now().Format(time.RFC3339),
		"services":    len(s.cfg.Services),
		"route_count": routeCount,
	})
}

func (s *Server) apiListServices(w http.ResponseWriter, _ *http.Request) {
	services := make([]map[string]any, 0, len(s.cfg.Services))
	for _, svc := range s.cfg.Services {
		services = append(services, map[string]any{
			"name":     svc,
			"schedule": s.cfg.EffectiveSchedule(svc),
		})
	}
	sort.Slice(services, func(i, j int) bool {
		return services[i]["name"].(string) < services[j]["name"].(string)
	})
	s.writeJSON(w, http.StatusOK, services)
}

func (s *Server) apiGetService(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	routes, err := s.syncer.Backup(r.Context(), name)
	if err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"name":     name,
		"schedule": s.cfg.EffectiveSchedule(name),
		"routes":   len(routes),
	})
}

func (s *Server) apiSyncAll(w http.ResponseWriter, r *http.Request) {
	dry := strings.EqualFold(r.URL.Query().Get("dry"), "true")
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		s.syncer.SyncMany(ctx, s.cfg.Services, dry)
	}()
	s.writeJSON(w, http.StatusAccepted, map[string]string{
		"status": "started", "dry": fmt.Sprint(dry),
	})
}

func (s *Server) apiSyncService(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	dry := strings.EqualFold(r.URL.Query().Get("dry"), "true")
	force := strings.EqualFold(r.URL.Query().Get("force"), "true")
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		_, _ = s.syncer.SyncService(ctx, name, dry, force)
	}()
	s.writeJSON(w, http.StatusAccepted, map[string]string{
		"status": "started", "service": name,
		"dry": fmt.Sprint(dry), "force": fmt.Sprint(force),
	})
}

func (s *Server) apiDryRun(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	res, err := s.syncer.SyncOneResult(r.Context(), name, true)
	if err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	s.writeJSON(w, http.StatusOK, res)
}

func (s *Server) apiDeleteService(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if err := s.syncer.RemoveService(r.Context(), name, true); err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	out := s.cfg.Services[:0]
	for _, x := range s.cfg.Services {
		if x != name {
			out = append(out, x)
		}
	}
	s.cfg.Services = out
	if err := s.cfg.Save(); err != nil {
		s.log.Error("save config after delete", "err", err)
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]string{"status": "deleted", "service": name})
}

func (s *Server) apiListSchedules(w http.ResponseWriter, _ *http.Request) {
	m := make(map[string]string, len(s.cfg.Services))
	for _, svc := range s.cfg.Services {
		m[svc] = s.cfg.EffectiveSchedule(svc)
	}
	s.writeJSON(w, http.StatusOK, m)
}

func (s *Server) apiUpdateSchedule(w http.ResponseWriter, r *http.Request) {
	service := chi.URLParam(r, "service")
	var req struct {
		Schedule string `json:"schedule"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	key := "schedules.services." + service + ".schedule"
	if err := s.cfg.Set(key, req.Schedule); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := s.cfg.Save(); err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]string{
		"status": "updated", "service": service, "schedule": req.Schedule,
	})
}

func (s *Server) apiLogs(w http.ResponseWriter, _ *http.Request) {
	s.writeJSON(w, http.StatusOK, map[string]any{
		"logs": []string{},
		"hint": "log streaming через /api/v1/ws или файл логов",
	})
}

func (s *Server) apiHistory(w http.ResponseWriter, _ *http.Request) {
	s.writeJSON(w, http.StatusOK, map[string]any{"history": []any{}})
}

// apiAudit возвращает статистику обращений к секретам (только для администратора).
func (s *Server) apiAudit(w http.ResponseWriter, _ *http.Request) {
	s.writeJSON(w, http.StatusOK, AuditStats())
}

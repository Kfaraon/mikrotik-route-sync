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
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/Kfaraon/mikrotik-route-sync/internal/config"
	"github.com/Kfaraon/mikrotik-route-sync/internal/core"
	"github.com/Kfaraon/mikrotik-route-sync/internal/scheduler"
	"github.com/go-chi/chi/v5"
	"github.com/gorilla/websocket"
)

//go:embed assets/*
var assets embed.FS

type Server struct {
	cfg        *config.Config
	syncer     *core.Syncer
	scheduler  *scheduler.Scheduler
	log        *slog.Logger
	configPath string
	csrf       string
	tmpl       *template.Template
	up         websocket.Upgrader
	mu         sync.Mutex
	clients    map[*websocket.Conn]struct{}
}
type pageData struct {
	Title    string
	Services []string
	Timezone string
	CSRF     string
}

func Run(ctx context.Context, cfg *config.Config, syncer *core.Syncer, sched *scheduler.Scheduler, log *slog.Logger, configPath string) error {
	if !cfg.Web.Enabled {
		return fmt.Errorf("web interface is disabled in config")
	}
	s, err := New(cfg, syncer, sched, log, configPath)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Addr:              cfg.Web.Listen,
		Handler:           s.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       90 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	select {
	case <-ctx.Done():
		shutdown, _ := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutdown.Done()
		return srv.Shutdown(shutdown)
	case err := <-errCh:
		if err == http.ErrServerClosed {
			return nil
		}
		return err
	}
}
func New(cfg *config.Config, syncer *core.Syncer, sched *scheduler.Scheduler, log *slog.Logger, path string) (*Server, error) {
	t, err := template.ParseFS(assets, "assets/*.html")
	if err != nil {
		return nil, err
	}
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return &Server{cfg: cfg, syncer: syncer, scheduler: sched, log: log, configPath: path, csrf: base64.RawURLEncoding.EncodeToString(b), tmpl: t, up: websocket.Upgrader{CheckOrigin: sameOrigin}, clients: map[*websocket.Conn]struct{}{}}, nil
}
func (s *Server) routes() http.Handler {
	r := chi.NewRouter()
	r.Use(s.lanOnly, s.basicAuth)
	r.Get("/", s.page("dashboard.html", "Dashboard"))
	r.Get("/services", s.page("services.html", "Services"))
	r.Get("/schedules", s.page("schedules.html", "Schedules"))
	r.Get("/settings", s.page("settings.html", "Settings"))
	r.Get("/logs", s.page("logs.html", "Logs"))
	r.Get("/api/v1/status", s.status)
	r.Get("/api/v1/services", s.services)
	r.Post("/api/v1/services/sync", s.syncMany)
	r.Post("/api/v1/services/{name}/sync", s.syncOne)
	r.Get("/api/v1/schedules", s.schedules)
	r.Put("/api/v1/schedules/{name}", s.putSchedule)
	r.Get("/api/v1/logs", s.logs)
	r.Get("/api/v1/ws", s.ws)
	return r
}
func (s *Server) page(name, title string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = s.tmpl.ExecuteTemplate(w, name, pageData{Title: title, Services: s.cfg.Services, Timezone: s.cfg.Timezone, CSRF: s.csrf})
	}
}
func (s *Server) basicAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.cfg.Web.Auth.Enabled {
			next.ServeHTTP(w, r)
			return
		}
		u, p, ok := r.BasicAuth()
		userOK := subtle.ConstantTimeCompare([]byte(u), []byte(s.cfg.Web.Auth.Username)) == 1
		passOK := subtle.ConstantTimeCompare([]byte(p), []byte(s.cfg.Web.Auth.Password)) == 1
		if !ok || !userOK || !passOK {
			w.Header().Set("WWW-Authenticate", `Basic realm="mikrotik-route-sync"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
func (s *Server) lanOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(s.cfg.Web.AllowedCIDRs) == 0 {
			next.ServeHTTP(w, r)
			return
		}
		host, _, _ := net.SplitHostPort(r.RemoteAddr)
		ip := net.ParseIP(host)
		allowed := false
		for _, cidr := range s.cfg.Web.AllowedCIDRs {
			_, n, e := net.ParseCIDR(cidr)
			if e == nil && n.Contains(ip) {
				allowed = true
				break
			}
		}
		if !allowed {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}
func (s *Server) requireCSRF(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("X-CSRF-Token") != s.csrf {
		http.Error(w, "invalid csrf token", http.StatusForbidden)
		return false
	}
	return true
}
func jsonOut(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	jsonOut(w, s.syncer.Status(r.Context()))
}
func (s *Server) services(w http.ResponseWriter, r *http.Request) {
	jsonOut(w, map[string]any{"services": s.cfg.Services})
}
func (s *Server) syncMany(w http.ResponseWriter, r *http.Request) {
	if !s.requireCSRF(w, r) {
		return
	}
	var p struct {
		Services []string `json:"services"`
		DryRun   bool     `json:"dry_run"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil && err != io.EOF {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if len(p.Services) == 0 {
		p.Services = append([]string(nil), s.cfg.Services...)
	}
	for _, name := range p.Services {
		if !s.cfg.HasService(name) {
			http.Error(w, "unknown service: "+name, http.StatusBadRequest)
			return
		}
	}
	services := append([]string(nil), p.Services...)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer cancel()
		_ = s.syncer.SyncMany(ctx, services, p.DryRun)
		s.broadcast("status")
	}()
	w.WriteHeader(http.StatusAccepted)
	jsonOut(w, map[string]any{"accepted": true})
}
func (s *Server) syncOne(w http.ResponseWriter, r *http.Request) {
	if !s.requireCSRF(w, r) {
		return
	}
	name := config.NormalizeService(chi.URLParam(r, "name"))
	if !s.cfg.HasService(name) {
		http.Error(w, "unknown service", http.StatusNotFound)
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer cancel()
		_, _ = s.syncer.SyncOneResult(ctx, name, false)
		s.broadcast("status")
	}()
	w.WriteHeader(http.StatusAccepted)
	jsonOut(w, map[string]any{"accepted": true, "service": name})
}
func (s *Server) schedules(w http.ResponseWriter, r *http.Request) {
	m := map[string]string{}
	for _, v := range s.cfg.Services {
		m[v] = s.cfg.ScheduleFor(v)
	}
	jsonOut(w, m)
}
func (s *Server) putSchedule(w http.ResponseWriter, r *http.Request) {
	if !s.requireCSRF(w, r) {
		return
	}
	name := config.NormalizeService(chi.URLParam(r, "name"))
	var p struct {
		Schedule string `json:"schedule"`
	}
	if json.NewDecoder(r.Body).Decode(&p) != nil || strings.TrimSpace(p.Schedule) == "" || !scheduler.ValidSpec(p.Schedule) {
		http.Error(w, "bad or unsupported schedule", http.StatusBadRequest)
		return
	}
	if !s.cfg.HasService(name) {
		http.Error(w, "unknown service", http.StatusNotFound)
		return
	}
	s.cfg.Schedules.Services[name] = config.ServiceSchedule{Schedule: p.Schedule}
	if err := s.cfg.Save(); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if err := s.scheduler.Reload(); err != nil {
		s.log.Error("scheduler reload failed", "err", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonOut(w, map[string]any{"ok": true})
}
func (s *Server) logs(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Logging.File == "" {
		jsonOut(w, map[string]string{"logs": "stdout only"})
		return
	}
	f, err := os.Open(s.cfg.Logging.File)
	if err != nil {
		http.Error(w, err.Error(), 404)
		return
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 2<<20))
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	jsonOut(w, map[string]string{"logs": string(b)})
}
func (s *Server) ws(w http.ResponseWriter, r *http.Request) {
	c, err := s.up.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	s.mu.Lock()
	s.clients[c] = struct{}{}
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.clients, c); s.mu.Unlock(); c.Close() }()
	for {
		if _, _, err := c.ReadMessage(); err != nil {
			return
		}
	}
}
func (s *Server) broadcast(event string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for c := range s.clients {
		_ = c.WriteJSON(map[string]string{"event": event})
	}
}

func sameOrigin(r *http.Request) bool {
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

var _ = fmt.Sprintf

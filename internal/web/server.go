package web

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/Kfaraon/mikrotik-route-sync/internal/config"
	"github.com/Kfaraon/mikrotik-route-sync/internal/core"
	"github.com/Kfaraon/mikrotik-route-sync/internal/scheduler"
)

//go:embed templates/*.html
var templatesFS embed.FS
//go:embed static/*
var staticFS embed.FS

type Server struct {
	cfg *config.Config; syncer *core.Syncer; sched *scheduler.Scheduler
	log *slog.Logger; tpl *template.Template; configPath string
}

func Run(ctx context.Context, cfg *config.Config, syncer *core.Syncer, sched *scheduler.Scheduler, log *slog.Logger, configPath string) error {
	if !cfg.Web.Enabled { <-ctx.Done(); return nil }

	tpl := template.Must(template.ParseFS(templatesFS, "templates/*.html"))
	s := &Server{cfg: cfg, syncer: syncer, sched: sched, log: log, tpl: tpl, configPath: configPath}

	r := chi.NewRouter()
	r.Use(middleware.Recoverer, middleware.RealIP)
	r.Handle("/static/*", http.StripPrefix("/static/", http.FileServer(http.FS(staticFS))))

	auth := func(next http.Handler) http.Handler { return next }
	if cfg.Web.Auth.Enabled {
		auth = func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				u, p, ok := r.BasicAuth()
				if !ok || u != cfg.Web.Auth.Username || p != cfg.Web.Auth.Password {
					w.Header().Set("WWW-Authenticate", `Basic realm="mikrotik-route-sync"`)
					http.Error(w, "Unauthorized", http.StatusUnauthorized); return
				}
				next.ServeHTTP(w, r)
			})
		}
	}

	r.Group(func(r chi.Router) {
		r.Use(auth)
		r.Get("/", s.pageIndex); r.Get("/settings", s.pageSettings); r.Get("/logs", s.pageLogs)
		r.Get("/partials/status", s.partialStatus); r.Get("/partials/services", s.partialServices)
		r.Get("/partials/schedules", s.partialSchedules); r.Get("/partials/settings", s.partialSettings); r.Get("/partials/logs", s.partialLogs)
		r.Group(func(r chi.Router) {
			r.Use(csrfMiddleware)
			r.Post("/actions/sync-all", s.actionSyncAll); r.Post("/actions/sync-selected", s.actionSyncSelected)
			r.Post("/actions/sync-one/{name}", s.actionSyncOne); r.Post("/actions/config/set", s.actionConfigSet)
		})
	})

	srv := &http.Server{Addr: cfg.Web.Listen, Handler: r, ReadHeaderTimeout: 10 * time.Second}
	go func() { <-ctx.Done(); shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second); defer cancel(); srv.Shutdown(shutdownCtx) }()
	
	s.log.Info("web server started", "listen", cfg.Web.Listen)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed { return err }
	return nil
}

func csrfMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == http.MethodHead { next.ServeHTTP(w, r); return }
		cookie, err := r.Cookie("csrf_token")
		if err != nil { http.Error(w, "missing CSRF", http.StatusForbidden); return }
		token := r.Header.Get("X-CSRF-Token"); if token == "" { token = r.FormValue("csrf_token") }
		if token == "" || token != cookie.Value { http.Error(w, "invalid CSRF", http.StatusForbidden); return }
		next.ServeHTTP(w, r)
	})
}

func ensureCSRFCookie(w http.ResponseWriter, r *http.Request) string {
	if c, err := r.Cookie("csrf_token"); err == nil { return c.Value }
	b := make([]byte, 32); rand.Read(b); v := hex.EncodeToString(b)
	http.SetCookie(w, &http.Cookie{Name: "csrf_token", Value: v, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: 86400})
	return v
}

func (s *Server) pageIndex(w http.ResponseWriter, r *http.Request) { s.tpl.ExecuteTemplate(w, "index.html", map[string]any{"Timezone": s.cfg.Timezone, "CSRF": ensureCSRFCookie(w, r)}) }
func (s *Server) pageSettings(w http.ResponseWriter, r *http.Request) { s.tpl.ExecuteTemplate(w, "settings.html", map[string]any{"CSRF": ensureCSRFCookie(w, r), "MikroTik": s.cfg.MikroTik, "Telegram": s.cfg.Telegram, "Web": s.cfg.Web, "Timezone": s.cfg.Timezone}) }
func (s *Server) pageLogs(w http.ResponseWriter, r *http.Request) { s.tpl.ExecuteTemplate(w, "logs.html", map[string]any{"CSRF": ensureCSRFCookie(w, r)}) }

func (s *Server) partialStatus(w http.ResponseWriter, r *http.Request) {
	total := 0; for _, svc := range s.cfg.Services { routes, _ := s.syncer.ListRoutes(r.Context(), svc); total += len(routes) }
	st := "ok"; if err := s.syncer.PingMikroTik(r.Context()); err != nil { st = "err" }
	s.tpl.ExecuteTemplate(w, "status.html", map[string]any{"MikroTikStatus": st, "ServiceCount": len(s.cfg.Services), "RouteCount": total})
}

func (s *Server) partialServices(w http.ResponseWriter, r *http.Request) {
	type row struct { Name, Schedule string; RouteCount int }
	rows := make([]row, 0, len(s.cfg.Services))
	for _, svc := range s.cfg.Services { routes, _ := s.syncer.ListRoutes(r.Context(), svc); rows = append(rows, row{Name: svc, RouteCount: len(routes), Schedule: s.cfg.ScheduleFor(svc)}) }
	s.tpl.ExecuteTemplate(w, "services.html", map[string]any{"Services": rows, "CSRF": ensureCSRFCookie(w, r)})
}

func (s *Server) partialSchedules(w http.ResponseWriter, r *http.Request) {
	type row struct { Service, Schedule string }
	rows := make([]row, 0, len(s.cfg.Services))
	for _, svc := range s.cfg.Services { rows = append(rows, row{Service: svc, Schedule: s.cfg.ScheduleFor(svc)}) }
	s.tpl.ExecuteTemplate(w, "schedules.html", map[string]any{"Schedules": rows})
}

func (s *Server) partialLogs(w http.ResponseWriter, r *http.Request) {
	buf, _ := os.ReadFile(s.cfg.Logging.File); lines := strings.Split(string(buf), "\n")
	if len(lines) > 200 { lines = lines[len(lines)-200:] }
	s.tpl.ExecuteTemplate(w, "logs.html", map[string]any{"Lines": strings.Join(lines, "\n")})
}

func (s *Server) actionSyncAll(w http.ResponseWriter, r *http.Request) { go func() { s.syncer.SyncMany(context.Background(), s.cfg.Services, false) }(); w.Write([]byte(`<p class="status-ok">Синхронизация всех сервисов запущена.</p>`)) }
func (s *Server) actionSyncSelected(w http.ResponseWriter, r *http.Request) {
	r.ParseForm(); services := r.Form["service"]
	if len(services) == 0 { w.Write([]byte(`<p class="status-err">Не выбрано ни одного сервиса.</p>`)); return }
	go func() { s.syncer.SyncMany(context.Background(), services, false) }(); w.Write([]byte(`<p class="status-ok">Запущено: ` + strings.Join(services, ", ") + `</p>`))
}
func (s *Server) actionSyncOne(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name"); go func() { s.syncer.SyncOne(context.Background(), name, false) }(); w.Write([]byte(`<p class="status-ok">Запущено: ` + name + `</p>`))
}
func (s *Server) actionConfigSet(w http.ResponseWriter, r *http.Request) {
	r.ParseForm(); key, value := r.FormValue("key"), r.FormValue("value")
	if key == "" { http.Error(w, "missing key", http.StatusBadRequest); return }
	if err := s.cfg.Set(key, value); err != nil { http.Error(w, err.Error(), http.StatusBadRequest); return }
	if err := s.cfg.Save(); err != nil { http.Error(w, err.Error(), http.StatusInternalServerError); return }
	w.Write([]byte(`<p class="status-ok">Сохранено: ` + key + `</p>`))
}

func writeJSON(w http.ResponseWriter, v any) { w.Header().Set("Content-Type", "application/json"); json.NewEncoder(w).Encode(v) }

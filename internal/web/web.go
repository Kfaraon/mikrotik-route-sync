package web

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/Kfaraon/mikrotik-route-sync/internal/config"
	"github.com/Kfaraon/mikrotik-route-sync/internal/core"
	"github.com/Kfaraon/mikrotik-route-sync/internal/scheduler"
)

//go:embed templates/*
var templatesFS embed.FS

type Server struct {
	cfg     *config.Config
	syncer  *core.Syncer
	sched   *scheduler.Scheduler
	log     *slog.Logger
	cfgPath string
	tmpl    *template.Template
}

func Run(ctx context.Context, cfg *config.Config, syncer *core.Syncer, sched *scheduler.Scheduler, log *slog.Logger, cfgPath string) error {
	s := &Server{
		cfg:     cfg,
		syncer:  syncer,
		sched:   sched,
		log:     log,
		cfgPath: cfgPath,
	}
	
	var err error
	s.tmpl, err = template.ParseFS(templatesFS, "templates/*.html")
	if err != nil {
		return fmt.Errorf("parse templates: %w", err)
	}
	
	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	
	if cfg.Web.Auth.Enabled {
		r.Use(basicAuth(cfg.Web.Auth.Username, cfg.Web.Auth.Password))
	}
	
	r.Get("/", s.handleDashboard)
	r.Get("/services", s.handleServices)
	r.Get("/schedules", s.handleSchedules)
	r.Get("/settings", s.handleSettings)
	r.Get("/logs", s.handleLogs)
	
	r.Route("/api/v1", func(r chi.Router) {
		r.Get("/status", s.apiStatus)
		r.Get("/services", s.apiListServices)
		r.Post("/services/sync", s.apiSyncAll)
		r.Post("/services/{name}/sync", s.apiSyncService)
		r.Get("/schedules", s.apiListSchedules)
		r.Put("/schedules/{service}", s.apiUpdateSchedule)
		r.Get("/logs", s.apiLogs)
	})
	
	srv := &http.Server{
		Addr:         cfg.Web.Listen,
		Handler:      r,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}
	
	errCh := make(chan error, 1)
	go func() {
		log.Info("web server started", "addr", cfg.Web.Listen)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()
	
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	case err := <-errCh:
		return err
	}
}

func basicAuth(username, password string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			user, pass, ok := r.BasicAuth()
			if !ok || user != username || pass != password {
				w.Header().Set("WWW-Authenticate", `Basic realm="Restricted"`)
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	s.tmpl.ExecuteTemplate(w, "dashboard.html", s.cfg)
}

func (s *Server) handleServices(w http.ResponseWriter, r *http.Request) {
	s.tmpl.ExecuteTemplate(w, "services.html", s.cfg)
}

func (s *Server) handleSchedules(w http.ResponseWriter, r *http.Request) {
	s.tmpl.ExecuteTemplate(w, "schedules.html", s.cfg)
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	s.tmpl.ExecuteTemplate(w, "settings.html", s.cfg)
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	s.tmpl.ExecuteTemplate(w, "logs.html", s.cfg)
}

func (s *Server) apiStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	err := s.syncer.PingMikroTik(ctx)
	
	status := map[string]any{
		"mikrotik": err == nil,
		"time":     time.Now().Format(time.RFC3339),
	}
	
	json.NewEncoder(w).Encode(status)
}

func (s *Server) apiListServices(w http.ResponseWriter, r *http.Request) {
	snap, _ := s.syncer.Snapshot(r.Context())
	
	services := make([]map[string]any, 0, len(s.cfg.Services))
	for _, svc := range s.cfg.Services {
		services = append(services, map[string]any{
			"name":     svc,
			"schedule": s.cfg.ScheduleFor(svc),
			"routes":   snap[svc],
		})
	}
	
	json.NewEncoder(w).Encode(services)
}

func (s *Server) apiSyncAll(w http.ResponseWriter, r *http.Request) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		s.syncer.SyncMany(ctx, s.cfg.Services, false)
	}()
	
	json.NewEncoder(w).Encode(map[string]string{"status": "started"})
}

func (s *Server) apiSyncService(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		s.syncer.SyncOne(ctx, name, false)
	}()
	
	json.NewEncoder(w).Encode(map[string]string{"status": "started", "service": name})
}

func (s *Server) apiListSchedules(w http.ResponseWriter, r *http.Request) {
	schedules := make(map[string]string)
	for _, svc := range s.cfg.Services {
		schedules[svc] = s.cfg.ScheduleFor(svc)
	}
	
	json.NewEncoder(w).Encode(schedules)
}

func (s *Server) apiUpdateSchedule(w http.ResponseWriter, r *http.Request) {
	service := chi.URLParam(r, "service")
	
	var req struct {
		Schedule string `json:"schedule"`
	}
	
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	
	if err := s.cfg.Set(fmt.Sprintf("schedules.services.%s.schedule", service), req.Schedule); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	
	if err := s.cfg.Save(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func (s *Server) apiLogs(w http.ResponseWriter, r *http.Request) {
	// Простая заглушка — в реальности читать из файла логов
	json.NewEncoder(w).Encode(map[string]any{
		"logs":   []string{},
		"status": "ok",
	})
}

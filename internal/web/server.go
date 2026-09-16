package web

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/Kfaraon/mikrotik-route-sync/internal/config"
	"github.com/Kfaraon/mikrotik-route-sync/internal/core"
	"github.com/Kfaraon/mikrotik-route-sync/internal/logging"
	"github.com/Kfaraon/mikrotik-route-sync/internal/version"
)

//go:embed templates/* static/*
var webFS embed.FS

// Hub управляет WebSocket соединениями
type Hub struct {
	mu      sync.RWMutex
	clients map[*websocket.Conn]bool
}

func NewHub() *Hub {
	return &Hub{
		clients: make(map[*websocket.Conn]bool),
	}
}

// Broadcast отправляет сообщение всем подключенным клиентам
// ИСПРАВЛЕНО: используем Lock() вместо RLock() для безопасной модификации
func (h *Hub) Broadcast(v any) {
	h.mu.Lock()
	defer h.mu.Unlock()
	
	var failed []*websocket.Conn
	for c := range h.clients {
		if err := c.WriteJSON(v); err != nil {
			failed = append(failed, c)
			_ = c.Close()
		}
	}
	
	for _, c := range failed {
		delete(h.clients, c)
	}
}

// HandleWS обрабатывает новые WebSocket соединения
func (h *Hub) HandleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Upgrade(w, r, nil, 1024, 1024)
	if err != nil {
		http.Error(w, "websocket upgrade failed", http.StatusInternalServerError)
		return
	}

	h.mu.Lock()
	h.clients[conn] = true
	h.mu.Unlock()

	// Отправка приветственного сообщения
	welcome := map[string]any{
		"type":    "welcome",
		"message": "Connected to MikroTik Route Sync",
		"time":    time.Now().Format(time.RFC3339),
	}
	if err := conn.WriteJSON(welcome); err != nil {
		conn.Close()
		h.mu.Lock()
		delete(h.clients, conn)
		h.mu.Unlock()
		return
	}

	// Ожидание закрытия соединения
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			break
		}
	}

	conn.Close()
	h.mu.Lock()
	delete(h.clients, conn)
	h.mu.Unlock()
}

// Server представляет веб-сервер приложения
type Server struct {
	cfg    *config.Config
	syncer *core.Syncer
	log    *slog.Logger
	hub    *Hub
	tmpl   *template.Template
	mux    *http.ServeMux
	startTime time.Time
}

func NewServer(cfg *config.Config, syncer *core.Syncer, log *slog.Logger) (*Server, error) {
	hub := NewHub()

	// Загрузка шаблонов
	tmpl, err := template.ParseFS(webFS, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}

	s := &Server{
		cfg:       cfg,
		syncer:    syncer,
		log:       log,
		hub:       hub,
		tmpl:      tmpl,
		mux:       http.NewServeMux(),
		startTime: time.Now(),
	}

	s.setupRoutes()
	return s, nil
}

func (s *Server) setupRoutes() {
	// Статические файлы
	static, _ := fs.Sub(webFS, "static")
	s.mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(static))))

	// Страницы
	s.mux.HandleFunc("/", s.handleDashboard)
	s.mux.HandleFunc("/dashboard", s.handleDashboard)
	s.mux.HandleFunc("/logs", s.handleLogs)
	s.mux.HandleFunc("/status", s.handleStatus)
	s.mux.HandleFunc("/settings", s.handleSettings)
	s.mux.HandleFunc("/schedules", s.handleSchedules)
	s.mux.HandleFunc("/services", s.handleServices)

	// API endpoints
	s.mux.HandleFunc("/api/status", s.apiStatus)
	s.mux.HandleFunc("/api/sync", s.apiSync)
	s.mux.HandleFunc("/api/snapshot", s.apiSnapshot)
	s.mux.HandleFunc("/api/snapshots", s.apiSnapshots)
	s.mux.HandleFunc("/api/services", s.apiServices)
	s.mux.HandleFunc("/api/service/add", s.apiAddService)
	s.mux.HandleFunc("/api/service/delete", s.apiDeleteService)
	s.mux.HandleFunc("/api/ws", s.hub.HandleWS)
	s.mux.HandleFunc("/api/settings/update", s.apiUpdateSettings)
	s.mux.HandleFunc("/api/schedule/update", s.apiUpdateSchedule)
	s.mux.HandleFunc("/api/logs", s.apiLogs)
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	data := map[string]any{
		"Title":   "Dashboard",
		"Version": version.Version,
		"Services": s.cfg.Services,
	}
	s.renderTemplate(w, "dashboard.html", data)
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	data := map[string]any{
		"Title": "Logs",
	}
	s.renderTemplate(w, "logs.html", data)
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	data := map[string]any{
		"Title": "Status",
	}
	s.renderTemplate(w, "status.html", data)
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	data := map[string]any{
		"Title":   "Settings",
		"Config":  s.cfg,
	}
	s.renderTemplate(w, "settings.html", data)
}

func (s *Server) handleSchedules(w http.ResponseWriter, r *http.Request) {
	data := map[string]any{
		"Title":    "Schedules",
		"Config":   s.cfg,
		"Services": s.cfg.Services,
	}
	s.renderTemplate(w, "schedules.html", data)
}

func (s *Server) handleServices(w http.ResponseWriter, r *http.Request) {
	data := map[string]any{
		"Title":    "Services",
		"Services": s.cfg.Services,
	}
	s.renderTemplate(w, "services.html", data)
}

func (s *Server) renderTemplate(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, name, data); err != nil {
		s.log.Error("template render error", "template", name, "err", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
	}
}

func (s *Server) apiStatus(w http.ResponseWriter, r *http.Request) {
	// ИСПРАВЛЕНО: используем PingMikroTik вместо PingMikrotik
	mtOK := s.syncer.PingMikroTik(r.Context()) == nil

	status := map[string]any{
		"mikrotik":    mtOK,
		"services":    s.cfg.Services,
		"version":     version.Version,
		"uptime":      time.Since(s.startTime).String(),
		"last_sync":   s.syncer.LastSync(),
		"schedules":   s.cfg.Schedules,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(status)
}

func (s *Server) apiSync(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Service string `json:"service"`
		DryRun  bool   `json:"dry_run"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	if req.Service == "" {
		http.Error(w, "Service name is required", http.StatusBadRequest)
		return
	}

	result, err := s.syncer.SyncService(r.Context(), req.Service, req.DryRun, false)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

func (s *Server) apiSnapshot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Service string `json:"service"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	if req.Service == "" {
		http.Error(w, "Service name is required", http.StatusBadRequest)
		return
	}

	id, err := s.syncer.CreateSnapshot(r.Context(), req.Service, nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	response := map[string]string{
		"id":      id,
		"service": req.Service,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

func (s *Server) apiSnapshots(w http.ResponseWriter, r *http.Request) {
	service := r.URL.Query().Get("service")
	if service == "" {
		http.Error(w, "Service parameter is required", http.StatusBadRequest)
		return
	}

	snapshots := s.syncer.ListSnapshots(r.Context(), service)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(snapshots)
}

func (s *Server) apiServices(w http.ResponseWriter, r *http.Request) {
	services := s.cfg.Services

	response := map[string]any{
		"services": services,
		"count":    len(services),
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

func (s *Server) apiAddService(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	if req.Name == "" {
		http.Error(w, "Service name is required", http.StatusBadRequest)
		return
	}

	err := s.syncer.AddService(r.Context(), req.Name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	response := map[string]string{
		"status":  "success",
		"service": req.Name,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

func (s *Server) apiDeleteService(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Name       string `json:"name"`
		PurgeRoutes bool  `json:"purge_routes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	if req.Name == "" {
		http.Error(w, "Service name is required", http.StatusBadRequest)
		return
	}

	err := s.syncer.RemoveService(r.Context(), req.Name, req.PurgeRoutes)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	response := map[string]string{
		"status":  "success",
		"service": req.Name,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

func (s *Server) apiUpdateSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req map[string]string
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	for key, value := range req {
		if err := s.cfg.Set(key, value); err != nil {
			http.Error(w, fmt.Sprintf("Failed to set %s: %v", key, err), http.StatusBadRequest)
			return
		}
	}

	if err := s.cfg.Save(); err != nil {
		http.Error(w, fmt.Sprintf("Failed to save config: %v", err), http.StatusInternalServerError)
		return
	}

	response := map[string]string{
		"status": "success",
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

func (s *Server) apiUpdateSchedule(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Service  string `json:"service"`
		Schedule string `json:"schedule"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	if req.Service == "" || req.Schedule == "" {
		http.Error(w, "Service and schedule are required", http.StatusBadRequest)
		return
	}

	key := "schedules.services." + req.Service + ".schedule"
	if err := s.cfg.Set(key, req.Schedule); err != nil {
		http.Error(w, fmt.Sprintf("Failed to set schedule: %v", err), http.StatusBadRequest)
		return
	}

	if err := s.cfg.Save(); err != nil {
		http.Error(w, fmt.Sprintf("Failed to save config: %v", err), http.StatusInternalServerError)
		return
	}

	// Перезагружаем планировщик
	if err := s.syncer.ReloadScheduler(); err != nil {
		s.log.Warn("Failed to reload scheduler", "err", err)
	}

	response := map[string]string{
		"status":   "success",
		"service":  req.Service,
		"schedule": req.Schedule,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

func (s *Server) apiLogs(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if limitStr := r.URL.Query().Get("limit"); limitStr != "" {
		if parsed, err := strconv.Atoi(limitStr); err == nil && parsed > 0 {
			limit = parsed
		}
	}

	logs, err := s.syncer.GetLogs(r.Context(), limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(logs)
}

func (s *Server) Run(ctx context.Context) error {
	addr := fmt.Sprintf(":%d", s.cfg.Web.Port)
	server := &http.Server{
		Addr:         addr,
		Handler:      s,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	s.log.Info("starting web server", "addr", addr)

	errCh := make(chan error, 1)
	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	select {
	case <-ctx.Done():
		s.log.Info("shutting down web server")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return server.Shutdown(shutdownCtx)
	case err := <-errCh:
		return err
	}
}

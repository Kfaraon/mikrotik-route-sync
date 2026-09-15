package web

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
)

func (s *Server) apiHealthz(w http.ResponseWriter, _ *http.Request) {
	s.writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

func (s *Server) apiReadyz(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, http.StatusOK, map[string]any{"status": "ready"})
}

func (s *Server) apiStatus(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, http.StatusOK, map[string]any{"status": "running", "version": "dev"})
}

func (s *Server) apiListServices(w http.ResponseWriter, _ *http.Request) {
	s.writeJSON(w, http.StatusOK, map[string]any{"services": s.syncer.ListServices()})
}

func (s *Server) apiGetService(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	s.writeJSON(w, http.StatusOK, map[string]any{"service": name, "schedule": s.cfg.EffectiveSchedule(name)})
}

func (s *Server) apiSyncAll(w http.ResponseWriter, r *http.Request) {
	go func() {
		_ = s.syncer.SyncMany(r.Context(), s.cfg.Services, false)
	}()
	s.writeJSON(w, http.StatusAccepted, map[string]any{"status": "started"})
}

func (s *Server) apiSyncService(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	go func() {
		_, _ = s.syncer.SyncService(r.Context(), name, false, false)
	}()
	s.writeJSON(w, http.StatusAccepted, map[string]any{"status": "started", "service": name})
}

func (s *Server) apiDryRun(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	res, err := s.syncer.SyncService(r.Context(), name, true, false)
	if err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	s.writeJSON(w, http.StatusOK, res)
}

func (s *Server) apiDeleteService(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	err := s.syncer.RemoveService(r.Context(), name, true)
	if err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"status": "deleted", "service": name})
}

func (s *Server) apiListSchedules(w http.ResponseWriter, _ *http.Request) {
	schedules := make(map[string]string)
	for _, svc := range s.cfg.Services {
		schedules[svc] = s.cfg.EffectiveSchedule(svc)
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"schedules": schedules})
}

func (s *Server) apiUpdateSchedule(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "service")
	var req struct {
		Schedule string `json:"schedule"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json"})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"status": "updated", "service": name, "schedule": req.Schedule})
}

func (s *Server) apiLogs(w http.ResponseWriter, _ *http.Request) {
	s.writeJSON(w, http.StatusOK, map[string]any{"logs": []string{"log1", "log2"}})
}

func (s *Server) apiHistory(w http.ResponseWriter, _ *http.Request) {
	s.writeJSON(w, http.StatusOK, map[string]any{"history": []any{}})
}

func (s *Server) apiAudit(w http.ResponseWriter, _ *http.Request) {
	s.writeJSON(w, http.StatusOK, AuditStats())
}

func (s *Server) actionSyncAll(w http.ResponseWriter, r *http.Request) {
	go func() {
		_ = s.syncer.SyncMany(r.Context(), s.cfg.Services, false)
	}()
	w.Write([]byte(`<div class="toast">Sync started</div>`))
}

func (s *Server) actionSyncSelected(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	services := r.Form["services"]
	go func() {
		_ = s.syncer.SyncMany(r.Context(), services, false)
	}()
	w.Write([]byte(`<div class="toast">Sync started for selected</div>`))
}

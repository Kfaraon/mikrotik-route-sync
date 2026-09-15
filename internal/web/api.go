package web

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
)

func (s *Server) writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) apiListServices(w http.ResponseWriter, r *http.Request) {
	// Используем syncer для получения списка сервисов из конфига
	svcs := s.syncer.ListServices()
	s.writeJSON(w, http.StatusOK, svcs)
}

func (s *Server) apiSyncOne(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	
	// Используем SyncService вместо SyncOne
	if err := s.syncer.SyncService(r.Context(), name, false, false); err != nil {
		s.writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	
	s.writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "service": name})
}

func (s *Server) apiDeleteService(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	
	// Используем RemoveService с force=true для API
	if err := s.syncer.RemoveService(r.Context(), name, true); err != nil {
		s.writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) apiSetSchedule(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "service")
	var body struct {
		Schedule string `json:"schedule"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	
	// TODO: Реализовать SetServiceSchedule в syncer
	// Пока возвращаем ошибку
	s.writeJSON(w, http.StatusNotImplemented, map[string]string{
		"error": "SetServiceSchedule not implemented yet"
	})
}

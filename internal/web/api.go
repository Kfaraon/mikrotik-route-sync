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
	svcs := s.app.ListServices()
	s.writeJSON(w, http.StatusOK, svcs)
}

func (s *Server) apiSyncOne(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if err := s.app.SyncOne(r.Context(), name); err != nil {
		s.writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	s.hub.Broadcast(map[string]any{"type": "service_synced", "service": name})
	s.writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "service": name})
}

func (s *Server) apiDeleteService(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if err := s.app.RemoveService(r.Context(), name); err != nil {
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
	if err := s.app.SetServiceSchedule(name, body.Schedule); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

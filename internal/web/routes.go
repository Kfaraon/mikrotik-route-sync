package web

import (
	"net/http"

	"github.com/go-chi/chi/v5"
)

func (s *Server) Mount(r chi.Router) {
	r.Use(s.basicAuth)
	r.Use(s.csrfProtection)

	// UI
	r.Get("/", s.pageDashboard)
	r.Get("/services", s.pageServices)
	r.Get("/schedules", s.pageSchedules)
	r.Get("/settings", s.pageSettings)
	r.Get("/logs", s.pageLogs)

	// HTMX-партиалы
	r.Get("/partials/services", s.partialServices)
	r.Get("/partials/status", s.partialStatus)
	r.Get("/partials/schedules", s.partialSchedules)

	// REST API
	r.Route("/api/v1", func(r chi.Router) {
		r.Get("/status", s.apiStatus)
		r.Get("/services", s.apiListServices)
		r.Post("/services/sync", s.apiSyncAll)
		r.Post("/services/{name}/sync", s.apiSyncOne)
		r.Delete("/services/{name}", s.apiDeleteService)
		r.Get("/services/{name}", s.apiGetService)
		r.Get("/schedules", s.apiSchedules)
		r.Put("/schedules/{service}", s.apiSetSchedule)
		r.Get("/logs", s.apiLogs)
		r.Get("/ws", s.hub.HandleWS)
	})
}

package web

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/Kfaraon/mikrotik-route-sync/internal/config"
	"github.com/Kfaraon/mikrotik-route-sync/internal/core"
	"github.com/Kfaraon/mikrotik-route-sync/internal/mikrotik"
	"github.com/go-chi/chi/v5"
)

type Server struct {
	cfg    *config.Config
	syncer *core.Syncer
	mt     *mikrotik.Client
	log    *slog.Logger
	csrf   string
	tpl    *template.Template
	http   *http.Server
}

func New(cfg *config.Config, s *core.Syncer, log *slog.Logger) *Server {
	token := make([]byte, 32)
	_, _ = rand.Read(token)
	x := &Server{cfg: cfg, syncer: s, mt: mikrotik.New(cfg.MikroTik), log: log, csrf: base64.RawURLEncoding.EncodeToString(token)}
	x.tpl = template.Must(template.New("index").Parse(`<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width"><meta http-equiv="Content-Security-Policy" content="default-src 'self'; style-src 'self' 'unsafe-inline'"><title>MikroTik Route Sync</title><style>body{font:16px system-ui;margin:2rem;max-width:1100px}table{border-collapse:collapse;width:100%}td,th{padding:.5rem;border-bottom:1px solid #ddd}.ok{color:green}</style></head><body><h1>MikroTik Route Sync</h1><p>Secure service-isolated RouterOS route synchronization.</p><table><tr><th>Service</th><th>Schedule</th></tr>{{range .Services}}<tr><td>{{.}}</td><td>{{schedule .}}</td></tr>{{end}}</table></body></html>`))
	return x
}
func (s *Server) allowedIP(r *http.Request) bool {
	if len(s.cfg.Web.AllowedCIDRs) == 0 {
		return true
	}
	host, _, e := net.SplitHostPort(r.RemoteAddr)
	if e != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	for _, c := range s.cfg.Web.AllowedCIDRs {
		_, n, _ := net.ParseCIDR(c)
		if n != nil && n.Contains(ip) {
			return true
		}
	}
	return false
}
func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.allowedIP(r) && r.URL.Path != "/api/v1/healthz" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if r.URL.Path == "/api/v1/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		if s.cfg.Web.Auth.Enabled {
			u, p, ok := r.BasicAuth()
			if !ok || subtle.ConstantTimeCompare([]byte(u), []byte(s.cfg.Web.Auth.Username)) != 1 || subtle.ConstantTimeCompare([]byte(p), []byte(s.cfg.Web.Auth.Password)) != 1 {
				w.Header().Set("WWW-Authenticate", `Basic realm="mrs"`)
				http.Error(w, "unauthorized", 401)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
func security(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		w.Header().Del("Server")
		next.ServeHTTP(w, r)
	})
}
func (s *Server) csrfGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost || r.Method == http.MethodPut || r.Method == http.MethodPatch || r.Method == http.MethodDelete {
			if s.cfg.Web.CSRFEnabled && subtle.ConstantTimeCompare([]byte(r.Header.Get("X-CSRF-Token")), []byte(s.csrf)) != 1 {
				http.Error(w, "csrf", 403)
				return
			}
			if r.ContentLength > 1<<20 {
				http.Error(w, "too large", 413)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
func jsonOut(w http.ResponseWriter, status int, data any, errMsg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": errMsg == "", "data": data, "error": func() any {
		if errMsg == "" {
			return nil
		}
		return errMsg
	}()})
}
func (s *Server) routes() http.Handler {
	r := chi.NewRouter()
	r.Use(security, s.auth, s.csrfGuard)
	r.Get("/", func(w http.ResponseWriter, r *http.Request) {
		_ = s.tpl.Execute(w, map[string]any{"Services": s.cfg.Services, "Schedule": s.cfg.EffectiveSchedule})
	})
	r.Get("/api/v1/healthz", func(w http.ResponseWriter, r *http.Request) { jsonOut(w, 200, map[string]string{"status": "ok"}, "") })
	r.Get("/api/v1/readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, c := context.WithTimeout(r.Context(), 3*time.Second)
		defer c()
		if e := s.mt.Ping(ctx); e != nil {
			jsonOut(w, 503, nil, "mikrotik unavailable")
			return
		}
		jsonOut(w, 200, map[string]string{"status": "ready"}, "")
	})
	r.Get("/api/v1/status", func(w http.ResponseWriter, r *http.Request) {
		jsonOut(w, 200, map[string]any{"services": len(s.cfg.Services), "timezone": s.cfg.Timezone}, "")
	})
	r.Get("/api/v1/services", func(w http.ResponseWriter, r *http.Request) { jsonOut(w, 200, s.cfg.Services, "") })
	r.Post("/api/v1/services/{name}/dry-run", func(w http.ResponseWriter, r *http.Request) {
		res, e := s.syncer.SyncService(r.Context(), chi.URLParam(r, "name"), true, false)
		if e != nil {
			jsonOut(w, 422, nil, e.Error())
			return
		}
		jsonOut(w, 200, res, "")
	})
	r.Post("/api/v1/services/{name}/sync", func(w http.ResponseWriter, r *http.Request) {
		res, e := s.syncer.SyncService(r.Context(), chi.URLParam(r, "name"), false, false)
		if e != nil {
			jsonOut(w, 409, nil, e.Error())
			return
		}
		jsonOut(w, 200, res, "")
	})
	r.Get("/api/v1/schedules", func(w http.ResponseWriter, r *http.Request) {
		m := map[string]string{}
		for _, x := range s.cfg.Services {
			m[x] = s.cfg.EffectiveSchedule(x)
		}
		jsonOut(w, 200, m, "")
	})
	return r
}
func (s *Server) Run(ctx context.Context) error {
	s.http = &http.Server{Addr: s.cfg.Web.Listen, Handler: s.routes(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second}
	go func() {
		<-ctx.Done()
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = s.http.Shutdown(c)
	}()
	s.log.Info("web listening", "addr", s.cfg.Web.Listen)
	e := s.http.ListenAndServe()
	if e == http.ErrServerClosed {
		return nil
	}
	return e
}

var _ = fmt.Sprintf
var _ = strings.TrimSpace

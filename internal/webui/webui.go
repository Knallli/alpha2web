// Package webui serves the server-rendered web interface.
package webui

import (
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"sync"

	"github.com/knallli/alpha2web/internal/camera"
	"github.com/knallli/alpha2web/internal/health"
	"github.com/knallli/alpha2web/internal/logbuf"
)

//go:embed templates/*.html static/*
var assets embed.FS

var tmpl = template.Must(template.ParseFS(assets, "templates/*.html"))

var levels = map[string]slog.Level{"debug": slog.LevelDebug, "info": slog.LevelInfo, "warn": slog.LevelWarn, "error": slog.LevelError}

const csp = "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self'; form-action 'self'; frame-ancestors 'none'"

// Handler serves the web UI.
type Handler struct {
	Version  string
	Camera   *camera.Supervisor
	Logs     *logbuf.Ring
	Password string // optional HTTP basic auth password
	Logger   *slog.Logger
	Settings *Settings // nil hides the settings page
	CacheDir string    // thumbnail/preview cache; empty disables caching

	Extra func(io.Writer) // optional extra /metrics lines (plugins)

	csrfOnce sync.Once
	csrf     string
	galOnce  sync.Once
	gal      gallery
}

type page struct {
	Title, Nav, Version, CSRF string
}

func (h *Handler) pg(title, nav string) page { return page{title, nav, h.Version, h.csrfToken()} }

// csrfToken is random per process; every POST form carries it.
func (h *Handler) csrfToken() string {
	h.csrfOnce.Do(func() { h.csrf = rand.Text() })
	return h.csrf
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	hd := w.Header()
	hd.Set("X-Frame-Options", "DENY")
	hd.Set("X-Content-Type-Options", "nosniff")
	hd.Set("Content-Security-Policy", csp)
	open := r.URL.Path == "/healthz" || r.URL.Path == "/health" || r.URL.Path == "/metrics"
	if h.Password != "" && !open {
		_, pass, ok := r.BasicAuth()
		if !ok || subtle.ConstantTimeCompare([]byte(pass), []byte(h.Password)) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="alpha2web"`)
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
	}
	if r.Method == http.MethodPost && subtle.ConstantTimeCompare([]byte(r.FormValue("csrf")), []byte(h.csrfToken())) != 1 {
		http.Error(w, "missing or wrong CSRF token: reload the page and try again", http.StatusBadRequest)
		return
	}
	h.mux().ServeHTTP(w, r)
}

func (h *Handler) mux() *http.ServeMux {
	m := http.NewServeMux()
	m.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		h.render(w, "dashboard.html", struct {
			page
			S camera.Status
		}{h.pg("Dashboard", "dashboard"), h.Camera.Status()})
	})
	m.HandleFunc("GET /fragment/dashboard", func(w http.ResponseWriter, _ *http.Request) {
		h.render(w, "status", h.Camera.Status())
	})
	m.HandleFunc("GET /liveview", func(w http.ResponseWriter, _ *http.Request) {
		h.render(w, "liveview.html", h.pg("Live view", "liveview"))
	})
	m.HandleFunc("GET /liveview.mjpeg", h.liveStream)
	m.HandleFunc("GET /logs", func(w http.ResponseWriter, r *http.Request) {
		name, l := levelParam(r)
		h.render(w, "logs.html", struct {
			page
			Level   string
			Levels  []string
			Entries []logbuf.Entry
		}{h.pg("Logs", "logs"), name, []string{"debug", "info", "warn", "error"}, h.Logs.Since(0, l)})
	})
	m.HandleFunc("GET /fragment/logs", func(w http.ResponseWriter, r *http.Request) {
		_, l := levelParam(r)
		h.render(w, "logrows", h.Logs.Since(0, l))
	})
	m.HandleFunc("GET /api/status", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		writeJSON(w, h.Camera.Status())
	})
	// Liveness only: stays 200 while the camera is off, so Docker does not restart the container.
	m.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, "ok connected=%v\n", h.Camera.Status().Connected)
	})
	ops := &health.Handler{Source: h.Camera, Version: h.Version, Extra: h.Extra}
	m.HandleFunc("GET /health", ops.Health)
	m.HandleFunc("GET /metrics", ops.Metrics)
	if h.Settings != nil {
		m.HandleFunc("GET /settings", h.settingsPage)
		m.HandleFunc("POST /settings", h.saveSettings)
	}
	h.controlRoutes(m)
	h.filesRoutes(m)
	h.galleryRoutes(m)
	m.Handle("GET /static/", http.FileServerFS(assets))
	return m
}

func levelParam(r *http.Request) (string, slog.Level) {
	n := r.URL.Query().Get("level")
	if l, ok := levels[n]; ok {
		return n, l
	}
	return "info", slog.LevelInfo
}

func (h *Handler) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.ExecuteTemplate(w, name, data); err != nil {
		slog.Error("render", "tmpl", name, "err", err)
	}
}

func writeJSON(w http.ResponseWriter, v any) { json.NewEncoder(w).Encode(v) }

// Log returns the handler logger.
func (h *Handler) Log() *slog.Logger {
	if h.Logger != nil {
		return h.Logger
	}
	return slog.Default()
}

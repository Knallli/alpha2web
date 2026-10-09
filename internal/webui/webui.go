// Package webui serves the server-rendered web interface.
package webui

import (
	"crypto/subtle"
	"embed"
	"encoding/json"
	"html/template"
	"log/slog"
	"net/http"

	"github.com/knallli/alpha2web/internal/camera"
	"github.com/knallli/alpha2web/internal/logbuf"
)

//go:embed templates/*.html static/*
var assets embed.FS

var tmpl = template.Must(template.ParseFS(assets, "templates/*.html"))

var levels = map[string]slog.Level{"debug": slog.LevelDebug, "info": slog.LevelInfo, "warn": slog.LevelWarn, "error": slog.LevelError}

// Handler serves the web UI.
type Handler struct {
	Version  string
	Camera   *camera.Supervisor
	Logs     *logbuf.Ring
	Password string // optional HTTP basic auth password
}

type page struct {
	Title, Nav, Version string
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.Password != "" {
		_, pass, ok := r.BasicAuth()
		if !ok || subtle.ConstantTimeCompare([]byte(pass), []byte(h.Password)) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="alpha2web"`)
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
	}
	h.mux().ServeHTTP(w, r)
}

func (h *Handler) mux() *http.ServeMux {
	m := http.NewServeMux()
	m.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		h.render(w, "dashboard.html", struct {
			page
			S camera.Status
		}{page{"Dashboard", "dashboard", h.Version}, h.Camera.Status()})
	})
	m.HandleFunc("GET /fragment/dashboard", func(w http.ResponseWriter, _ *http.Request) {
		h.render(w, "status", h.Camera.Status())
	})
	m.HandleFunc("GET /liveview", func(w http.ResponseWriter, _ *http.Request) {
		h.render(w, "liveview.html", page{"Live view", "liveview", h.Version})
	})
	m.HandleFunc("GET /liveview.mjpeg", h.liveStream)
	m.HandleFunc("GET /logs", func(w http.ResponseWriter, r *http.Request) {
		name, l := levelParam(r)
		h.render(w, "logs.html", struct {
			page
			Level   string
			Levels  []string
			Entries []logbuf.Entry
		}{page{"Logs", "logs", h.Version}, name, []string{"debug", "info", "warn", "error"}, h.Logs.Since(0, l)})
	})
	m.HandleFunc("GET /fragment/logs", func(w http.ResponseWriter, r *http.Request) {
		_, l := levelParam(r)
		h.render(w, "logrows", h.Logs.Since(0, l))
	})
	m.HandleFunc("GET /api/status", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		writeJSON(w, h.Camera.Status())
	})
	m.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		if !h.Camera.Status().Connected {
			http.Error(w, "camera not connected", http.StatusServiceUnavailable)
		}
	})
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

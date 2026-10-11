// Package health serves the JSON health report and Prometheus metrics.
package health

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/knallli/alpha2web/internal/camera"
)

var started = time.Now()

// Source is what the report reads; *camera.Supervisor satisfies it.
type Source interface {
	Status() camera.Status
	Sessions() uint64
	Failures() uint64
	Restarts() uint64
}

// Handler provides the Health and Metrics endpoints.
type Handler struct {
	Source  Source
	Version string
	Extra   func(w io.Writer)     // optional, appends metric lines (plugins)
	Plugins func() map[string]any // optional, the "plugins" object of the report
}

type cameraInfo struct {
	Connected bool   `json:"connected"`
	Host      string `json:"host"`
	Model     string `json:"model"`
	Restarts  uint64 `json:"restarts"`
	LastError string `json:"last_error"`
}

type report struct {
	Status   string         `json:"status"`
	Problems []string       `json:"problems"`
	Version  string         `json:"version"`
	Camera   cameraInfo     `json:"camera"`
	Plugins  map[string]any `json:"plugins"`
}

// Health answers 200 with status "degraded" while a camera session that was in
// use has dropped, so monitoring sees it without a container restart. Not being
// connected is the normal idle state.
func (h *Handler) Health(w http.ResponseWriter, _ *http.Request) {
	st := h.Source.Status()
	rep := report{Status: "ok", Problems: []string{}, Version: h.Version, Plugins: map[string]any{},
		Camera: cameraInfo{st.Connected, st.Host, st.Model, h.Source.Restarts(), st.LastError}}
	if st.Reconnecting {
		rep.Status = "degraded"
		rep.Problems = append(rep.Problems, "camera connection lost, reconnecting")
	}
	if h.Plugins != nil {
		rep.Plugins = h.Plugins()
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(rep)
}

// Metrics writes the Prometheus text format.
func (h *Handler) Metrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	connected := 0
	if h.Source.Status().Connected {
		connected = 1
	}
	fmt.Fprintf(w, "# TYPE alpha2web_info gauge\nalpha2web_info{version=%q} 1\n", h.Version)
	fmt.Fprintf(w, "# TYPE alpha2web_camera_connected gauge\nalpha2web_camera_connected %d\n", connected)
	fmt.Fprintf(w, "# TYPE alpha2web_camera_sessions_total counter\nalpha2web_camera_sessions_total %d\n", h.Source.Sessions())
	fmt.Fprintf(w, "# TYPE alpha2web_camera_connect_failures_total counter\nalpha2web_camera_connect_failures_total %d\n", h.Source.Failures())
	fmt.Fprintf(w, "# TYPE alpha2web_uptime_seconds gauge\nalpha2web_uptime_seconds %.0f\n", time.Since(started).Seconds())
	if h.Extra != nil {
		h.Extra(w)
	}
}

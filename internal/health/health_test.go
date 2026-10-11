package health

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/knallli/alpha2web/internal/camera"
)

type fake struct{ st camera.Status }

func (f fake) Status() camera.Status { return f.st }
func (fake) Sessions() uint64        { return 3 }
func (fake) Failures() uint64        { return 2 }
func (fake) Restarts() uint64        { return 4 }

func TestHealthAndMetrics(t *testing.T) {
	h := &Handler{Source: fake{camera.Status{Reconnecting: true}}, Version: "v1", Extra: func(w io.Writer) { io.WriteString(w, "extra_metric 1\n") }}
	w := httptest.NewRecorder()
	h.Health(w, httptest.NewRequest("GET", "/health", nil))
	for _, s := range []string{`"status":"degraded"`, `"restarts":4`, `"version":"v1"`, "camera connection lost"} {
		if w.Code != 200 || !strings.Contains(w.Body.String(), s) {
			t.Errorf("%s: %d %s", s, w.Code, w.Body)
		}
	}
	for _, st := range []camera.Status{{Connected: true, Host: "h", Model: "m"}, {}} { // idle is fine
		h.Source = fake{st}
		w = httptest.NewRecorder()
		h.Health(w, httptest.NewRequest("GET", "/health", nil))
		if !strings.Contains(w.Body.String(), `"status":"ok"`) || !strings.Contains(w.Body.String(), `"problems":[]`) {
			t.Error(w.Body)
		}
	}
	h.Source = fake{camera.Status{Connected: true}}
	w = httptest.NewRecorder()
	h.Metrics(w, httptest.NewRequest("GET", "/metrics", nil))
	for _, s := range []string{`alpha2web_info{version="v1"} 1`, "alpha2web_camera_connected 1", "alpha2web_camera_sessions_total 3",
		"alpha2web_camera_connect_failures_total 2", "alpha2web_uptime_seconds ", "extra_metric 1"} {
		if !strings.Contains(w.Body.String(), s) {
			t.Errorf("missing %q in %s", s, w.Body)
		}
	}
}

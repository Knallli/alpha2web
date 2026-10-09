package webui

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/knallli/alpha2web/internal/backoff"
	"github.com/knallli/alpha2web/internal/camera"
	"github.com/knallli/alpha2web/internal/logbuf"
)

func TestPages(t *testing.T) {
	h := &Handler{Version: "t", Logs: logbuf.NewRing(10), Password: "pw",
		Camera: camera.New(camera.Config{Host: "1.2.3.4"}, backoff.Default, slog.Default())}
	get := func(path string, auth bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", path, nil)
		if auth {
			r.SetBasicAuth("", "pw")
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if get("/", false).Code != http.StatusUnauthorized {
		t.Fatal("want 401")
	}
	for path, want := range map[string]string{"/": "Searching", "/logs": "Logs", "/fragment/logs": "No log entries", "/static/style.css": "--bg"} {
		w := get(path, true)
		if w.Code != 200 || !strings.Contains(w.Body.String(), want) {
			t.Errorf("%s: %d %q", path, w.Code, w.Body.String())
		}
	}
	if get("/healthz", true).Code != http.StatusServiceUnavailable {
		t.Fatal("want 503 without camera")
	}
}

func TestLiveviewNotConnected(t *testing.T) {
	h := &Handler{Logs: logbuf.NewRing(1), Camera: camera.New(camera.Config{}, backoff.Default, slog.Default())}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/liveview.mjpeg", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatal(w.Code)
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/liveview", nil))
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
}

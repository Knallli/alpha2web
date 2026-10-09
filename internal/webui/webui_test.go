package webui

import (
	"context"
	"github.com/knallli/alpha2web/internal/settings"
	"github.com/knallli/alpha2web/plugin"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
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

func TestSettingsSave(t *testing.T) {
	store, _ := settings.Open(filepath.Join(t.TempDir(), "s.json"))
	var applied settings.Values
	h := &Handler{Logs: logbuf.NewRing(1), Camera: camera.New(camera.Config{}, backoff.Default, slog.Default()),
		Settings: &Settings{Store: store, Locked: func(f string) bool { return f == "user" }, Apply: func(v settings.Values) { applied = v }}}
	post := func(form url.Values) int {
		r := httptest.NewRequest("POST", "/settings", strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	if c := post(url.Values{"host": {"1.2.3.4"}, "user": {"ignored"}, "password": {"pw"}}); c != http.StatusSeeOther {
		t.Fatal(c)
	}
	if applied.CameraHost != "1.2.3.4" || applied.CameraUser != "" || applied.CameraPassword != "pw" {
		t.Fatalf("%+v", applied)
	}
	post(url.Values{"host": {"5.6.7.8"}}) // empty password keeps the old one
	if store.Get().CameraPassword != "pw" || store.Get().CameraHost != "5.6.7.8" {
		t.Fatalf("%+v", store.Get())
	}
	post(url.Values{"clear_password": {"1"}})
	if store.Get().CameraPassword != "" {
		t.Fatal("password not cleared")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/settings", nil))
	if w.Code != 200 || strings.Contains(w.Body.String(), "pw") {
		t.Fatal("settings page")
	}
}

type testPlugin struct{}

func (testPlugin) Name() string { return "tp" }
func (testPlugin) Fields() []plugin.Field {
	return []plugin.Field{{Key: "url", Label: "URL"}, {Key: "secret", Label: "Secret", Secret: true}}
}
func (testPlugin) Run(context.Context, plugin.Env) error { return nil }

func TestPluginSettings(t *testing.T) {
	plugin.Register(testPlugin{})
	store, _ := settings.Open(filepath.Join(t.TempDir(), "s.json"))
	h := &Handler{Logs: logbuf.NewRing(1), Camera: camera.New(camera.Config{}, backoff.Default, slog.Default()),
		Settings: &Settings{Store: store, Locked: func(string) bool { return false }, Apply: func(settings.Values) {}}}
	post := func(form url.Values) {
		r := httptest.NewRequest("POST", "/settings", strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		h.ServeHTTP(httptest.NewRecorder(), r)
	}
	post(url.Values{"plugin.tp.url": {"u"}, "plugin.tp.secret": {"s3"}})
	post(url.Values{"plugin.tp.url": {"u2"}}) // empty secret keeps the old one
	got := store.Get().Plugins["tp"]
	if got["url"] != "u2" || got["secret"] != "s3" {
		t.Fatalf("%v", got)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/settings", nil))
	if !strings.Contains(w.Body.String(), "Plugin: tp") || strings.Contains(w.Body.String(), "s3") {
		t.Fatal("plugin section missing or secret leaked")
	}
}

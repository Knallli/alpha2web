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
	"github.com/knallli/alpha2web/internal/export"
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
	for path, want := range map[string]string{"/": "Not connected — connects when needed", "/logs": "Logs", "/fragment/logs": "No log entries", "/static/style.css": "--bg"} {
		w := get(path, true)
		if w.Code != 200 || !strings.Contains(w.Body.String(), want) {
			t.Errorf("%s: %d %q", path, w.Code, w.Body.String())
		}
	}
	if w := get("/healthz", false); w.Code != 200 || !strings.Contains(w.Body.String(), "connected=false") {
		t.Fatal("healthz must be open and 200 without camera")
	}
	if w := get("/health", false); w.Code != 200 || !strings.Contains(w.Body.String(), `"ok"`) {
		t.Fatal("health must be open and ok while idle", w.Code)
	}
	if w := get("/metrics", false); w.Code != 200 || !strings.Contains(w.Body.String(), "alpha2web_camera_connected 0") {
		t.Fatal("metrics must be open", w.Code)
	}
	w := get("/", true)
	for k, v := range map[string]string{"X-Frame-Options": "DENY", "X-Content-Type-Options": "nosniff"} {
		if w.Header().Get(k) != v {
			t.Errorf("%s = %q", k, w.Header().Get(k))
		}
	}
	if !strings.Contains(w.Header().Get("Content-Security-Policy"), "default-src 'none'") || get("/", false).Header().Get("X-Frame-Options") != "DENY" {
		t.Error("CSP/headers missing (also on 401)")
	}
}

func TestCSRF(t *testing.T) {
	store, _ := settings.Open(filepath.Join(t.TempDir(), "s.json"))
	h := &Handler{Logs: logbuf.NewRing(1), Camera: camera.New(camera.Config{}, backoff.Default, slog.Default()),
		Settings: &Settings{Store: store, Locked: func(string) bool { return false }, Apply: func(settings.Values) {}}}
	post := func(form url.Values) int {
		r := httptest.NewRequest("POST", "/settings", strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	if c := post(url.Values{"host": {"x"}}); c != http.StatusBadRequest {
		t.Fatal("no token", c)
	}
	if c := post(url.Values{"host": {"x"}, "csrf": {"wrong"}}); c != http.StatusBadRequest {
		t.Fatal("wrong token", c)
	}
	if store.Get().CameraHost != "" {
		t.Fatal("rejected request changed settings")
	}
	if c := post(url.Values{"host": {"x"}, "csrf": {h.csrfToken()}}); c != http.StatusSeeOther {
		t.Fatal("good token", c)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/settings", nil))
	if !strings.Contains(w.Body.String(), `name="csrf" value="`+h.csrfToken()+`"`) {
		t.Fatal("settings form lacks the token")
	}
}

func TestNoInlineCode(t *testing.T) {
	for _, n := range tmpl.Templates() {
		if n.Tree == nil {
			continue
		}
		s := n.Tree.Root.String()
		if strings.Contains(s, " style=") || strings.Contains(s, " onclick=") || strings.Contains(s, "<script>") {
			t.Errorf("template %s has inline style/script", n.Name())
		}
	}
}

func TestLiveviewNotConnected(t *testing.T) {
	h := &Handler{Logs: logbuf.NewRing(1), Camera: camera.New(camera.Config{Host: "127.0.0.1:1"}, backoff.Default, slog.Default())}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.Camera.Run(ctx)
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
		form.Set("csrf", h.csrfToken())
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
	if c := post(url.Values{"keepalive": {"0"}, "debug_log": {"1"}}); c != http.StatusSeeOther || store.Get().Keepalive != "0" || !store.Get().DebugLog {
		t.Fatalf("diagnostics not saved: %d %+v", c, store.Get())
	}
	if c := post(url.Values{"keepalive": {"soon"}}); c != http.StatusBadRequest {
		t.Fatal(c)
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
	return []plugin.Field{{Key: "url", Label: "URL"}, {Key: "secret", Label: "Secret", Kind: plugin.FieldSecret}}
}
func (testPlugin) Run(context.Context, plugin.Env) error { return nil }

func TestPluginSettings(t *testing.T) {
	plugin.Register(testPlugin{})
	store, _ := settings.Open(filepath.Join(t.TempDir(), "s.json"))
	h := &Handler{Logs: logbuf.NewRing(1), Camera: camera.New(camera.Config{}, backoff.Default, slog.Default()),
		Settings: &Settings{Store: store, Locked: func(string) bool { return false }, Apply: func(settings.Values) {}}}
	post := func(form url.Values) {
		form.Set("csrf", h.csrfToken())
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

func TestConnectButtons(t *testing.T) {
	h := &Handler{Logs: logbuf.NewRing(1), Camera: camera.New(camera.Config{Host: "127.0.0.1:1"}, backoff.Default, slog.Default())}
	body := func(method, path string, v url.Values) (int, string) {
		var r *http.Request
		if method == "POST" {
			v.Set("csrf", h.csrfToken())
			r = httptest.NewRequest(method, path, strings.NewReader(v.Encode()))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		} else {
			r = httptest.NewRequest(method, path, nil)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code, w.Header().Get("Location") + w.Body.String()
	}
	if _, b := body("GET", "/", nil); strings.Contains(b, "msg wait") || !strings.Contains(b, "Not connected — connects when needed") || !strings.Contains(b, "Connect</button>") {
		t.Fatal(b)
	}
	if c, loc := body("POST", "/camera/connect", url.Values{"back": {"control"}}); c != http.StatusSeeOther || loc != "/control" {
		t.Fatal(c, loc)
	}
	if _, b := body("GET", "/", nil); !strings.Contains(b, "Connecting to the camera…") || !strings.Contains(b, "Connecting…") {
		t.Fatal(b)
	}
	if c, loc := body("POST", "/camera/disconnect", url.Values{}); c != http.StatusSeeOther || !strings.HasPrefix(loc, "/?msg=") {
		t.Fatal(c, loc)
	}
}

func TestSettingsExport(t *testing.T) {
	store, _ := settings.Open(filepath.Join(t.TempDir(), "s.json"))
	h := &Handler{Logs: logbuf.NewRing(1), Camera: camera.New(camera.Config{}, backoff.Default, slog.Default()),
		Settings: &Settings{Store: store, Locked: func(f string) bool { return f == "export_case" }, Apply: func(settings.Values) {}}}
	h.Export = func() export.Config {
		v := store.Get()
		return export.New(v.ExportDir, v.ExportTemplate, v.ExportCase, v.ExportTypes, v.ExportConflict, v.ExportMtime)
	}
	post := func(form url.Values) int { return post(h, "/settings", form).Code }
	for _, bad := range []string{"{foo}/{name}", "../{name}", "{year}", "/abs/{name}"} {
		if c := post(url.Values{"export_template": {bad}}); c != http.StatusBadRequest {
			t.Fatalf("%q accepted: %d", bad, c)
		}
	}
	if c := post(url.Values{"export_template": {"{date}/{name}{ext}"}, "export_case": {"upper"}, "export_types_present": {"1"}, "export_types": {"JPEG", "RAW"}, "export_conflict": {"overwrite"}, "export_mtime_present": {"1"}}); c != http.StatusSeeOther {
		t.Fatal(c)
	}
	v := store.Get()
	if v.ExportTemplate != "{date}/{name}{ext}" || v.ExportCase != "" || v.ExportTypes != "JPEG,RAW" || v.ExportConflict != "overwrite" || v.ExportMtime != "false" {
		t.Fatalf("%+v", v)
	}
	if c := post(url.Values{"export_types_present": {"1"}}); c != http.StatusBadRequest {
		t.Fatal("no file type selected must be refused:", c)
	}
	if b := get(h, "/settings").Body.String(); !strings.Contains(b, "Export to folder") || !strings.Contains(b, "<code>2025-03-07/DSC01234.ARW</code>") {
		t.Fatal(b)
	}
}

func TestUserActivity(t *testing.T) {
	for path, want := range map[string]bool{
		"/": true, "/gallery": true, "/gallery/pull": true, "/settings": true,
		"/fragment/dashboard": false, "/fragment/logs": false, "/static/app.js": false,
		"/api/status": false, "/gallery/job": false, "/liveview/status": false, "/metrics": false,
	} {
		if got := userActivity(path); got != want {
			t.Errorf("%s: %v", path, got)
		}
	}
}

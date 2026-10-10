package webui

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/knallli/alpha2web/internal/backoff"
	"github.com/knallli/alpha2web/internal/camera"
	"github.com/knallli/alpha2web/internal/logbuf"
	"github.com/knallli/alpha2web/internal/settings"
	"github.com/knallli/alpha2web/plugin"
)

type fullPlugin struct{}

func (fullPlugin) Name() string { return "full" }
func (fullPlugin) Fields() []plugin.Field {
	return []plugin.Field{
		{Key: "url", Label: "Target address", Group: "Target"},
		{Key: "token", Label: "Token", Kind: plugin.FieldSecret, Group: "Target"},
		{Key: "dry", Label: "Dry run", Kind: plugin.FieldBool, Group: "Behaviour"},
		{Key: "every", Label: "Interval", Kind: plugin.FieldDuration, Default: "2m", Group: "Behaviour"},
		{Key: "tries", Label: "Tries", Kind: plugin.FieldInt, Group: "Behaviour"},
		{Key: "misc", Label: "Misc"},
	}
}
func (fullPlugin) Run(context.Context, plugin.Env) error { return nil }
func (fullPlugin) Validate(cfg map[string]string) error {
	if cfg["url"] == "bad" {
		return io.ErrUnexpectedEOF
	}
	return nil
}
func (fullPlugin) Status(context.Context) plugin.Status {
	return plugin.Status{Title: "Target", Level: "ok", Headline: "Connected to target",
		Rows:     []plugin.Row{{Label: "Version", Value: "v9.9"}},
		Counts:   []plugin.Count{{Label: "Waiting", N: 7}, {Label: "Rejected", N: 2, Bad: true}},
		Totals:   []plugin.Row{{Label: "Sent", Value: "12 files"}},
		Transfer: &plugin.Transfer{Name: "DSC00042.ARW", Percent: 40, Done: 1 << 20, Size: 5 << 20, Started: time.Now()},
		Problems: []plugin.FileRow{{Path: "DCIM/DSC00013.JPG", Status: "failed", Attempts: "1/3", Note: "boom", Updated: time.Now()}},
		Recent:   []plugin.FileRow{{Path: "DCIM/DSC00001.JPG", Status: "done", Size: 2048, Link: "http://x/1", Updated: time.Now()}},
		Notice:   "Not set up yet", NoticeLink: "/settings"}
}
func (fullPlugin) FileStatuses() []string { return []string{"done", "failed"} }
func (fullPlugin) Files(_ context.Context, status string, _ int) []plugin.FileRow {
	all := []plugin.FileRow{{Path: "DCIM/DSC00001.JPG", Slot: 1, Status: "done"}, {Path: "DCIM/DSC00013.JPG", Slot: 2, Status: "failed", Note: "boom"}}
	var out []plugin.FileRow
	for _, f := range all {
		if status == "" || f.Status == status {
			out = append(out, f)
		}
	}
	return out
}
func (fullPlugin) Actions() []plugin.Action {
	return []plugin.Action{{ID: "retry", Label: "Retry now", Primary: true}, {ID: "rescan", Label: "Rescan"}}
}
func (fullPlugin) Do(_ context.Context, id string) (string, error) { return "did " + id, nil }
func (fullPlugin) WriteMetrics(w io.Writer)                        { io.WriteString(w, "full_queue 7\n") }
func (fullPlugin) Health() map[string]any                          { return map[string]any{"queue": 7} }

func TestPluginUI(t *testing.T) {
	plugin.Register(fullPlugin{})
	store, _ := settings.Open(filepath.Join(t.TempDir(), "s.json"))
	h := &Handler{Logs: logbuf.NewRing(1), Camera: camera.New(camera.Config{}, backoff.Default, slog.Default()),
		Settings: &Settings{Store: store, Locked: func(string) bool { return false }, Apply: func(settings.Values) {}}}
	do := func(method, path string, form url.Values) *httptest.ResponseRecorder {
		var r *http.Request
		if method == "POST" {
			r = httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		} else {
			r = httptest.NewRequest(method, path, nil)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	has := func(what, body string, want ...string) {
		t.Helper()
		for _, s := range want {
			if !strings.Contains(body, s) {
				t.Errorf("%s: missing %q in %s", what, s, body)
			}
		}
	}

	has("dashboard", do("GET", "/", nil).Body.String(), "Connected to target", "v9.9", "Waiting", "Rejected", "12 files",
		"DSC00042.ARW", "<progress", "DSC00013.JPG", "boom", "badge s-failed", "DSC00001.JPG", "http://x/1", "Retry now",
		"/plugins/full/actions/retry", "Not set up yet", "Restarts", `name="csrf" value="`+h.csrfToken()+`"`)
	has("fragment", do("GET", "/fragment/dashboard", nil).Body.String(), "Connected to target", "Rescan")

	// action: CSRF is enforced, the message shows on the dashboard
	if w := do("POST", "/plugins/full/actions/retry", nil); w.Code != http.StatusBadRequest {
		t.Fatal("no csrf", w.Code)
	}
	w := do("POST", "/plugins/full/actions/retry", url.Values{"csrf": {h.csrfToken()}})
	loc := w.Header().Get("Location")
	if w.Code != http.StatusSeeOther || !strings.HasPrefix(loc, "/?msg=") {
		t.Fatal(w.Code, loc)
	}
	has("msg", do("GET", loc, nil).Body.String(), "did retry")
	if w := do("POST", "/plugins/full/actions/nope", url.Values{"csrf": {h.csrfToken()}}); w.Code != http.StatusNotFound {
		t.Fatal("unknown action", w.Code)
	}

	// files: tracked table and status filter
	body := do("GET", "/files", nil).Body.String()
	has("files", body, "Tracked files", "DSC00001.JPG", "DSC00013.JPG", "<th>Captured</th>")
	body = do("GET", "/files?status=failed", nil).Body.String()
	has("files filtered", body, "DSC00013.JPG", `<option value="failed" selected>`)
	if strings.Contains(body, "DSC00001.JPG") {
		t.Error("filter did not apply")
	}

	// settings page
	body = do("GET", "/settings", nil).Body.String()
	has("settings", body, "<h2>Target</h2>", "<h2>Behaviour</h2>", "<h2>Plugin: full</h2>", `type="checkbox"`,
		`name="plugin.full.dry.present"`, `placeholder="2m"`, `type="password"`)

	save := func(form url.Values) int {
		form.Set("csrf", h.csrfToken())
		return do("POST", "/settings", form).Code
	}
	if c := save(url.Values{"plugin.full.every": {"soon"}, "plugin.full.url": {"u"}}); c != http.StatusBadRequest {
		t.Fatal("bad duration", c)
	}
	if c := save(url.Values{"plugin.full.tries": {"x"}}); c != http.StatusBadRequest {
		t.Fatal("bad int", c)
	}
	if c := save(url.Values{"plugin.full.url": {"bad"}}); c != http.StatusBadRequest {
		t.Fatal("validator", c)
	}
	if got := store.Get().Plugins["full"]; len(got) != 0 {
		t.Fatalf("rejected save stored %v", got)
	}
	if c := save(url.Values{"plugin.full.dry": {"true"}, "plugin.full.dry.present": {"1"}, "plugin.full.every": {"30s"}, "plugin.full.tries": {"3"}}); c != http.StatusSeeOther {
		t.Fatal(c)
	}
	if got := store.Get().Plugins["full"]; got["dry"] != "true" || got["every"] != "30s" || got["tries"] != "3" {
		t.Fatalf("%v", got)
	}
	save(url.Values{"plugin.full.dry.present": {"1"}}) // unchecked
	if got := store.Get().Plugins["full"]["dry"]; got != "false" {
		t.Fatalf("dry = %q", got)
	}

	// metrics and health
	has("metrics", do("GET", "/metrics", nil).Body.String(), "full_queue 7")
	has("health", do("GET", "/health", nil).Body.String(), `"full":{"queue":7}`)
}

type envPlugin struct{}

func (envPlugin) Name() string { return "envp" }
func (envPlugin) Fields() []plugin.Field {
	return []plugin.Field{
		{Key: "addr", Label: "Address", Env: "ENVP_ADDR"},
		{Key: "key", Label: "Key", Kind: plugin.FieldSecret, Env: "ENVP_KEY"},
		{Key: "other", Label: "Other"},
	}
}
func (envPlugin) Run(context.Context, plugin.Env) error { return nil }
func (envPlugin) Validate(cfg map[string]string) error {
	if e := os.Getenv("ENVP_ADDR"); e != "" && cfg["addr"] != e {
		return io.ErrUnexpectedEOF // must see the environment value
	}
	return nil
}

func TestPluginFieldEnv(t *testing.T) {
	plugin.Register(envPlugin{})
	t.Setenv("ENVP_ADDR", "http://from-env")
	store, _ := settings.Open(filepath.Join(t.TempDir(), "s.json"))
	v := store.Get()
	v.Plugins = map[string]map[string]string{"envp": {"addr": "stored"}}
	store.Set(v)
	h := &Handler{Logs: logbuf.NewRing(1), Camera: camera.New(camera.Config{}, backoff.Default, slog.Default()),
		Settings: &Settings{Store: store, Locked: func(string) bool { return false }, Apply: func(settings.Values) {}}}
	do := func(r *http.Request) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	body := do(httptest.NewRequest("GET", "/settings", nil)).Body.String()
	for _, want := range []string{"Set via ENVP_ADDR", `value="http://from-env"`, `name="plugin.envp.addr" disabled`} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(body, "Set via ENVP_KEY") {
		t.Error("unset variable locks the field")
	}
	form := url.Values{"csrf": {h.csrfToken()}, "plugin.envp.addr": {"hacked"}, "plugin.envp.other": {"x"}}
	r := httptest.NewRequest("POST", "/settings", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if w := do(r); w.Code != http.StatusSeeOther {
		t.Fatal(w.Code, w.Body.String()) // validator saw the env address
	}
	if got := store.Get().Plugins["envp"]; got["addr"] != "stored" || got["other"] != "x" {
		t.Fatalf("%v", got)
	}
}

package webui

import (
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/knallli/alpha2web/internal/settings"
	"github.com/knallli/alpha2web/plugin"
)

// Settings is the part of the settings the Handler needs; main supplies the
// environment-overrides-file logic in Apply.
type Settings struct {
	Store  *settings.Store
	Locked func(field string) bool // true if fixed by an environment variable
	Apply  func(settings.Values)   // called after a save
}

type pluginField struct {
	plugin.Field
	Input, Value string
	IsSet        bool
	Checked      bool
	Locked       bool // fixed by the environment variable Field.Env
}

// Type is the HTML input type.
func (f pluginField) Type() string {
	switch f.Kind {
	case plugin.FieldSecret:
		return "password"
	case plugin.FieldBool:
		return "checkbox"
	}
	return "text"
}

// pluginCard is one settings card: the fields of a plugin sharing a Group.
type pluginCard struct {
	Title  string
	Fields []pluginField
}

func (h *Handler) settingsPage(w http.ResponseWriter, r *http.Request) {
	v := h.Settings.Store.Get()
	lock := map[string]bool{}
	for _, f := range []string{"host", "mac", "user", "password", "fingerprint", "keepalive"} {
		lock[f] = h.Settings.Locked(f)
	}
	var cards []pluginCard
	for _, p := range plugin.All() {
		idx := map[string]int{}
		for _, f := range p.Fields() {
			saved := v.Plugins[p.Name()][f.Key]
			pf := pluginField{Field: f, Input: "plugin." + p.Name() + "." + f.Key, Value: saved, IsSet: saved != "",
				Locked: f.Env != "" && os.Getenv(f.Env) != ""}
			if pf.Locked {
				saved = os.Getenv(f.Env)
				pf.Value = saved
			}
			switch f.Kind {
			case plugin.FieldSecret:
				pf.Value = ""
			case plugin.FieldBool:
				pf.Checked = saved == "true" || saved == "" && f.Default == "true"
			}
			title := f.Group
			if title == "" {
				title = "Plugin: " + p.Name()
			}
			i, ok := idx[title]
			if !ok {
				i = len(cards)
				idx[title] = i
				cards = append(cards, pluginCard{Title: title})
			}
			cards[i].Fields = append(cards[i].Fields, pf)
		}
	}
	h.render(w, "settings.html", struct {
		page
		V       settings.Values
		HasPass bool
		Lock    map[string]bool
		Msg     string
		Plugins []pluginCard
	}{h.pg("Settings", "settings"), v, v.CameraPassword != "", lock, r.URL.Query().Get("done"), cards})
}

func (h *Handler) saveSettings(w http.ResponseWriter, r *http.Request) {
	v := h.Settings.Store.Get()
	set := func(field string, dst *string) {
		if !h.Settings.Locked(field) {
			*dst = r.FormValue(field)
		}
	}
	set("host", &v.CameraHost)
	set("mac", &v.CameraMAC)
	set("user", &v.CameraUser)
	set("fingerprint", &v.CameraFingerprint)
	if !h.Settings.Locked("keepalive") {
		ka := strings.TrimSpace(r.FormValue("keepalive"))
		if d, err := time.ParseDuration(ka); ka != "" && (err != nil || d < 0) {
			http.Error(w, "Property poll: not a duration like 10s (0 = off)", http.StatusBadRequest)
			return
		}
		v.Keepalive = ka
	}
	v.DebugLog = r.FormValue("debug_log") != ""
	// An empty password field keeps the stored one; "clear" removes it.
	if !h.Settings.Locked("password") {
		if r.FormValue("clear_password") != "" {
			v.CameraPassword = ""
		} else if p := r.FormValue("password"); p != "" {
			v.CameraPassword = p
		}
	}
	// Copy before writing: the stored map may be read concurrently by plugins.
	plugins := map[string]map[string]string{}
	for _, p := range plugin.All() {
		m := map[string]string{}
		for _, f := range p.Fields() {
			if f.Env != "" && os.Getenv(f.Env) != "" {
				// Locked: keep what is stored, the form did not offer it.
				if old := v.Plugins[p.Name()][f.Key]; old != "" {
					m[f.Key] = old
				}
				continue
			}
			in := "plugin." + p.Name() + "." + f.Key
			val := strings.TrimSpace(r.FormValue(in))
			switch f.Kind {
			case plugin.FieldSecret:
				if val == "" {
					val = v.Plugins[p.Name()][f.Key]
				}
			case plugin.FieldBool:
				// An unchecked box posts nothing; the marker tells it from a missing field.
				if val == "" && r.FormValue(in+".present") != "" {
					val = "false"
				}
			case plugin.FieldDuration:
				if _, err := time.ParseDuration(val); val != "" && err != nil {
					http.Error(w, f.Label+": not a duration like 30s or 2m", http.StatusBadRequest)
					return
				}
			case plugin.FieldInt:
				if _, err := strconv.Atoi(val); val != "" && err != nil {
					http.Error(w, f.Label+": not a whole number", http.StatusBadRequest)
					return
				}
			}
			if val != "" {
				m[f.Key] = val
			}
		}
		if cv, ok := p.(plugin.ConfigValidator); ok {
			if err := cv.Validate(plugin.Overlay(p.Fields(), m, os.Getenv)); err != nil {
				http.Error(w, p.Name()+": "+err.Error(), http.StatusBadRequest)
				return
			}
		}
		plugins[p.Name()] = m
	}
	v.Plugins = plugins
	if err := h.Settings.Store.Set(v); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.Settings.Apply(v)
	http.Redirect(w, r, "/settings?done=1", http.StatusSeeOther)
}

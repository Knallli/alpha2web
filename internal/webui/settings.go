package webui

import (
	"net/http"

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
}

type pluginSection struct {
	Name   string
	Fields []pluginField
}

func (h *Handler) settingsPage(w http.ResponseWriter, r *http.Request) {
	v := h.Settings.Store.Get()
	lock := map[string]bool{}
	for _, f := range []string{"host", "mac", "user", "password", "fingerprint"} {
		lock[f] = h.Settings.Locked(f)
	}
	var secs []pluginSection
	for _, p := range plugin.All() {
		sec := pluginSection{Name: p.Name()}
		for _, f := range p.Fields() {
			saved := v.Plugins[p.Name()][f.Key]
			val := saved
			if f.Secret {
				val = ""
			}
			sec.Fields = append(sec.Fields, pluginField{Field: f, Input: "plugin." + p.Name() + "." + f.Key, Value: val, IsSet: saved != ""})
		}
		secs = append(secs, sec)
	}
	h.render(w, "settings.html", struct {
		page
		V       settings.Values
		HasPass bool
		Lock    map[string]bool
		Msg     string
		Plugins []pluginSection
	}{h.pg("Settings", "settings"), v, v.CameraPassword != "", lock, r.URL.Query().Get("done"), secs})
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
			val := r.FormValue("plugin." + p.Name() + "." + f.Key)
			if val == "" && f.Secret {
				val = v.Plugins[p.Name()][f.Key]
			}
			if val != "" {
				m[f.Key] = val
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

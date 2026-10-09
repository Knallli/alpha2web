package webui

import (
	"net/http"

	"github.com/knallli/alpha2web/internal/settings"
)

// Settings is the part of the settings the Handler needs; main supplies the
// environment-overrides-file logic in Apply.
type Settings struct {
	Store  *settings.Store
	Locked func(field string) bool // true if fixed by an environment variable
	Apply  func(settings.Values)   // called after a save
}

func (h *Handler) settingsPage(w http.ResponseWriter, r *http.Request) {
	v := h.Settings.Store.Get()
	lock := map[string]bool{}
	for _, f := range []string{"host", "user", "password", "fingerprint"} {
		lock[f] = h.Settings.Locked(f)
	}
	h.render(w, "settings.html", struct {
		page
		V       settings.Values
		HasPass bool
		Lock    map[string]bool
		Msg     string
	}{page{"Settings", "settings", h.Version}, v, v.CameraPassword != "", lock, r.URL.Query().Get("done")})
}

func (h *Handler) saveSettings(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		http.Error(w, "cross-site request", http.StatusForbidden)
		return
	}
	v := h.Settings.Store.Get()
	set := func(field string, dst *string) {
		if !h.Settings.Locked(field) {
			*dst = r.FormValue(field)
		}
	}
	set("host", &v.CameraHost)
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
	if err := h.Settings.Store.Set(v); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.Settings.Apply(v)
	http.Redirect(w, r, "/settings?done=1", http.StatusSeeOther)
}

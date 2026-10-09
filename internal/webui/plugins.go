package webui

import (
	"context"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/knallli/alpha2web/internal/camera"
	"github.com/knallli/alpha2web/plugin"
)

// pluginBlock is one plugin's dashboard section.
type pluginBlock struct {
	Name string
	plugin.Status
	Actions []plugin.Action
}

type dashData struct {
	S        camera.Status
	Restarts uint64
	CSRF     string
	Plugins  []pluginBlock
}

func (h *Handler) dash(ctx context.Context) dashData {
	d := dashData{S: h.Camera.Status(), Restarts: h.Camera.Restarts(), CSRF: h.csrfToken()}
	for _, p := range plugin.All() {
		sp, ok := p.(plugin.StatusProvider)
		ap, _ := p.(plugin.ActionProvider)
		if !ok && ap == nil {
			continue
		}
		b := pluginBlock{Name: p.Name()}
		if ok {
			b.Status = sp.Status(ctx)
		}
		if b.Title == "" {
			b.Title = p.Name()
		}
		if ap != nil {
			b.Actions = ap.Actions()
		}
		d.Plugins = append(d.Plugins, b)
	}
	return d
}

type trackedFiles struct {
	Plugin, Filter string
	Statuses       []string
	Files          []plugin.FileRow
}

const trackedLimit = 500

// tracked collects the file tables of all plugins for the files page.
func tracked(ctx context.Context, r *http.Request) []trackedFiles {
	var out []trackedFiles
	for _, p := range plugin.All() {
		fp, ok := p.(plugin.FileStateProvider)
		if !ok {
			continue
		}
		f := r.URL.Query().Get("status")
		if !slices.Contains(fp.FileStatuses(), f) {
			f = ""
		}
		out = append(out, trackedFiles{p.Name(), f, fp.FileStatuses(), fp.Files(ctx, f, trackedLimit)})
	}
	return out
}

func (h *Handler) pluginRoutes(m *http.ServeMux) {
	m.HandleFunc("POST /plugins/{name}/actions/{id}", func(w http.ResponseWriter, r *http.Request) {
		for _, p := range plugin.All() {
			ap, ok := p.(plugin.ActionProvider)
			if !ok || p.Name() != r.PathValue("name") {
				continue
			}
			id := r.PathValue("id")
			if !slices.ContainsFunc(ap.Actions(), func(a plugin.Action) bool { return a.ID == id }) {
				break
			}
			msg, err := ap.Do(r.Context(), id)
			if err != nil {
				msg = p.Name() + ": " + err.Error()
			}
			http.Redirect(w, r, "/?msg="+url.QueryEscape(msg), http.StatusSeeOther)
			return
		}
		http.NotFound(w, r)
	})
}

func pluginMetrics(w io.Writer) {
	for _, p := range plugin.All() {
		if mp, ok := p.(plugin.MetricsProvider); ok {
			mp.WriteMetrics(w)
		}
	}
}

func pluginHealth() map[string]any {
	out := map[string]any{}
	for _, p := range plugin.All() {
		if hp, ok := p.(plugin.HealthProvider); ok {
			out[p.Name()] = hp.Health()
		}
	}
	return out
}

var funcs = template.FuncMap{
	"ago": func(t time.Time) string {
		if t.IsZero() {
			return "never"
		}
		d := time.Since(t)
		switch {
		case d < time.Minute:
			return strconv.Itoa(int(d.Seconds())) + "s ago"
		case d < time.Hour:
			return strconv.Itoa(int(d.Minutes())) + "m ago"
		case d < 48*time.Hour:
			return strconv.Itoa(int(d.Hours())) + "h ago"
		}
		return t.Local().Format("2006-01-02")
	},
	"bytes": func(n int64) string {
		const unit = 1024
		if n < unit {
			return strconv.FormatInt(n, 10) + " B"
		}
		div, exp := int64(unit), 0
		for m := n / unit; m >= unit; m /= unit {
			div *= unit
			exp++
		}
		return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
	},
	"int64": func(f float64) int64 { return int64(f) },
	"since": func(t time.Time) time.Duration { return time.Since(t).Round(time.Second) },
	"short": func(s string) string {
		if r := []rune(s); len(r) > 160 {
			return string(r[:157]) + "…"
		}
		return s
	},
	"base": func(p string) string {
		p = strings.ReplaceAll(p, "\\", "/")
		return p[strings.LastIndex(p, "/")+1:]
	},
	// cls turns a status into a safe lowercase CSS class suffix.
	"cls": func(s string) string {
		return strings.Map(func(r rune) rune {
			switch {
			case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
				return r
			case r >= 'A' && r <= 'Z':
				return r + 32
			}
			return '-'
		}, s)
	},
}

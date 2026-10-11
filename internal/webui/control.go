package webui

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/knallli/alpha2go/propdb"
	"github.com/knallli/alpha2go/ptpip"
	"github.com/knallli/alpha2web/internal/camera"
)

type propOption struct {
	Value int64  `json:"value"`
	Label string `json:"label"`
}

// propView is one property as the page and the JSON endpoint show it.
type propView struct {
	Code       uint16       `json:"code"`
	CodeHex    string       `json:"codeHex"`
	Name       string       `json:"name"`
	Desc       string       `json:"desc"`
	Group      string       `json:"group"`
	Documented bool         `json:"documented"`
	Widget     string       `json:"widget"` // switch, select, slider, number, readonly
	Current    int64        `json:"current"`
	Display    string       `json:"display"`
	Options    []propOption `json:"options,omitempty"`
	Min        int64        `json:"min"`
	Max        int64        `json:"max"`
	Step       int64        `json:"step"`
	Unit       string       `json:"unit,omitempty"`
	Scale      float64      `json:"scale,omitempty"`
	Disabled   string       `json:"disabled,omitempty"` // why the value cannot be changed now
	Danger     string       `json:"danger"`             // none, caution, danger
	Note       string       `json:"note,omitempty"`
}

func (v propView) Writable() bool { return v.Widget != "readonly" }

// format shows a raw value with its label, or scaled with its unit.
func format(raw int64, i propdb.Info) string {
	if l, ok := i.Labels[raw]; ok {
		return l
	}
	s := strconv.FormatInt(raw, 10)
	if i.Scale != 0 {
		s = strconv.FormatFloat(float64(raw)/i.Scale, 'f', -1, 64)
	}
	if i.Unit != "" {
		s += " " + i.Unit
	}
	return s
}

func viewOf(p ptpip.SonyProp) propView {
	info, documented := propdb.Lookup(p.Code)
	v := propView{Code: p.Code, CodeHex: fmt.Sprintf("0x%04X", p.Code), Name: info.Name, Desc: info.Desc, Group: info.Group,
		Documented: documented, Unit: info.Unit, Scale: info.Scale, Danger: info.Danger.Tier(), Note: info.Note, Widget: "readonly"}
	if !documented {
		v.Name = "Setting " + v.CodeHex
		v.Desc = "Not documented. The value is shown as the camera reports it."
		v.Group = "Undocumented"
	}
	integer := p.Type >= 1 && p.Type <= 8
	switch {
	case p.Type == 0xFFFF:
		v.Display = p.Current.Str
	case p.Type&0x4000 != 0:
		v.Display = fmt.Sprint(p.Current.Arr)
	case !integer:
		v.Display = "unsupported type"
	default:
		v.Current = p.Current.Int
		v.Display = format(p.Current.Int, info)
	}
	switch {
	case !integer:
		v.Disabled = "Text and list values cannot be edited here"
	case p.Enabled == 0:
		v.Disabled = "Currently not available in this camera mode"
	case p.Enabled == 2 || !p.Writable:
		v.Disabled = "Display only"
	case documented && info.Widget == "readonly":
		v.Disabled = "Read-only; not a value to set"
	case p.Form == 2 && len(p.Settable) > 0:
		v.Widget = "select"
		if len(p.Settable) == 2 {
			v.Widget = "switch"
		}
		for _, s := range p.Settable {
			v.Options = append(v.Options, propOption{s.Int, format(s.Int, info)})
		}
	case p.Form == 2:
		v.Disabled = "The camera offers no values"
	case p.Form == 1:
		v.Widget, v.Min, v.Max, v.Step = "slider", p.Min.Int, p.Max.Int, max(p.Step.Int, 1)
	default:
		v.Widget = "number"
	}
	return v
}

type propGroup struct {
	Name  string
	Props []propView
}

func groupProps(ps []ptpip.SonyProp) []propGroup {
	by := map[string][]propView{}
	for _, p := range ps {
		v := viewOf(p)
		by[v.Group] = append(by[v.Group], v)
	}
	var gs []propGroup
	for _, n := range propdb.Groups {
		if len(by[n]) > 0 {
			gs = append(gs, propGroup{n, by[n]})
		}
	}
	return gs
}

func (h *Handler) controlPage(w http.ResponseWriter, r *http.Request) {
	d := struct {
		page
		Msg       string
		Connected bool
		Groups    []propGroup
	}{page: h.pg("Control", "control"), Msg: r.URL.Query().Get("done")}
	// opening the page must not open a session: show what is there, Connect is a button
	if d.Connected = h.Camera.Status().Connected; d.Connected {
		ps, _ := h.Camera.Props() // marks the properties as wanted; the supervisor keeps them fresh
		d.Groups = groupProps(ps)
	}
	h.render(w, "control.html", d)
}

// propsJSON serves the cached property list. With ?since=<version> it answers
// 204 while nothing changed; it never does so while the camera is away, so the
// page keeps seeing the connection state. The poll keeps an open session open
// but never opens one.
func (h *Handler) propsJSON(w http.ResponseWriter, r *http.Request) {
	h.Camera.Extend()
	st := h.Camera.Status()
	ps, ver := h.Camera.Props()
	if s, err := strconv.ParseUint(r.URL.Query().Get("since"), 10, 64); err == nil && s == ver && st.Connected {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	reason := ""
	if !st.Connected {
		reason = h.banner()
	}
	out := struct {
		Version      uint64     `json:"version"`
		Connected    bool       `json:"connected"`
		Reconnecting bool       `json:"reconnecting"`
		Reason       string     `json:"reason"`
		Props        []propView `json:"props"`
	}{ver, st.Connected, st.Reconnecting, reason, make([]propView, 0, len(ps))}
	for _, p := range ps {
		out.Props = append(out.Props, viewOf(p))
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, out)
}

func wantsJSON(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "application/json")
}

func failStatus(err error) int {
	if errors.Is(err, camera.ErrNotConnected) {
		return http.StatusServiceUnavailable
	}
	return http.StatusBadGateway
}

// act runs fn against the live client and redirects back with the result.
// Callers that ask for JSON get {ok, error} instead.
func (h *Handler) act(name string, fn func(r *http.Request, c *ptpip.Client) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, release, err := h.use(r)
		if err == nil {
			// a cancelled transaction would tear the whole camera session down
			func() {
				defer release() // a panic in fn must not keep the camera connected
				err = fn(r.WithContext(context.WithoutCancel(r.Context())), c)
			}()
		}
		if wantsJSON(r) {
			w.Header().Set("Content-Type", "application/json")
			res := struct {
				OK    bool   `json:"ok"`
				Error string `json:"error,omitempty"`
				Msg   string `json:"msg,omitempty"`
			}{OK: err == nil, Msg: name}
			if err != nil {
				res.Error = camera.Explain(err)
				w.WriteHeader(failStatus(err))
			}
			writeJSON(w, res)
			return
		}
		if err != nil {
			http.Error(w, camera.Explain(err), failStatus(err))
			return
		}
		http.Redirect(w, r, "/control?done="+url.QueryEscape(name), http.StatusSeeOther)
	}
}

// settable checks a value against what the camera offers right now.
func settable(p ptpip.SonyProp, v int64) string {
	switch p.Form {
	case 2:
		if !slices.ContainsFunc(p.Settable, func(s ptpip.SonyValue) bool { return s.Int == v }) {
			return "The camera does not accept that value now"
		}
	case 1:
		if v < p.Min.Int || v > p.Max.Int || (v-p.Min.Int)%max(p.Step.Int, 1) != 0 {
			return fmt.Sprintf("Value must be between %d and %d in steps of %d", p.Min.Int, p.Max.Int, max(p.Step.Int, 1))
		}
	}
	return ""
}

func (h *Handler) setProp(w http.ResponseWriter, r *http.Request) {
	var pv *propView
	reply := func(status int, msg string) {
		if wantsJSON(r) {
			w.Header().Set("Content-Type", "application/json")
			if status != 0 {
				w.WriteHeader(status)
			}
			writeJSON(w, struct {
				OK    bool      `json:"ok"`
				Error string    `json:"error,omitempty"`
				Prop  *propView `json:"prop,omitempty"`
			}{status == 0, msg, pv})
		} else if status != 0 {
			http.Error(w, msg, status)
		} else {
			http.Redirect(w, r, "/control?done=property+set", http.StatusSeeOther)
		}
	}
	code, err := strconv.ParseUint(r.FormValue("code"), 10, 16)
	if err != nil {
		reply(http.StatusBadRequest, "Invalid property code")
		return
	}
	v, err := strconv.ParseInt(r.FormValue("value"), 10, 64)
	if err != nil {
		reply(http.StatusBadRequest, "The value must be a whole number")
		return
	}
	c, release, err := h.use(r)
	if err != nil {
		reply(failStatus(err), camera.Explain(err))
		return
	}
	defer release()
	// a cancelled transaction would tear the whole camera session down
	ctx := context.WithoutCancel(r.Context())
	p, err := c.SonyGetProp(ctx, uint16(code))
	if ptpip.IsCode(err, 0x200A) {
		reply(http.StatusNotFound, fmt.Sprintf("Unknown property 0x%04X", code))
		return
	} else if err != nil {
		reply(failStatus(err), camera.Explain(err))
		return
	}
	view := viewOf(p)
	pv = &view
	switch {
	case view.Widget == "readonly":
		reply(http.StatusUnprocessableEntity, view.Name+": "+view.Disabled)
	case !view.Documented && r.FormValue("unlock") != "1":
		reply(http.StatusForbidden, "Undocumented setting: enable editing of undocumented settings first")
	case view.Danger == "danger" && r.FormValue("confirm") != "1":
		reply(http.StatusConflict, view.Note)
	default:
		if msg := settable(p, v); msg != "" {
			reply(http.StatusUnprocessableEntity, msg)
			return
		}
		if err := c.SonySetProp(ctx, p.Code, p.Type, ptpip.SonyValue{Int: v}); err != nil {
			reply(failStatus(err), camera.Explain(err))
			return
		}
		// single-property read: a full read would swallow the supervisor's "changed" list
		if np, err := c.SonyGetProp(ctx, p.Code); err == nil {
			view = viewOf(np)
		}
		reply(0, "")
	}
}

func (h *Handler) controlRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /control", h.controlPage)
	m.HandleFunc("GET /control/props", h.propsJSON)
	m.HandleFunc("POST /actions/capture", h.act("captured", func(r *http.Request, c *ptpip.Client) error {
		return c.SonyCapture(r.Context(), r.FormValue("af") == "1")
	}))
	m.HandleFunc("POST /actions/movie", h.act("movie toggled", func(r *http.Request, c *ptpip.Client) error {
		return c.SonyToggleMovie(r.Context())
	}))
	m.HandleFunc("POST /actions/setprop", h.setProp)
}

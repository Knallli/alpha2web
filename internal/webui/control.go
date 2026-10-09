package webui

import (
	"fmt"
	"net/http"
	"slices"
	"strconv"

	"github.com/knallli/alpha2go/ptpip"
)

type propRow struct {
	Code     uint16
	Current  string
	Writable bool
	Choices  []int64
}

func (h *Handler) controlPage(w http.ResponseWriter, r *http.Request) {
	d := struct {
		page
		Msg, Err string
		Props    []propRow
	}{page: page{"Control", "control", h.Version}, Msg: r.URL.Query().Get("done")}
	c, err := h.Camera.Client()
	if err == nil {
		var ps []ptpip.SonyProp
		if ps, err = c.SonyGetProps(r.Context(), false); err == nil {
			slices.SortFunc(ps, func(a, b ptpip.SonyProp) int { return int(a.Code) - int(b.Code) })
			for _, p := range ps {
				row := propRow{Code: p.Code, Writable: p.Writable && p.Enabled == 1 && p.Type != 0xFFFF && p.Type&0x4000 == 0}
				if p.Type == 0xFFFF {
					row.Current = p.Current.Str
				} else if p.Type&0x4000 != 0 {
					row.Current = fmt.Sprint(p.Current.Arr)
				} else {
					row.Current = strconv.FormatInt(p.Current.Int, 10)
				}
				for _, v := range p.Settable {
					row.Choices = append(row.Choices, v.Int)
				}
				d.Props = append(d.Props, row)
			}
		}
	}
	if err != nil {
		d.Err = err.Error()
	}
	h.render(w, "control.html", d)
}

// act runs fn against the live client and redirects back with the result.
func (h *Handler) act(name string, fn func(r *http.Request, c *ptpip.Client) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
			http.Error(w, "cross-site request", http.StatusForbidden)
			return
		}
		c, err := h.Camera.Client()
		if err == nil {
			err = fn(r, c)
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		http.Redirect(w, r, "/control?done="+name, http.StatusSeeOther)
	}
}

func (h *Handler) controlRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /control", h.controlPage)
	m.HandleFunc("POST /actions/capture", h.act("captured", func(r *http.Request, c *ptpip.Client) error {
		return c.SonyCapture(r.Context(), r.FormValue("af") == "1")
	}))
	m.HandleFunc("POST /actions/movie", h.act("movie toggled", func(r *http.Request, c *ptpip.Client) error {
		return c.SonyToggleMovie(r.Context())
	}))
	m.HandleFunc("POST /actions/setprop", h.act("property set", func(r *http.Request, c *ptpip.Client) error {
		code, err := strconv.ParseUint(r.FormValue("code"), 10, 16)
		if err != nil {
			return err
		}
		v, err := strconv.ParseInt(r.FormValue("value"), 10, 64)
		if err != nil {
			return err
		}
		ps, err := c.SonyGetProps(r.Context(), false)
		if err != nil {
			return err
		}
		for _, p := range ps {
			if p.Code == uint16(code) {
				if !p.Writable || p.Enabled != 1 {
					return fmt.Errorf("property 0x%04X is not settable now", code)
				}
				ctx := r.Context()
				return c.SonySetProp(ctx, p.Code, p.Type, ptpip.SonyValue{Int: v})
			}
		}
		return fmt.Errorf("unknown property 0x%04X", code)
	}))
}

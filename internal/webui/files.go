package webui

import (
	"net/http"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/knallli/alpha2go/ptpip"
)

func slotParam(r *http.Request) uint32 {
	if n, _ := strconv.ParseUint(r.FormValue("slot"), 10, 8); n == 2 {
		return 2
	}
	return 1
}

func (h *Handler) filesPage(w http.ResponseWriter, r *http.Request) {
	d := struct {
		page
		Slot     uint32
		Msg, Err string
		Items    []ptpip.SonyContent
	}{page: page{"Files", "files", h.Version}, Slot: slotParam(r), Msg: r.URL.Query().Get("done")}
	c, err := h.Camera.Client()
	if err == nil {
		d.Items, err = c.SonyListAllContents(r.Context(), d.Slot)
		slices.SortFunc(d.Items, func(a, b ptpip.SonyContent) int { return b.CreatedUTC.Compare(a.CreatedUTC) })
	}
	if err != nil {
		d.Err = err.Error()
	}
	h.render(w, "files.html", d)
}

func (h *Handler) fileThumb(w http.ResponseWriter, r *http.Request) {
	c, err := h.Camera.Client()
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	id, _ := strconv.ParseUint(r.FormValue("id"), 10, 32)
	file, _ := strconv.ParseUint(r.FormValue("file"), 10, 16)
	w.Header().Set("Cache-Control", "private, max-age=3600")
	if _, err := c.SonyGetContentsCompressed(r.Context(), slotParam(r), uint32(id), uint16(file), ptpip.SonyThumbnail, w); err != nil {
		h.Log().Warn("thumbnail", "err", err)
	}
}

func (h *Handler) fileGet(w http.ResponseWriter, r *http.Request) {
	c, err := h.Camera.Client()
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	id, _ := strconv.ParseUint(r.FormValue("id"), 10, 32)
	file, _ := strconv.ParseUint(r.FormValue("file"), 10, 16)
	size, err := strconv.ParseUint(r.FormValue("size"), 10, 64)
	if err != nil || size == 0 {
		http.Error(w, "bad size", http.StatusBadRequest)
		return
	}
	name := path.Base(strings.ReplaceAll(r.FormValue("path"), "\\", "/"))
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatUint(size, 10))
	w.Header().Set("Content-Disposition", `attachment; filename="`+strings.NewReplacer(`"`, "", "\r", "", "\n", "").Replace(name)+`"`)
	if _, err := c.SonyDownloadContentFile(r.Context(), slotParam(r), uint32(id), uint16(file), size, w, nil); err != nil {
		h.Log().Warn("download", "err", err)
	}
}

func (h *Handler) filesRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /files", h.filesPage)
	m.HandleFunc("GET /files/thumb", h.fileThumb)
	m.HandleFunc("GET /files/get", h.fileGet)
	m.HandleFunc("POST /actions/delete", h.act("deleted", func(r *http.Request, c *ptpip.Client) error {
		id, err := strconv.ParseUint(r.FormValue("id"), 10, 32)
		if err != nil {
			return err
		}
		return c.SonyDeleteContent(r.Context(), slotParam(r), uint32(id))
	}))
}

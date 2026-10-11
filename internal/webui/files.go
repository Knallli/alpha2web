package webui

import (
	"net/http"
	"strconv"
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
		Msg     string
		Tracked []trackedFiles
	}{h.pg("Files", "files"), r.URL.Query().Get("done"), tracked(r.Context(), r)}
	h.render(w, "files.html", d)
}

func (h *Handler) filesRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /files", h.filesPage)
}

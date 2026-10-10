package webui

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/knallli/alpha2go/ptpip"
	"github.com/knallli/alpha2web/internal/camera"
)

// lenientWriter swallows write errors so a transfer already running can finish.
type lenientWriter struct {
	w    io.Writer
	dead bool
}

func (l *lenientWriter) Write(p []byte) (int, error) {
	if !l.dead {
		if _, err := l.w.Write(p); err != nil {
			l.dead = true
		}
	}
	return len(p), nil
}

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
		Tracked  []trackedFiles
	}{page: h.pg("Files", "files"), Slot: slotParam(r), Msg: r.URL.Query().Get("done"), Tracked: tracked(r.Context(), r)}
	c, err := h.client(r)
	if err == nil {
		d.Items, err = c.SonyListAllContents(context.WithoutCancel(r.Context()), d.Slot)
		slices.SortFunc(d.Items, func(a, b ptpip.SonyContent) int { return b.CreatedUTC.Compare(a.CreatedUTC) })
	}
	if err != nil && !errors.Is(err, camera.ErrNotConnected) { // the banner says that
		d.Err = camera.Explain(err)
	}
	h.render(w, "files.html", d)
}

func (h *Handler) fileThumb(w http.ResponseWriter, r *http.Request) {
	c, err := h.client(r)
	if err != nil {
		http.Error(w, camera.Explain(err), http.StatusServiceUnavailable)
		return
	}
	id, _ := strconv.ParseUint(r.FormValue("id"), 10, 32)
	file, _ := strconv.ParseUint(r.FormValue("file"), 10, 16)
	release, ok := h.acquire(r)
	if !ok {
		return
	}
	defer release()
	var buf bytes.Buffer
	if _, err := c.SonyGetContentsCompressed(context.WithoutCancel(r.Context()), slotParam(r), uint32(id), uint16(file), ptpip.SonyThumbnail, &buf); err != nil {
		h.Log().Warn("thumbnail", "err", err)
		http.Error(w, "thumbnail unavailable", http.StatusBadGateway)
		return
	}
	w.Header().Set("Cache-Control", "private, max-age=3600")
	w.Write(buf.Bytes())
}

func (h *Handler) fileGet(w http.ResponseWriter, r *http.Request) {
	c, err := h.client(r)
	if err != nil {
		http.Error(w, camera.Explain(err), http.StatusServiceUnavailable)
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
	release, ok := h.acquire(r)
	if !ok {
		return
	}
	defer release()
	// A closed browser tab must not cancel the transfer (that breaks the camera
	// session); the rest of the file is read and dropped instead.
	if _, err := c.SonyDownloadContentFile(context.WithoutCancel(r.Context()), slotParam(r), uint32(id), uint16(file), size, &lenientWriter{w: w}, nil); err != nil {
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
		err = c.SonyDeleteContent(r.Context(), slotParam(r), uint32(id))
		h.galleryState().invalidate()
		return err
	}))
}

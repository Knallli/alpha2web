package webui

import (
	"bytes"
	"context"
	"net/http"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/knallli/alpha2go/ptpip"
	"github.com/knallli/alpha2web/internal/thumbcache"
)

const (
	galleryPageSize = 48
	listingTTL      = 30 * time.Second
	maxFetches      = 2 // the ptpip client serializes transactions; leave room for live view and control
)

type gallery struct {
	mu    sync.Mutex
	lists map[uint32]listing
	sem   chan struct{}
	cache *thumbcache.Cache
}

type listing struct {
	at    time.Time
	items []ptpip.SonyContent // newest first
}

func (h *Handler) galleryState() *gallery {
	h.galOnce.Do(func() {
		h.gal.lists = map[uint32]listing{}
		h.gal.sem = make(chan struct{}, maxFetches)
		h.gal.cache = &thumbcache.Cache{Dir: h.CacheDir}
	})
	return &h.gal
}

func (g *gallery) invalidate() {
	g.mu.Lock()
	clear(g.lists)
	g.mu.Unlock()
}

// list returns the slot's contents, newest first, from a 30 s in-memory cache.
// The returned slice must not be modified.
func (g *gallery) list(ctx context.Context, c *ptpip.Client, slot uint32) ([]ptpip.SonyContent, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if l, ok := g.lists[slot]; ok && time.Since(l.at) < listingTTL {
		return l.items, nil
	}
	items, err := c.SonyListAllContents(ctx, slot)
	if err != nil {
		return nil, err
	}
	slices.SortFunc(items, func(a, b ptpip.SonyContent) int { return b.CreatedUTC.Compare(a.CreatedUTC) })
	g.lists[slot] = listing{time.Now(), items}
	return items, nil
}

func fileExt(f ptpip.SonyContentFile) string {
	return strings.ToUpper(strings.TrimPrefix(path.Ext(strings.ReplaceAll(f.Path, "\\", "/")), "."))
}

// coverFile prefers a viewable file (JPEG/HEIF) over RAW for the card image.
func coverFile(ct ptpip.SonyContent) ptpip.SonyContentFile {
	for _, f := range ct.Files {
		switch fileExt(f) {
		case "JPG", "JPEG", "HIF", "HEIF", "HEIC":
			return f
		}
	}
	return ct.Files[0]
}

type galleryFile struct {
	ID   uint16 `json:"id"`
	Path string `json:"path"`
	Size uint64 `json:"size"`
}

type galleryItem struct {
	ID      uint32        `json:"id"`
	Cover   uint16        `json:"cover"`
	Created time.Time     `json:"created"`
	Exts    []string      `json:"-"`
	Video   bool          `json:"video"`
	Files   []galleryFile `json:"files"`
}

var videoExt = map[string]bool{"MP4": true, "MOV": true, "MXF": true}

func toItem(ct ptpip.SonyContent) galleryItem {
	it := galleryItem{ID: ct.ID, Cover: coverFile(ct).ID, Created: ct.CreatedUTC}
	for _, f := range ct.Files {
		it.Files = append(it.Files, galleryFile{f.ID, f.Path, f.Size})
		e := fileExt(f)
		if e != "" && !slices.Contains(it.Exts, e) {
			it.Exts = append(it.Exts, e)
		}
		if f.HasVideo || videoExt[e] {
			it.Video = true
		}
	}
	return it
}

func (h *Handler) galleryPage(w http.ResponseWriter, r *http.Request) {
	d := struct {
		page
		Slot             uint32
		Msg, Err         string
		Items            []galleryItem
		Page, Prev, Next int
		Total            int
	}{page: page{"Gallery", "gallery", h.Version}, Slot: slotParam(r), Msg: r.URL.Query().Get("done")}
	c, err := h.Camera.Client()
	var all []ptpip.SonyContent
	if err == nil {
		all, err = h.galleryState().list(r.Context(), c, d.Slot)
	}
	if err != nil {
		d.Err = err.Error()
	}
	d.Total = len(all)
	pages := max(1, (len(all)+galleryPageSize-1)/galleryPageSize)
	n, _ := strconv.Atoi(r.FormValue("page"))
	d.Page = min(max(n, 1), pages)
	if d.Page > 1 {
		d.Prev = d.Page - 1
	}
	if d.Page < pages {
		d.Next = d.Page + 1
	}
	for _, ct := range all[min((d.Page-1)*galleryPageSize, len(all)):min(d.Page*galleryPageSize, len(all))] {
		if len(ct.Files) > 0 {
			d.Items = append(d.Items, toItem(ct))
		}
	}
	h.render(w, "gallery.html", d)
}

func sniff(b []byte) string {
	switch {
	case bytes.HasPrefix(b, []byte{0xFF, 0xD8}):
		return "image/jpeg"
	case len(b) >= 8 && string(b[4:8]) == "ftyp":
		return "image/heic"
	}
	return "application/octet-stream"
}

// galleryImage serves a thumbnail or preview from the disk cache, fetching from the camera on a miss.
func (h *Handler) galleryImage(preview bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := h.Camera.Client()
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		g := h.galleryState()
		slot := slotParam(r)
		items, err := g.list(r.Context(), c, slot)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		id, _ := strconv.ParseUint(r.FormValue("id"), 10, 32)
		fid, _ := strconv.ParseUint(r.FormValue("file"), 10, 16)
		var ct *ptpip.SonyContent
		var f *ptpip.SonyContentFile
		for i := range items {
			if items[i].ID == uint32(id) {
				ct = &items[i]
			}
		}
		if ct != nil {
			for i := range ct.Files {
				if uint64(ct.Files[i].ID) == fid {
					f = &ct.Files[i]
				}
			}
		}
		if f == nil {
			http.NotFound(w, r)
			return
		}
		kind, typ := "thumb", ptpip.SonyThumbnail
		full := false
		if preview {
			kind, typ = "screennail", ptpip.SonyScreennail
			if e := fileExt(*f); e == "JPG" || e == "JPEG" {
				kind, full = "file", true
			}
		}
		key := thumbcache.Key(slot, f.Path, f.Size, ct.CreatedUTC, kind)
		data, ok := g.cache.Get(key)
		if !ok {
			select {
			case g.sem <- struct{}{}:
				defer func() { <-g.sem }()
			case <-r.Context().Done():
				return
			}
			var buf bytes.Buffer
			if full {
				_, err = c.SonyDownloadContentFile(r.Context(), slot, ct.ID, f.ID, f.Size, &buf, nil)
			} else {
				_, err = c.SonyGetContentsCompressed(r.Context(), slot, ct.ID, f.ID, typ, &buf)
			}
			if err != nil || buf.Len() == 0 {
				if err != nil {
					h.Log().Warn("gallery image", "kind", kind, "err", err)
				}
				http.NotFound(w, r)
				return
			}
			data = buf.Bytes()
			if err := g.cache.Put(key, data); err != nil {
				h.Log().Warn("thumb cache", "err", err)
			}
		}
		w.Header().Set("Content-Type", sniff(data))
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "private, max-age=86400")
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.Write(data)
	}
}

func (h *Handler) galleryRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /gallery", h.galleryPage)
	m.HandleFunc("GET /gallery/thumb", h.galleryImage(false))
	m.HandleFunc("GET /gallery/preview", h.galleryImage(true))
}

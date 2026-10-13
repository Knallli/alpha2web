package webui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/knallli/alpha2go/ptpip"
	"github.com/knallli/alpha2web/internal/camera"
	"github.com/knallli/alpha2web/internal/export"
	"github.com/knallli/alpha2web/internal/heicconv"
	"github.com/knallli/alpha2web/internal/thumbcache"
)

const (
	galleryPageSize = 48
	maxFetches      = 2    // the ptpip client serializes transactions; leave room for live view and control
	previewEdge     = 2400 // long edge of HEIF previews converted to JPEG
	cardMsg         = "The camera did not allow access to the memory card. Close the menu or playback on the camera, make sure the Sony app (Creators' App / Imaging Edge) is not connected, then try again."
)

// The catalog is what the gallery page shows; it is only written by a refresh
// job (or a delete) and never needs the camera.
type catFile struct {
	Path  string `json:"path"`
	Size  uint64 `json:"size"`
	Local string `json:"local,omitempty"` // where a pull put it; the template may change later
}

type catItem struct {
	Key     string    `json:"key"` // stable across sessions, unlike content IDs
	Created time.Time `json:"created"`
	Video   bool      `json:"video"`
	Exts    []string  `json:"exts"`
	Files   []catFile `json:"files"`
	Cover   int       `json:"cover"` // index into Files
}

type catSlot struct {
	Refreshed time.Time `json:"refreshed"`
	Err       string    `json:"err"`
	Empty     bool      `json:"empty"` // the camera reported no card
	Items     []catItem `json:"items"` // newest first
}

type catalog struct {
	Model string     `json:"model"`
	Slots [2]catSlot `json:"slots"`
}

type listed struct {
	items []ptpip.SonyContent
	err   error
}

type gallery struct {
	mu   sync.Mutex // cat and job
	cat  catalog
	job  job
	stop atomic.Bool

	lmu   sync.Mutex // session listing
	lc    *ptpip.Client
	lists map[uint32]listed

	sem   chan struct{}
	cache *thumbcache.Cache
}

func (h *Handler) galleryState() *gallery {
	h.galOnce.Do(func() {
		h.gal.sem = make(chan struct{}, maxFetches)
		h.gal.cache = &thumbcache.Cache{Dir: h.CacheDir}
		if b, err := os.ReadFile(h.catalogFile()); err == nil {
			if err := json.Unmarshal(b, &h.gal.cat); err != nil {
				h.Log().Warn("gallery catalog unreadable, starting empty", "err", err)
			}
		}
	})
	return &h.gal
}

func (h *Handler) catalogFile() string {
	if h.CacheDir == "" {
		return ""
	}
	return filepath.Join(h.CacheDir, "catalog.json")
}

// saveLocked writes the catalog atomically; g.mu must be held.
func (h *Handler) saveLocked() {
	p := h.catalogFile()
	if p == "" {
		return
	}
	b, err := json.Marshal(&h.gal.cat)
	if err == nil {
		if err = os.MkdirAll(h.CacheDir, 0o755); err == nil {
			err = os.WriteFile(p+".tmp", b, 0o644)
		}
		if err == nil {
			err = os.Rename(p+".tmp", p)
		}
	}
	if err != nil {
		h.Log().Warn("gallery catalog not saved", "err", err)
	}
}

// sessionList lists a slot once per camera session (a new client is a new
// session: content IDs change). Errors are cached too, so a refusing camera is
// asked once, not once per image.
func (g *gallery) sessionList(c *ptpip.Client, slot uint32, card func() func()) ([]ptpip.SonyContent, error) {
	g.lmu.Lock()
	defer g.lmu.Unlock()
	if g.lc != c {
		g.lc, g.lists = c, map[uint32]listed{}
	}
	if l, ok := g.lists[slot]; ok {
		return l.items, l.err
	}
	// a cancelled transaction would break the camera session
	unlock := card()
	items, err := c.SonyListAllContents(context.Background(), slot)
	unlock()
	slices.SortFunc(items, func(a, b ptpip.SonyContent) int { return b.CreatedUTC.Compare(a.CreatedUTC) })
	if !errors.Is(err, ptpip.ErrCardUnavailable) { // the user may close the camera menu and retry
		g.lists[slot] = listed{items, err}
	}
	return items, err
}

func (g *gallery) forget(slot uint32) {
	g.lmu.Lock()
	delete(g.lists, slot)
	g.lmu.Unlock()
}

func fileExt(p string) string {
	return strings.ToUpper(strings.TrimPrefix(path.Ext(strings.ReplaceAll(p, "\\", "/")), "."))
}

func viewable(ext string) bool {
	switch ext {
	case "JPG", "JPEG", "HIF", "HEIF", "HEIC":
		return true
	}
	return false
}

// coverIdx prefers a viewable file (JPEG/HEIF) over RAW for the card image.
func coverIdx(files []ptpip.SonyContentFile) int {
	for i, f := range files {
		if viewable(fileExt(f.Path)) {
			return i
		}
	}
	return 0
}

var videoExt = map[string]bool{"MP4": true, "MOV": true, "MXF": true}

func itemKey(slot uint32, ct ptpip.SonyContent) string {
	f := ct.Files[coverIdx(ct.Files)]
	return thumbcache.Key(slot, f.Path, f.Size, ct.CreatedUTC, "item")[:16]
}

func toItem(slot uint32, ct ptpip.SonyContent) catItem {
	it := catItem{Key: itemKey(slot, ct), Created: ct.CreatedUTC, Cover: coverIdx(ct.Files)}
	for _, f := range ct.Files {
		it.Files = append(it.Files, catFile{Path: f.Path, Size: f.Size})
		e := fileExt(f.Path)
		if e != "" && !slices.Contains(it.Exts, e) {
			it.Exts = append(it.Exts, e)
		}
		if f.HasVideo || videoExt[e] {
			it.Video = true
		}
	}
	return it
}

// findContent maps a catalog item to its content in the session listing.
func findContent(live []ptpip.SonyContent, slot uint32, key string) *ptpip.SonyContent {
	for i := range live {
		if len(live[i].Files) > 0 && itemKey(slot, live[i]) == key {
			return &live[i]
		}
	}
	return nil
}

func liveFile(ct *ptpip.SonyContent, f catFile) *ptpip.SonyContentFile {
	for i := range ct.Files {
		if ct.Files[i].Path == f.Path && ct.Files[i].Size == f.Size {
			return &ct.Files[i]
		}
	}
	return nil
}

func baseName(p string) string { return path.Base(strings.ReplaceAll(p, "\\", "/")) }

// pulled: a pull recorded the copy and it is still there.
func pulled(f catFile) bool {
	st, err := os.Stat(f.Local)
	return f.Local != "" && err == nil && uint64(st.Size()) == f.Size
}

// imgKey is the cache key of an item's cover image. HEIF is stored converted
// under kind+"-jpg" so a hit needs neither the camera nor the decoder.
func (it catItem) imgKey(slot uint32, kind string) string {
	f := it.Files[it.Cover]
	return thumbcache.Key(slot, f.Path, f.Size, it.Created, kind)
}

func (g *gallery) getImage(slot uint32, it catItem, kind string) ([]byte, bool) {
	if data, ok := g.cache.Get(it.imgKey(slot, kind+"-jpg")); ok {
		return data, true
	}
	return g.cache.Get(it.imgKey(slot, kind))
}

func (g *gallery) hasImage(slot uint32, it catItem, kind string) bool {
	if g.cache.Dir == "" {
		return false
	}
	for _, k := range []string{kind + "-jpg", kind} {
		if _, err := os.Stat(filepath.Join(g.cache.Dir, it.imgKey(slot, k))); err == nil {
			return true
		}
	}
	return false
}

// putImage caches data (HEIF converted to JPEG) and returns what to serve.
func (h *Handler) putImage(slot uint32, it catItem, kind string, data []byte) []byte {
	key := it.imgKey(slot, kind)
	if sniff(data) == "image/heic" {
		if jpg, err := heicconv.ToJPEG(data, previewEdge); err != nil {
			h.warnOnce("heic conversion failed, serving original", err)
		} else {
			data, key = jpg, it.imgKey(slot, kind+"-jpg")
		}
	}
	if err := h.galleryState().cache.Put(key, data); err != nil {
		h.Log().Warn("thumb cache", "err", err)
	}
	return data
}

type viewFile struct {
	Path   string `json:"path"`
	Size   uint64 `json:"size"`
	Pulled bool   `json:"pulled"`
	Immich bool   `json:"immich"`
}

type viewItem struct {
	Key     string     `json:"key"`
	Created time.Time  `json:"created"`
	Video   bool       `json:"video"`
	Exts    []string   `json:"exts"`
	Files   []viewFile `json:"files"`
	Pulled  bool       `json:"pulled"`
	Immich  bool       `json:"immich"` // every file is held by a plugin
}

func (h *Handler) galleryPage(w http.ResponseWriter, r *http.Request) {
	g := h.galleryState()
	slot := slotParam(r)
	cfg := h.exportCfg()
	g.mu.Lock()
	s, last := g.cat.Slots[slot-1], g.job
	g.mu.Unlock()
	d := struct {
		page
		Slot             uint32
		Msg              string
		S                catSlot
		Never, HasCat    bool
		Last             *job // the last finished job, until the next one starts
		Items            []viewItem
		Page, Prev, Next int
		Total            int
		Keys             []string // every item of the slot, for "select all"
		Library          string
	}{page: h.pg("Gallery", "gallery"), Slot: slot, Msg: r.URL.Query().Get("done"), S: s, Total: len(s.Items), Library: cfg.Dir}
	for _, it := range s.Items {
		d.Keys = append(d.Keys, it.Key)
	}
	d.Never = s.Refreshed.IsZero() && s.Err == ""
	d.HasCat = !g.catalogEmpty()
	if last.Finished && (last.Err != "" || last.Result != "") {
		d.Last = &last
	}
	pages := max(1, (len(s.Items)+galleryPageSize-1)/galleryPageSize)
	n, _ := strconv.Atoi(r.FormValue("page"))
	d.Page = min(max(n, 1), pages)
	if d.Page > 1 {
		d.Prev = d.Page - 1
	}
	if d.Page < pages {
		d.Next = d.Page + 1
	}
	for _, it := range s.Items[min((d.Page-1)*galleryPageSize, len(s.Items)):min(d.Page*galleryPageSize, len(s.Items))] {
		v := viewItem{Key: it.Key, Created: it.Created, Video: it.Video, Exts: it.Exts, Pulled: true, Immich: len(it.Files) > 0}
		wanted := 0
		for _, f := range it.Files {
			p := pulled(f)
			im := h.stored(r.Context(), slot, it, f)
			v.Immich = v.Immich && im
			v.Files = append(v.Files, viewFile{f.Path, f.Size, p, im})
			if cfg.Wants(export.Type(fileExt(f.Path))) {
				wanted++
				v.Pulled = v.Pulled && p
			}
		}
		v.Pulled = v.Pulled && wanted > 0
		d.Items = append(d.Items, v)
	}
	h.render(w, "gallery.html", d)
}

func (g *gallery) catalogEmpty() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.cat.Slots[0].Refreshed.IsZero() && g.cat.Slots[1].Refreshed.IsZero()
}

func sniff(b []byte) string {
	switch {
	case bytes.HasPrefix(b, []byte{0xFF, 0xD8}):
		return "image/jpeg"
	case len(b) >= 8 && string(b[4:8]) == "ftyp":
		return "image/heic"
	case len(b) >= 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WEBP":
		return "image/webp"
	}
	return "application/octet-stream"
}

// acquire takes one of the camera fetch slots. It gives up, without touching
// the camera, when the browser went away while waiting.
func (h *Handler) acquire(r *http.Request) (release func(), ok bool) {
	g := h.galleryState()
	select {
	case g.sem <- struct{}{}:
	case <-r.Context().Done():
		return nil, false
	}
	if r.Context().Err() != nil {
		<-g.sem
		return nil, false
	}
	return func() { <-g.sem }, true
}

// lookup finds the catalog item named by the request.
func (h *Handler) lookup(r *http.Request) (uint32, catItem, bool) {
	g := h.galleryState()
	slot, k := slotParam(r), r.FormValue("k")
	g.mu.Lock()
	defer g.mu.Unlock()
	i := slices.IndexFunc(g.cat.Slots[slot-1].Items, func(it catItem) bool { return it.Key == k })
	if i < 0 {
		return slot, catItem{}, false
	}
	return slot, g.cat.Slots[slot-1].Items[i], true
}

// cameraFail answers a failed camera access with a readable text.
func (h *Handler) cameraFail(w http.ResponseWriter, err error) {
	if errors.Is(err, ptpip.ErrCardUnavailable) {
		http.Error(w, cardMsg, http.StatusServiceUnavailable)
		return
	}
	http.Error(w, camera.Explain(err), failStatus(err))
}

func serveImage(w http.ResponseWriter, data []byte) {
	w.Header().Set("Content-Type", sniff(data))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "private, max-age=86400")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Write(data)
}

// galleryThumb serves the cached thumbnail only; the camera is never asked.
func (h *Handler) galleryThumb(w http.ResponseWriter, r *http.Request) {
	slot, it, ok := h.lookup(r)
	if ok {
		var data []byte
		if data, ok = h.galleryState().getImage(slot, it, "thumb"); ok {
			serveImage(w, data)
			return
		}
		// not cached (the card was never read for it): a plugin may hold the shot
		if data, ok = h.storedImage(r.Context(), slot, it, it.Files[it.Cover], "thumbnail"); ok {
			serveImage(w, h.putImage(slot, it, "thumb", data))
			return
		}
	}
	http.NotFound(w, r)
}

// galleryPreview serves the expanded original. Order: a plugin that holds the
// shot (Immich), the cache, a pulled library copy, the camera.
func (h *Handler) galleryPreview(w http.ResponseWriter, r *http.Request) {
	slot, it, ok := h.lookup(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	g := h.galleryState()
	cover := it.Files[it.Cover]
	if data, ok := h.storedImage(r.Context(), slot, it, cover, "preview"); ok {
		serveImage(w, h.putImage(slot, it, "preview", data))
		return
	}
	if data, ok := g.getImage(slot, it, "preview"); ok {
		serveImage(w, data)
		return
	}
	if pulled(cover) && viewable(fileExt(cover.Path)) {
		if data, err := os.ReadFile(cover.Local); err == nil {
			serveImage(w, h.putImage(slot, it, "preview", data))
			return
		}
	}
	release, ok := h.acquire(r)
	if !ok {
		return
	}
	defer release()
	c, done, err := h.use(r)
	if err != nil {
		h.cameraFail(w, err)
		return
	}
	defer done()
	live, err := g.sessionList(c, slot, h.card)
	if err != nil {
		h.cameraFail(w, err)
		return
	}
	ct := findContent(live, slot, it.Key)
	if ct == nil {
		http.Error(w, "The shot is no longer on the card.", http.StatusNotFound)
		return
	}
	// leaving the page must not cancel a running transaction: that breaks the camera session
	unlock := h.card()
	data, err := fetchPreview(context.WithoutCancel(r.Context()), c, slot, ct, ct.Files[coverIdx(ct.Files)])
	unlock()
	if err != nil {
		h.Log().Warn("gallery preview", "err", err)
		h.cameraFail(w, err)
		return
	}
	serveImage(w, h.putImage(slot, it, "preview", data))
}

// fetchPreview downloads a JPEG or HEIF cover; RAW-only items, videos and
// HEIF that does not convert get the camera's screennail (else the thumbnail).
func fetchPreview(ctx context.Context, c *ptpip.Client, slot uint32, ct *ptpip.SonyContent, f ptpip.SonyContentFile) ([]byte, error) {
	var buf bytes.Buffer
	switch e := fileExt(f.Path); {
	case e == "JPG" || e == "JPEG":
		if _, err := c.SonyDownloadContentFile(ctx, slot, ct.ID, f.ID, f.Size, &buf, nil); err != nil {
			return nil, err
		}
		return buf.Bytes(), nil
	case viewable(e):
		if _, err := c.SonyDownloadContentFile(ctx, slot, ct.ID, f.ID, f.Size, &buf, nil); err == nil {
			if jpg, err := heicconv.ToJPEG(buf.Bytes(), previewEdge); err == nil {
				return jpg, nil
			}
		} else if errors.Is(err, ptpip.ErrCardUnavailable) {
			return nil, err
		}
		buf.Reset()
	}
	var err error
	for _, typ := range []uint32{ptpip.SonyScreennail, ptpip.SonyThumbnail} {
		buf.Reset()
		if _, err = c.SonyGetContentsCompressed(ctx, slot, ct.ID, f.ID, typ, &buf); err == nil && buf.Len() > 0 {
			return buf.Bytes(), nil
		}
	}
	if err == nil {
		err = errors.New("the camera sent no image")
	}
	return nil, err
}

// lenientWriter swallows write errors so a transfer already running can finish.
// A browser that stops reading (paused download) gives up after a deadline,
// so the transfer and the card lock it holds are not stuck.
type lenientWriter struct {
	w    http.ResponseWriter
	dead bool
}

func (l *lenientWriter) Write(p []byte) (int, error) {
	if !l.dead {
		http.NewResponseController(l.w).SetWriteDeadline(time.Now().Add(30 * time.Second))
		if _, err := l.w.Write(p); err != nil {
			l.dead = true
		}
	}
	return len(p), nil
}

// galleryFile downloads one file of an item: the pulled copy, else the camera.
func (h *Handler) galleryFile(w http.ResponseWriter, r *http.Request) {
	slot, it, ok := h.lookup(r)
	fi, err := strconv.Atoi(r.FormValue("f"))
	if !ok || err != nil || fi < 0 || fi >= len(it.Files) {
		http.NotFound(w, r)
		return
	}
	f := it.Files[fi]
	name := strings.NewReplacer(`"`, "", "\r", "", "\n", "").Replace(baseName(f.Path))
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	if rc, ct, ok := h.openStored(r.Context(), slot, it, f, "original"); ok {
		defer rc.Close()
		if ct != "" {
			w.Header().Set("Content-Type", ct)
		}
		io.Copy(w, rc)
		return
	}
	if pulled(f) {
		if file, err := os.Open(f.Local); err == nil {
			defer file.Close()
			http.ServeContent(w, r, name, time.Time{}, file)
			return
		}
	}
	c, done, err := h.use(r)
	if err != nil {
		h.cameraFail(w, err)
		return
	}
	defer done()
	live, err := h.galleryState().sessionList(c, slot, h.card)
	if err != nil {
		h.cameraFail(w, err)
		return
	}
	var lf *ptpip.SonyContentFile
	ct := findContent(live, slot, it.Key)
	if ct != nil {
		lf = liveFile(ct, f)
	}
	if lf == nil {
		http.Error(w, "The shot is no longer on the card.", http.StatusNotFound)
		return
	}
	release, ok := h.acquire(r)
	if !ok {
		return
	}
	defer release()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatUint(f.Size, 10))
	// A closed browser tab must not cancel the transfer (that breaks the camera
	// session); the rest of the file is read and dropped instead.
	defer h.card()()
	if _, err := c.SonyDownloadContentFile(context.WithoutCancel(r.Context()), slot, ct.ID, lf.ID, f.Size, &lenientWriter{w: w}, nil); err != nil {
		h.Log().Warn("download", "err", err)
	}
}

// galleryDelete removes the shot from the card (pulled copies stay).
func (h *Handler) galleryDelete(w http.ResponseWriter, r *http.Request) {
	slot, it, ok := h.lookup(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	g := h.galleryState()
	c, done, err := h.use(r)
	if err != nil {
		h.cameraFail(w, err)
		return
	}
	defer done()
	live, err := g.sessionList(c, slot, h.card)
	if err == nil {
		if ct := findContent(live, slot, it.Key); ct == nil {
			err = errors.New("The shot is no longer on the card.")
		} else {
			unlock := h.card()
			err = c.SonyDeleteContent(context.WithoutCancel(r.Context()), slot, ct.ID)
			unlock()
		}
		g.forget(slot)
	}
	if err != nil {
		h.cameraFail(w, err)
		return
	}
	g.mu.Lock()
	s := &g.cat.Slots[slot-1]
	s.Items = slices.DeleteFunc(slices.Clone(s.Items), func(x catItem) bool { return x.Key == it.Key })
	h.saveLocked()
	g.mu.Unlock()
	http.Redirect(w, r, "/gallery?slot="+strconv.Itoa(int(slot))+"&done="+url.QueryEscape("deleted"), http.StatusSeeOther)
}

var warned sync.Map

// warnOnce logs each distinct error text once per process.
func (h *Handler) warnOnce(msg string, err error) {
	if _, dup := warned.LoadOrStore(err.Error(), true); !dup {
		h.Log().Warn(msg, "err", err)
	}
}

func (h *Handler) galleryRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /gallery", h.galleryPage)
	m.HandleFunc("GET /gallery/thumb", h.galleryThumb)
	m.HandleFunc("GET /gallery/preview", h.galleryPreview)
	m.HandleFunc("GET /gallery/file", h.galleryFile)
	m.HandleFunc("POST /gallery/delete", h.galleryDelete)
	m.HandleFunc("POST /gallery/purge", h.galleryPurge)
	m.HandleFunc("GET /gallery/job", h.jobStatus)
	m.HandleFunc("POST /gallery/job/stop", h.jobStop)
	m.HandleFunc("POST /gallery/refresh", h.galleryRefresh)
	m.HandleFunc("POST /gallery/pull", h.galleryPull)
}

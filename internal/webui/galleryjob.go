package webui

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/knallli/alpha2go/ptpip"
	"github.com/knallli/alpha2web/internal/camera"
	"github.com/knallli/alpha2web/internal/export"
	"github.com/knallli/alpha2web/plugin"
)

const maxPullKeys = 2000

var drivePrefix = regexp.MustCompile(`^[A-Za-z]:`)

// job is the state of the one background job (refresh or pull) of the gallery.
type job struct {
	Kind     string    `json:"kind"`
	Started  time.Time `json:"started"`
	Ended    time.Time `json:"ended"`
	Done     int       `json:"done"`
	Total    int       `json:"total"`
	Current  string    `json:"current"`
	Err      string    `json:"error"`
	Result   string    `json:"result"`
	Finished bool      `json:"finished"`
}

func (g *gallery) update(f func(j *job)) {
	g.mu.Lock()
	f(&g.job)
	g.mu.Unlock()
}

func (h *Handler) jobStatus(w http.ResponseWriter, _ *http.Request) {
	g := h.galleryState()
	g.mu.Lock()
	j := g.job
	g.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	writeJSON(w, j)
}

func (h *Handler) jobStop(w http.ResponseWriter, _ *http.Request) {
	h.galleryState().stop.Store(true)
	w.WriteHeader(http.StatusAccepted)
}

// startJob runs fn in the background with one camera lease; the HTTP request
// is not involved, so leaving the page cancels nothing. fn may release early.
func (h *Handler) startJob(w http.ResponseWriter, kind string, fn func(c *ptpip.Client, release func())) {
	g := h.galleryState()
	g.mu.Lock()
	if g.job.Kind != "" && !g.job.Finished {
		k := g.job.Kind
		g.mu.Unlock()
		http.Error(w, "A "+k+" job is already running.", http.StatusConflict)
		return
	}
	g.job = job{Kind: kind, Started: time.Now()}
	g.stop.Store(false)
	g.mu.Unlock()
	go func() {
		defer g.update(func(j *job) { j.Finished, j.Ended, j.Current = true, time.Now(), "" })
		c, release, err := h.Camera.Acquire(context.Background())
		if err != nil {
			g.update(func(j *job) { j.Err = camera.Explain(err) })
			return
		}
		defer release()
		fn(c, release)
	}()
	w.WriteHeader(http.StatusAccepted)
}

func (h *Handler) galleryRefresh(w http.ResponseWriter, _ *http.Request) {
	h.startJob(w, "refresh", func(c *ptpip.Client, _ func()) { h.refresh(c) })
}

func (h *Handler) refresh(c *ptpip.Client) {
	g := h.galleryState()
	type thumb struct {
		slot uint32
		it   catItem
		id   uint32
		fid  uint16
	}
	var todo []thumb
	g.mu.Lock()
	g.cat.Model = h.Camera.Status().Model
	g.mu.Unlock()
	for slot := uint32(1); slot <= 2 && !g.stop.Load(); slot++ {
		g.update(func(j *job) { j.Current = fmt.Sprintf("Listing slot %d", slot) })
		g.forget(slot)
		items, err := g.sessionList(c, slot)
		g.mu.Lock()
		s := &g.cat.Slots[slot-1]
		switch {
		case err == nil:
			old := s.Items
			*s = catSlot{Refreshed: time.Now(), Empty: len(items) == 0}
			for _, ct := range items {
				if len(ct.Files) == 0 {
					continue
				}
				it := toItem(slot, ct)
				if i := slices.IndexFunc(old, func(o catItem) bool { return o.Key == it.Key }); i >= 0 {
					for n, f := range it.Files { // keep what earlier pulls recorded
						if j := slices.IndexFunc(old[i].Files, func(o catFile) bool { return o.Path == f.Path && o.Size == f.Size }); j >= 0 {
							it.Files[n].Local = old[i].Files[j].Local
						}
					}
				}
				s.Items = append(s.Items, it)
				if !g.hasImage(slot, it, "thumb") {
					todo = append(todo, thumb{slot, it, ct.ID, ct.Files[it.Cover].ID})
				}
			}
		case errors.Is(err, ptpip.ErrSlotEmpty):
			*s = catSlot{Refreshed: time.Now(), Empty: true}
		case errors.Is(err, ptpip.ErrCardUnavailable):
			s.Err = cardMsg // the previous items stay
		default:
			s.Err = camera.Explain(err)
			g.job.Err = s.Err
			g.stop.Store(true) // a broken session: do not go on
		}
		g.mu.Unlock()
	}
	g.mu.Lock()
	h.saveLocked()
	g.mu.Unlock()
	g.update(func(j *job) { j.Total = len(todo) })
	fails := 0
	for _, t := range todo {
		if g.stop.Load() {
			break
		}
		g.update(func(j *job) { j.Current = baseName(t.it.Files[t.it.Cover].Path) })
		var buf bytes.Buffer
		_, err := c.SonyGetContentsCompressed(context.Background(), t.slot, t.id, t.fid, ptpip.SonyThumbnail, &buf)
		if err == nil && buf.Len() == 0 {
			err = errors.New("the camera sent no thumbnail")
		}
		if errors.Is(err, ptpip.ErrCardUnavailable) {
			g.update(func(j *job) { j.Err = "Thumbnails stopped: " + cardMsg })
			break
		} else if err != nil {
			h.Log().Warn("gallery thumbnail", "err", err)
			if fails++; fails >= 3 {
				g.update(func(j *job) { j.Err = "Thumbnails stopped: " + camera.Explain(err) })
				break
			}
		} else {
			fails = 0
			h.putImage(t.slot, t.it, "thumb", buf.Bytes())
		}
		g.update(func(j *job) { j.Done++ })
	}
}

func (h *Handler) galleryPull(w http.ResponseWriter, r *http.Request) {
	g := h.galleryState()
	slot := slotParam(r)
	keys := r.Form["k"]
	cfg := h.exportCfg()
	if cfg.Dir == "" {
		http.Error(w, "No export folder is set (LIBRARY_DIR or the Settings page).", http.StatusBadRequest)
		return
	}
	if err := writable(cfg.Dir); err != nil {
		http.Error(w, "The export folder is not writable: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if len(keys) == 0 || len(keys) > maxPullKeys {
		http.Error(w, "Select between 1 and "+strconv.Itoa(maxPullKeys)+" shots.", http.StatusBadRequest)
		return
	}
	var items []catItem
	g.mu.Lock()
	model := g.cat.Model
	for _, it := range g.cat.Slots[slot-1].Items {
		if slices.Contains(keys, it.Key) {
			items = append(items, it)
		}
	}
	g.mu.Unlock()
	if len(items) == 0 {
		http.Error(w, "The selected shots are not in the catalog.", http.StatusBadRequest)
		return
	}
	h.startJob(w, "pull", func(c *ptpip.Client, release func()) { h.pull(c, release, cfg, slot, model, items) })
}

// writable creates dir if needed and checks that a file can be written there.
func writable(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".write-test-")
	if err != nil {
		return err
	}
	f.Close()
	return os.Remove(f.Name())
}

// statFor reports an existing file as Taken when another catalog entry or this
// pull owns it, and as this shot only when its mtime is the capture time
// (same size alone is not proof: uncompressed RAWs all have one size).
func statFor(taken map[string]bool, captured time.Time) func(string) (int64, bool) {
	return func(p string) (int64, bool) {
		st, err := os.Stat(p)
		switch {
		case err != nil:
			return 0, false
		case taken[p]:
			return export.Taken, true
		case st.ModTime().Sub(captured).Abs() > 2*time.Second: // exFAT stores 2 s steps
			return -2, true
		}
		return st.Size(), true
	}
}

func (h *Handler) pull(c *ptpip.Client, release func(), cfg export.Config, slot uint32, model string, items []catItem) {
	g := h.galleryState()
	g.update(func(j *job) { j.Total = len(items) })
	fail := func(err error) {
		g.update(func(j *job) {
			if j.Err = camera.Explain(err); errors.Is(err, ptpip.ErrCardUnavailable) {
				j.Err = cardMsg
			}
		})
	}
	g.forget(slot) // the card may have changed since the last listing
	live, err := g.sessionList(c, slot)
	if err != nil {
		fail(err)
		return
	}
	serial := h.Camera.Status().Serial
	taken := map[string]bool{}
	g.mu.Lock()
	for _, s := range g.cat.Slots {
		for _, it := range s.Items {
			for _, f := range it.Files {
				if f.Local != "" {
					taken[f.Local] = true
				}
			}
		}
	}
	g.mu.Unlock()
	var local []plugin.LocalFile
	var gone, copied, skipped, inPlugin, seq int
loop:
	for _, it := range items {
		if g.stop.Load() {
			break
		}
		g.update(func(j *job) { j.Current = baseName(it.Files[it.Cover].Path) })
		ct := findContent(live, slot, it.Key)
		var lfs []*ptpip.SonyContentFile
		for _, f := range it.Files {
			if ct == nil {
				break
			}
			lf := liveFile(ct, f)
			if lf == nil {
				ct = nil
				break
			}
			lfs = append(lfs, lf)
		}
		if ct == nil {
			gone++
			g.update(func(j *job) { j.Done++ })
			continue
		}
		for i, f := range it.Files {
			if !cfg.Wants(export.Type(fileExt(f.Path))) {
				skipped++
				continue
			}
			if !pulled(f) && h.stored(context.Background(), slot, it, f) {
				inPlugin++ // lives in Immich only: not copied again
				continue
			}
			target := f.Local
			if !pulled(f) {
				seq++
				rel := export.Expand(cfg.Template, cfg.Case, export.Item{Camera: model, Serial: serial, Slot: int(slot), When: ct.CreatedLocal, Path: f.Path, Seq: seq})
				if rel == "" {
					fail(fmt.Errorf("the path template gives no file name for %s", f.Path))
					break loop
				}
				var have bool
				if target, have = cfg.Target(rel, int64(f.Size), statFor(taken, ct.CreatedUTC)); !have {
					if err := download(c, slot, ct.ID, lfs[i], target); err != nil {
						h.Log().Warn("gallery pull", "err", err)
						fail(err)
						break loop
					}
					copied++
					if cfg.SetMtime {
						os.Chtimes(target, ct.CreatedUTC, ct.CreatedUTC)
					}
				}
				taken[target] = true
				g.record(slot, it.Key, f, target)
			}
			local = append(local, plugin.LocalFile{Camera: model, Slot: int(slot), Path: strings.TrimLeft(drivePrefix.ReplaceAllString(strings.ReplaceAll(f.Path, "\\", "/"), ""), "/"), CapturedAt: ct.CreatedUTC, Size: int64(f.Size), Local: target})
		}
		g.update(func(j *job) { j.Done++ })
	}
	g.mu.Lock()
	h.saveLocked()
	g.mu.Unlock()
	release()
	res := fmt.Sprintf("%d files copied, %d already in the library.", copied, len(local)-copied)
	if skipped > 0 {
		res += fmt.Sprintf(" %d files skipped by the file type setting.", skipped)
	}
	if inPlugin > 0 {
		res += fmt.Sprintf(" %d files are stored in Immich and were not copied.", inPlugin)
	}
	if gone > 0 {
		res += fmt.Sprintf(" %d items are no longer on the card.", gone)
	}
	if len(local) > 0 {
		g.update(func(j *job) { j.Current = "Handing over to plugins" })
		for _, p := range plugin.All() {
			if imp, ok := p.(plugin.Importer); ok {
				if err := imp.Import(context.Background(), local); err != nil {
					res += fmt.Sprintf(" %s: %v.", p.Name(), err)
				}
			}
		}
	}
	g.update(func(j *job) { j.Result = res })
}

// record remembers where a file was put.
func (g *gallery) record(slot uint32, key string, f catFile, local string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for i := range g.cat.Slots[slot-1].Items {
		if it := &g.cat.Slots[slot-1].Items[i]; it.Key == key {
			it.Files = slices.Clone(it.Files) // page renders may hold the old slice
			for n := range it.Files {
				if it.Files[n].Path == f.Path && it.Files[n].Size == f.Size {
					it.Files[n].Local = local
				}
			}
		}
	}
}

// download writes the file to target via <target>.partial.
func download(c *ptpip.Client, slot, id uint32, f *ptpip.SonyContentFile, target string) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	tmp := target + ".partial"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	n, err := c.SonyDownloadContentFile(context.Background(), slot, id, f.ID, f.Size, out, nil)
	if err == nil && uint64(n) != f.Size {
		err = fmt.Errorf("%s: got %d of %d bytes", f.Path, n, f.Size)
	}
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, target)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}

// galleryPurge removes the library copies of files a plugin (Immich) holds, so
// they live there only. The card and the plugin's copy are not touched.
func (h *Handler) galleryPurge(w http.ResponseWriter, r *http.Request) {
	g := h.galleryState()
	g.mu.Lock()
	busy := g.job.Kind != "" && !g.job.Finished
	g.mu.Unlock()
	if busy {
		http.Error(w, "A job is running; wait until it has finished.", http.StatusConflict)
		return
	}
	root := filepath.Clean(h.exportCfg().Dir)
	var removed, failed int
	for slot := uint32(1); slot <= 2; slot++ {
		g.mu.Lock()
		items := slices.Clone(g.cat.Slots[slot-1].Items)
		g.mu.Unlock()
		for _, it := range items {
			for _, f := range it.Files {
				if !pulled(f) || !h.stored(r.Context(), slot, it, f) {
					continue
				}
				if err := os.Remove(f.Local); err != nil && !errors.Is(err, os.ErrNotExist) {
					h.Log().Warn("gallery purge", "path", f.Local, "err", err)
					failed++
					continue
				}
				pruneEmpty(filepath.Dir(f.Local), root)
				g.record(slot, it.Key, f, "")
				removed++
			}
		}
	}
	g.mu.Lock()
	h.saveLocked()
	g.mu.Unlock()
	msg := fmt.Sprintf("%d library files removed; they live in Immich only.", removed)
	if failed > 0 {
		msg += fmt.Sprintf(" %d could not be removed (see the log).", failed)
	}
	http.Redirect(w, r, "/gallery?slot="+strconv.Itoa(int(slotParam(r)))+"&done="+url.QueryEscape(msg), http.StatusSeeOther)
}

// pruneEmpty removes empty directories from dir up to (not including) root.
func pruneEmpty(dir, root string) {
	for dir = filepath.Clean(dir); strings.HasPrefix(dir, root+string(filepath.Separator)); dir = filepath.Dir(dir) {
		if os.Remove(dir) != nil { // not empty
			return
		}
	}
}

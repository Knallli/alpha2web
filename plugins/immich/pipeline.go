// The pipeline moves files from the camera to Immich:
//
//	sync, only on request (connect, list both cards, record new files)
//	  -> download (one at a time) -> verify + SHA-1 -> staging
//	  -> upload workers (bulk-upload-check, POST /assets)
//	  -> post-processing (album, RAW+JPEG stack, delete staged copy)
//	  -> optional delete of the content on the camera
//
// All progress is persisted in the store, so any step can be retried after
// failures, disconnects or restarts without creating duplicates.

package immich

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/knallli/alpha2go/ptpip"
	"github.com/knallli/alpha2web/internal/camera"
	"github.com/knallli/alpha2web/plugin"
	"github.com/knallli/alpha2web/plugins/immich/store"
	"github.com/knallli/alpha2web/plugins/immich/verify"
	"io"
)

const (
	maxPostAttempts = 5
	groupWaitLimit  = 24 * time.Hour // give up waiting for a RAW/JPEG sibling to stack
)

// ref locates a file on the camera. IDs are only valid for one connection.
type ref struct {
	slot, content uint32
	file          uint16
}

// content is one camera content (a shot: JPEG, RAW, ...) minus skipped files.
type content struct {
	slot, id  uint32
	protected bool
	files     []store.Discovered
}

type pipeline struct {
	env     plugin.Env
	log     *slog.Logger
	store   *store.Store
	staging string
	Metrics Metrics

	// Tunables; tests shorten them.
	statfs func(dir string) (free int64, err error)
	idle   time.Duration // how often idle workers re-check the database

	syncCh, uploadWake, postWake, immichWake chan struct{}

	mu         sync.Mutex
	files      map[string]ref // identity -> ids of the current listing
	contents   []content
	byID       map[string]int // identity -> index in contents
	lastSync   time.Time
	lastResult string
	lowSpace   bool
	im         *Client
	imKey      string
	album      string
	albumID    string

	syncing       atomic.Bool
	immichOK      atomic.Bool
	immichVersion atomic.Value // string
	immichErr     atomic.Value // string: why the last health check failed, "" if it passed or none ran
	keyProblem    atomic.Value // string: set when Immich refuses the key
	transfer      atomic.Pointer[transfer]
}

func newPipeline(env plugin.Env, st *store.Store, staging string) *pipeline {
	p := &pipeline{
		env: env, log: env.Log, store: st, staging: staging,
		statfs: freeBytes,
		idle:   5 * time.Second,

		syncCh:     make(chan struct{}, 1),
		uploadWake: make(chan struct{}, 1),
		postWake:   make(chan struct{}, 1),
		immichWake: make(chan struct{}, 1),
		files:      map[string]ref{},
	}
	p.immichVersion.Store("")
	p.immichErr.Store("")
	p.keyProblem.Store("")
	return p
}

func freeBytes(dir string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}

func (p *pipeline) cfg() settings {
	s, _ := parseSettings(p.env.Config())
	return s
}

// client returns the Immich client for the current settings, or nil while
// url or API key are missing.
func (p *pipeline) client(s settings) *Client {
	if s.URL == "" || s.APIKey == "" {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if key := s.URL + "\x00" + s.APIKey; p.im == nil || key != p.imKey {
		p.im, p.imKey, p.albumID = New(s.URL, s.APIKey, "alpha2web"), key, ""
		p.immichOK.Store(false)
		p.immichVersion.Store("")
		p.immichErr.Store("")
		p.keyProblem.Store("")
		wake(p.immichWake)
	}
	return p.im
}

func wake(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// sleep waits for d, a wake-up on ch (may be nil) or ctx; it reports a wake-up.
func sleep(ctx context.Context, d time.Duration, ch <-chan struct{}) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ch:
		return true
	case <-ctx.Done():
	}
	return false
}

// run blocks until ctx is done. Camera operations are cancelled with it.
func (p *pipeline) run(ctx context.Context) {
	var wg sync.WaitGroup
	start := func(f func()) { wg.Add(1); go func() { defer wg.Done(); f() }() }
	start(func() { p.immichLoop(ctx) })
	start(func() { p.downloadLoop(ctx) })
	start(func() { p.uploadLoop(ctx) })
	start(func() { p.postLoop(ctx) })
	wg.Wait()
}

// checkKey remembers whether Immich refuses the API key; other errors are
// transient and leave the last verdict alone.
func (p *pipeline) checkKey(ctx context.Context, im *Client) {
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	err := im.ValidateKey(cctx)
	switch {
	case err == nil:
		p.keyProblem.Store("")
	case IsAuthError(err):
		p.keyProblem.Store("Immich refuses the API key or it lacks permissions. It needs: " + strings.Join(requiredPermissions(p.cfg()), ", ") + ".")
	}
}

// StartSync asks for a sync run; it reports false if one is already running.
func (p *pipeline) StartSync() bool {
	if !p.syncing.CompareAndSwap(false, true) {
		return false
	}
	wake(p.syncCh)
	return true
}

// WakeAll makes every loop look for work now.
func (p *pipeline) WakeAll() {
	for _, ch := range []chan struct{}{p.uploadWake, p.postWake, p.immichWake} {
		wake(ch)
	}
}

// CurrentTransfer returns the running download, or nil.
func (p *pipeline) CurrentTransfer() *plugin.Transfer {
	if t := p.transfer.Load(); t != nil {
		s := t.snapshot()
		return &s
	}
	return nil
}

// ---------------------------------------------------------------- transfer

type transfer struct {
	mu        sync.Mutex
	name      string
	size      int64
	done      int64
	speed     float64 // bytes/s, smoothed
	started   time.Time
	at        time.Time
	doneAt    int64
	lastWrite atomic.Int64 // unix nanos, for the stall watchdog
}

func newTransfer(name string, size int64) *transfer {
	now := time.Now()
	t := &transfer{name: name, size: size, started: now, at: now}
	t.lastWrite.Store(now.UnixNano())
	return t
}

func (t *transfer) add(n int) {
	now := time.Now()
	t.lastWrite.Store(now.UnixNano())
	t.mu.Lock()
	defer t.mu.Unlock()
	t.done += int64(n)
	if dt := now.Sub(t.at).Seconds(); dt >= 0.5 {
		v := float64(t.done-t.doneAt) / dt
		if t.speed == 0 {
			t.speed = v
		} else {
			t.speed = 0.7*t.speed + 0.3*v
		}
		t.at, t.doneAt = now, t.done
	}
}

func (t *transfer) snapshot() plugin.Transfer {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := plugin.Transfer{Name: t.name, Size: t.size, Done: t.done, BytesPerSec: t.speed, Started: t.started}
	if t.size > 0 {
		out.Percent = int(t.done * 100 / t.size)
		if t.speed > 0 {
			out.ETA = time.Duration(float64(t.size-t.done) / t.speed * float64(time.Second)).Round(time.Second)
		}
	}
	return out
}

// progressWriter feeds the transfer while the file is written.
type progressWriter struct {
	w io.Writer
	t *transfer
}

func (w progressWriter) Write(b []byte) (int, error) {
	n, err := w.w.Write(b)
	w.t.add(n)
	return n, err
}

// ---------------------------------------------------------------- reconcile

var unsafeID = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func sanitizeID(model string) string {
	id := strings.Trim(unsafeID.ReplaceAllString(model, "_"), "_")
	if id == "" {
		id = "camera"
	}
	return id
}

func (p *pipeline) cameraID() string { return sanitizeID(p.env.Camera.Status().Model) }

func itemKey(d store.Discovered) string {
	return fmt.Sprintf("%d|%s|%d|%d", d.Slot, d.Path, d.CapturedAt.UnixMilli(), d.Size)
}

// groupKey groups files of one shot: same camera, slot, folder, base name and day.
func groupKey(d store.Discovered) string {
	dir, base := path.Split(d.Path)
	stem := strings.ToUpper(strings.TrimSuffix(base, path.Ext(base)))
	return fmt.Sprintf("%s|%d|%s|%s|%s", d.Camera, d.Slot, dir, stem, d.CapturedAt.UTC().Format("2006-01-02"))
}

var drivePrefix = regexp.MustCompile(`^[A-Za-z]:`)

// cleanPath turns "A:/DCIM/100MSDCF/DSC00001.JPG" into "DCIM/100MSDCF/DSC00001.JPG".
func cleanPath(p string) string {
	p = drivePrefix.ReplaceAllString(strings.ReplaceAll(p, "\\", "/"), "")
	return strings.TrimLeft(path.Clean("/"+p), "/")
}

// reconcile lists both cards in full and records new files. Content IDs of
// the listing are only valid for this session. It returns the new files.
func (p *pipeline) reconcile(ctx context.Context, cam *ptpip.Client, s settings) (int, error) {
	camID := p.cameraID()
	files := map[string]ref{}
	byID := map[string]int{}
	var contents []content
	var discovered []store.Discovered
	skipped := 0
	for slot := uint32(1); slot <= 2; slot++ {
		cs, err := cam.SonyListAllContents(ctx, slot)
		if err != nil {
			// Only "no card in this slot" may be skipped. Any other failure (a
			// timeout, a dropped session) must not look like an empty card.
			if !errors.Is(err, ptpip.ErrSlotEmpty) {
				return 0, err
			}
			p.log.Debug("slot has no card", "slot", slot, "err", err)
			continue
		}
		for _, c := range cs {
			ct := content{slot: slot, id: c.ID, protected: c.Protected}
			for _, f := range c.Files {
				d := store.Discovered{Camera: camID, Slot: int(slot), Path: cleanPath(f.Path), CapturedAt: c.CreatedUTC, Size: int64(f.Size)}
				if s.skipped(d.Path) {
					skipped++
					continue
				}
				d.GroupKey = groupKey(d)
				files[itemKey(d)] = ref{slot, c.ID, f.ID}
				byID[itemKey(d)] = len(contents)
				ct.files = append(ct.files, d)
				discovered = append(discovered, d)
			}
			contents = append(contents, ct)
		}
	}
	added, err := p.store.UpsertDiscovered(context.WithoutCancel(ctx), discovered)
	if err != nil {
		return 0, err
	}
	p.mu.Lock()
	p.files, p.contents, p.byID = files, contents, byID
	p.mu.Unlock()
	p.Metrics.Reconciles.Add(1)
	p.log.Info("reconciled camera contents", "camera_id", camID, "files_on_camera", len(files), "new", added, "skipped", skipped)
	return added, nil
}

const cardUnavailableMsg = "The camera did not allow access to the memory card. Close the menu or playback on the camera and make sure the Sony app is not connected, then sync again."

// downloadLoop waits for sync requests; nothing touches the camera otherwise.
func (p *pipeline) downloadLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-p.syncCh:
		}
		res := p.sync(ctx)
		p.log.Info("camera sync finished", "result", res)
		p.mu.Lock()
		p.lastSync, p.lastResult = time.Now(), res
		p.mu.Unlock()
		p.syncing.Store(false)
	}
}

// sync connects, transfers every due file of the listing and returns a
// readable result. The lease is released at the end; the session closes by
// itself after the linger time.
func (p *pipeline) sync(ctx context.Context) string {
	s := p.cfg()
	cam, release, err := p.env.Camera.Acquire(ctx)
	if err != nil {
		return camera.Explain(err)
	}
	defer release()
	added, err := p.reconcile(ctx, cam, s)
	if err != nil {
		if errors.Is(err, ptpip.ErrCardUnavailable) {
			return cardUnavailableMsg
		}
		return camera.Explain(err)
	}
	downloaded, failed := 0, 0
	res := ""
loop:
	for ctx.Err() == nil {
		out, err := p.downloadNext(ctx, cam, s)
		if err != nil {
			p.log.Error("download step failed", "err", err)
			res = "Stopped: " + err.Error()
			break
		}
		switch out {
		case dlNone:
			break loop
		case dlOK:
			downloaded++
		case dlFailed:
			failed++
		case dlLost:
			failed++
			res = "Stopped: " + cmp.Or(p.env.Camera.Status().LastDropReason, "the connection to the camera was lost")
			break loop
		}
	}
	if s.DeleteOnCamera && ctx.Err() == nil && res == "" {
		p.deleteDone(ctx, cam)
	}
	r := fmt.Sprintf("%d new, %d downloaded", added, downloaded)
	if failed > 0 {
		r += fmt.Sprintf(", %d failed", failed)
	}
	if c, err := p.store.Counts(ctx); err == nil && c[store.StatusPending] > 0 {
		r += fmt.Sprintf(", %d still waiting", c[store.StatusPending])
	}
	p.mu.Lock()
	low := p.lowSpace
	p.mu.Unlock()
	if low {
		r += ". Paused: not enough free space"
	}
	if res != "" {
		r += ". " + res
	}
	return r
}

func (p *pipeline) freeSpaceOK(s settings) bool {
	if s.MinFreeBytes <= 0 {
		return true
	}
	free, err := p.statfs(p.staging)
	if err != nil {
		return true
	}
	ok := free >= s.MinFreeBytes
	p.mu.Lock()
	if !ok && !p.lowSpace {
		p.log.Warn("staging volume low on space; pausing downloads", "free_bytes", free, "min_free_bytes", s.MinFreeBytes)
	} else if ok && p.lowSpace {
		p.log.Info("staging volume has space again; resuming downloads")
	}
	p.lowSpace = !ok
	p.mu.Unlock()
	return ok
}

func safeRelPath(camPath string) string {
	parts := strings.Split(camPath, "/")
	out := parts[:0]
	for _, s := range parts {
		if s == "" || s == "." || s == ".." {
			continue
		}
		out = append(out, s)
	}
	return filepath.Join(out...)
}

func ident(f store.File) store.Discovered {
	return store.Discovered{Camera: f.Camera, Slot: f.Slot, Path: f.Path, CapturedAt: f.CapturedAt, Size: f.Size}
}

type dlResult int

const (
	dlNone   dlResult = iota // nothing (more) to download in this listing
	dlOK                     // one file downloaded
	dlFailed                 // one file failed; the next may work
	dlLost                   // the session is gone or stalled; end the sync
)

// downloadNext transfers one due file of the current listing. Due files that
// are not on the card are skipped.
func (p *pipeline) downloadNext(ctx context.Context, cam *ptpip.Client, s settings) (dlResult, error) {
	camID := p.cameraID()
	due, err := p.store.DuePending(ctx, camID, 200)
	if err != nil {
		return dlNone, err
	}
	var f store.File
	var r ref
	found := false
	p.mu.Lock()
	for _, cand := range due {
		if x, ok := p.files[itemKey(ident(cand))]; ok {
			f, r, found = cand, x, true
			break
		}
	}
	p.mu.Unlock()
	if !found || !p.freeSpaceOK(s) {
		return dlNone, nil
	}

	db := context.WithoutCancel(ctx)
	if err := p.store.StartDownload(db, f.ID); err != nil {
		return dlNone, err
	}
	fail := func(err error, count bool) (dlResult, error) {
		next := time.Now()
		if count {
			next = next.Add(s.Retry.Delay(f.DownloadAttempts + 1))
			p.Metrics.DownloadFailures.Add(1)
		}
		p.log.Warn("download failed", "file", f.Path, "err", err, "retry_at", next.UTC().Format(time.RFC3339), "counted", count)
		if ferr := p.store.FailDownload(db, f.ID, err.Error(), next, count); ferr != nil {
			return dlFailed, ferr
		}
		return dlFailed, nil
	}
	partialDir := filepath.Join(p.staging, ".partial")
	if err := os.MkdirAll(partialDir, 0o755); err != nil {
		return fail(err, true)
	}
	partPath := filepath.Join(partialDir, fmt.Sprintf("%d-%s.part", f.ID, path.Base(f.Path)))
	out, err := os.Create(partPath)
	if err != nil {
		return fail(err, true)
	}

	log := p.log.With("file", f.Path, "slot", f.Slot, "id", f.ID)
	started := time.Now()
	log.Info("downloading", "attempt", f.DownloadAttempts+1, "bytes", f.Size)
	tr := newTransfer(f.Path, f.Size)
	p.transfer.Store(tr)

	// No bytes for stall_timeout: cancelling breaks the session, which the
	// supervisor re-establishes.
	dctx, cancel := context.WithCancel(ctx)
	var stalled atomic.Bool
	go func() {
		t := time.NewTicker(max(s.Stall/4, 5*time.Millisecond))
		defer t.Stop()
		for {
			select {
			case <-dctx.Done():
				return
			case <-t.C:
				if time.Since(time.Unix(0, tr.lastWrite.Load())) > s.Stall {
					stalled.Store(true)
					cancel()
					return
				}
			}
		}
	}()
	n, err := cam.SonyDownloadContentFile(dctx, r.slot, r.content, r.file, uint64(f.Size), progressWriter{out, tr}, nil)
	cancel()
	p.transfer.Store(nil)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil && n != f.Size {
		err = fmt.Errorf("%w: got %d of %d bytes", verify.ErrIncomplete, n, f.Size)
	}
	if err == nil {
		err = p.finishDownload(db, f, partPath, log, started)
	}
	if err != nil {
		os.Remove(partPath)
		if stalled.Load() {
			res, ferr := fail(fmt.Errorf("stalled: no data for %s", s.Stall), true)
			if ferr == nil {
				res = dlLost
			}
			return res, ferr
		}
		transient := ctx.Err() != nil || !p.env.Camera.Status().Connected
		res, ferr := fail(err, !transient)
		if transient && ferr == nil {
			res = dlLost
		}
		return res, ferr
	}
	return dlOK, nil
}

// stagedPath is where the verified copy of f lives until it is uploaded.
func (p *pipeline) stagedPath(f store.File) string {
	dir, base := filepath.Split(safeRelPath(f.Path))
	return filepath.Join(p.staging, f.Camera, fmt.Sprintf("slot%d", f.Slot), dir, fmt.Sprintf("%d_%s", f.ID, base))
}

func (p *pipeline) finishDownload(db context.Context, f store.File, partPath string, log *slog.Logger, started time.Time) error {
	v, err := verify.File(partPath, f.Path)
	if err != nil {
		return fmt.Errorf("verify: %w", err)
	}
	final := p.stagedPath(f)
	if err := os.MkdirAll(filepath.Dir(final), 0o755); err != nil {
		return err
	}
	if err := os.Rename(partPath, final); err != nil {
		return err
	}
	if err := p.store.FinishDownload(db, f.ID, final, v.Size, v.SHA1); err != nil {
		os.Remove(final)
		return err
	}
	p.Metrics.Downloads.Add(1)
	p.Metrics.DownloadBytes.Add(v.Size)
	p.Metrics.lastDownload.Store(time.Now().Unix())
	log.Info("downloaded", "bytes", v.Size, "sha1", v.SHA1, "duration", time.Since(started).Round(time.Millisecond).String())
	wake(p.uploadWake)
	return nil
}

// ---------------------------------------------------------------- delete on camera

// deleteDone removes contents from the card once every file of them is done.
func (p *pipeline) deleteDone(ctx context.Context, cam *ptpip.Client) {
	db := context.WithoutCancel(ctx)
	rows, err := p.store.DoneNotDeleted(ctx, p.cameraID())
	if err != nil {
		p.log.Error("query camera deletes", "err", err)
		return
	}
	cands := map[int]content{}
	p.mu.Lock()
	for _, f := range rows {
		if i, ok := p.byID[itemKey(ident(f))]; ok {
			cands[i] = p.contents[i]
		}
	}
	p.mu.Unlock()
	for _, c := range cands {
		if c.protected || len(c.files) == 0 {
			continue
		}
		var ids []int64
		all := true
		for _, d := range c.files {
			f, ok, err := p.store.Lookup(ctx, d)
			if err != nil || !ok || f.Status != store.StatusDone {
				all = false
				break
			}
			ids = append(ids, f.ID)
		}
		if !all {
			continue
		}
		if err := cam.SonyDeleteContent(ctx, c.slot, c.id); err != nil {
			p.log.Warn("delete on camera", "slot", c.slot, "content", c.id, "err", err)
			continue
		}
		if err := p.store.MarkCameraDeleted(db, ids...); err != nil {
			p.log.Error("record camera delete", "err", err)
		}
		p.mu.Lock()
		for _, d := range c.files {
			delete(p.files, itemKey(d))
		}
		p.mu.Unlock()
		p.Metrics.CameraDeletes.Add(1)
		p.log.Info("deleted from camera", "slot", c.slot, "content", c.id, "files", len(c.files))
	}
}

// ---------------------------------------------------------------- Immich health

func (p *pipeline) immichLoop(ctx context.Context) {
	for ctx.Err() == nil {
		im := p.client(p.cfg())
		if im == nil {
			p.immichOK.Store(false)
			sleep(ctx, p.idle, p.immichWake)
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		v, err := im.CheckCompatible(cctx)
		cancel()
		was := p.immichOK.Load()
		if err != nil {
			p.immichOK.Store(false)
			p.immichErr.Store(err.Error())
			if was || p.immichVersion.Load().(string) == "" {
				p.log.Error("immich unavailable", "err", err)
			}
		} else {
			p.immichOK.Store(true)
			p.immichErr.Store("")
			p.immichVersion.Store(v.String())
			p.checkKey(ctx, im)
			if !was {
				p.log.Info("immich available", "version", v.String())
				wake(p.uploadWake)
				wake(p.postWake)
			}
		}
		interval := time.Minute
		if !p.immichOK.Load() {
			interval = 15 * time.Second
		}
		sleep(ctx, interval, p.immichWake)
	}
}

// ---------------------------------------------------------------- upload

func (p *pipeline) uploadLoop(ctx context.Context) {
	var wg sync.WaitGroup
	defer wg.Wait()
	var inflight atomic.Int32
	for ctx.Err() == nil {
		s := p.cfg()
		im := p.client(s)
		if im == nil || !p.immichOK.Load() || int(inflight.Load()) >= s.Concurrency {
			sleep(ctx, p.idle, p.uploadWake)
			continue
		}
		due, err := p.store.DueDownloaded(ctx, s.Concurrency*2)
		if err != nil {
			p.log.Error("query uploads", "err", err)
			sleep(ctx, p.idle, nil)
			continue
		}
		claimed := false
		for _, f := range due {
			if int(inflight.Load()) >= s.Concurrency {
				break
			}
			if err := p.store.StartUpload(ctx, f.ID); err != nil {
				continue
			}
			claimed = true
			inflight.Add(1)
			wg.Add(1)
			go func() {
				defer wg.Done()
				p.upload(ctx, im, s, f)
				inflight.Add(-1)
				wake(p.uploadWake)
			}()
		}
		if !claimed {
			sleep(ctx, p.idle, p.uploadWake)
		}
	}
}

func (p *pipeline) upload(ctx context.Context, im *Client, s settings, f store.File) {
	db := context.WithoutCancel(ctx)
	log := p.log.With("file", f.Path, "slot", f.Slot, "id", f.ID)
	fail := func(err error) {
		p.Metrics.UploadFailures.Add(1)
		next := time.Now().Add(s.Retry.Delay(f.UploadAttempts + 1))
		if errors.Is(err, context.Canceled) {
			next = time.Now()
		}
		if IsAuthError(err) {
			log.Error("immich rejected the API key or it lacks permissions (needs asset.upload, album.read, album.create, albumAsset.create, stack.create)", "err", err)
		} else {
			log.Warn("upload failed", "err", err, "retry_at", next.UTC().Format(time.RFC3339))
		}
		var ae *APIError
		if !errors.As(err, &ae) && !errors.Is(err, context.Canceled) {
			// Network-level failure: let the health loop re-check Immich.
			p.immichOK.Store(false)
			wake(p.immichWake)
		}
		if ferr := p.store.FailUpload(db, f.ID, err.Error(), next); ferr != nil {
			log.Error("record upload failure", "err", ferr)
		}
	}

	if _, err := os.Stat(f.LocalPath); err != nil {
		log.Warn("staged file missing; will download again", "path", f.LocalPath, "err", err)
		if err := p.store.Redownload(db, f.ID, "staged file missing: "+err.Error()); err != nil {
			log.Error("reset for re-download", "err", err)
		}
		return
	}

	// Same bytes already uploaded under another path (e.g. after the camera
	// changed its path scheme): no need to ask Immich or upload again.
	if f.SHA1 != "" {
		if assetID, same, err := p.store.UploadedSHA1(db, f.SHA1, f.ID); err != nil {
			log.Warn("local duplicate check", "err", err)
		} else if assetID != "" {
			if err := p.store.FinishUpload(db, f.ID, assetID, true); err != nil {
				log.Error("record duplicate", "err", err)
				return
			}
			p.Metrics.UploadDuplicates.Add(1)
			log.Info("identical file already uploaded; skipped upload", "asset_id", assetID, "same_as", same)
			wake(p.postWake)
			return
		}
	}

	// The id is echoed back; Immich documents it as e.g. the filename.
	checkID := path.Base(f.Path)
	results, err := im.BulkUploadCheck(ctx, []CheckItem{{ID: checkID, Checksum: f.SHA1}})
	if err != nil {
		fail(err)
		return
	}
	for _, r := range results {
		if r.ID != checkID || r.Action != "reject" {
			continue
		}
		switch {
		case r.Reason == "duplicate" && r.AssetID != "":
			if err := p.store.FinishUpload(db, f.ID, r.AssetID, true); err != nil {
				log.Error("record duplicate", "err", err)
				return
			}
			p.Metrics.UploadDuplicates.Add(1)
			p.Metrics.lastUpload.Store(time.Now().Unix())
			log.Info("already in immich; skipped upload", "asset_id", r.AssetID, "trashed", r.IsTrashed)
			wake(p.postWake)
			return
		case r.Reason == "unsupported-format":
			if err := p.store.Reject(db, f.ID, "immich: unsupported format"); err != nil {
				log.Error("record rejection", "err", err)
			}
			p.Metrics.Rejected.Add(1)
			log.Warn("immich does not support this file format; leaving it in staging", "path", f.LocalPath)
			return
		}
	}

	started := time.Now()
	res, err := im.Upload(ctx, UploadRequest{
		Path: f.LocalPath, Filename: path.Base(f.Path), SHA1: f.SHA1,
		CreatedAt: f.CapturedAt, ModifiedAt: f.CapturedAt,
	})
	if err != nil {
		fail(err)
		return
	}
	if err := p.store.FinishUpload(db, f.ID, res.ID, res.Duplicate); err != nil {
		log.Error("record upload", "err", err)
		return
	}
	if res.Duplicate {
		p.Metrics.UploadDuplicates.Add(1)
	} else {
		p.Metrics.Uploads.Add(1)
		p.Metrics.UploadBytes.Add(f.Size)
	}
	p.Metrics.lastUpload.Store(time.Now().Unix())
	log.Info("uploaded", "asset_id", res.ID, "duplicate", res.Duplicate, "bytes", f.Size, "duration", time.Since(started).Round(time.Millisecond).String())
	wake(p.postWake)
}

// ---------------------------------------------------------------- post-processing

func (p *pipeline) postLoop(ctx context.Context) {
	for ctx.Err() == nil {
		s := p.cfg()
		im := p.client(s)
		var due []store.File
		if im != nil && p.immichOK.Load() {
			var err error
			if due, err = p.store.DueUploaded(ctx, 200); err != nil {
				p.log.Error("query post-processing", "err", err)
			}
		}
		if len(due) == 0 {
			sleep(ctx, p.idle, p.postWake)
			continue
		}
		p.postProcess(ctx, im, s, due)
	}
}

func (p *pipeline) postProcess(ctx context.Context, im *Client, s settings, files []store.File) {
	// Rows that are not finished here stay "uploaded" and are retried later.
	db := context.WithoutCancel(ctx)

	// 1. Delete staged copies; the asset is safely in Immich.
	if s.DeleteLocal {
		for i := range files {
			f := &files[i]
			if f.LocalDeleted {
				continue
			}
			if err := os.Remove(f.LocalPath); err != nil && !errors.Is(err, os.ErrNotExist) {
				p.log.Warn("delete staged file", "path", f.LocalPath, "err", err)
				continue
			}
			p.pruneEmptyDirs(filepath.Dir(f.LocalPath))
			if err := p.store.MarkLocalDeleted(db, f.ID); err == nil {
				f.LocalDeleted = true
			}
		}
	}

	// 1b. Delete the library copy of gallery imports, if asked to. It is only
	// removed when it still has the size that was uploaded.
	if s.DeleteLibrary {
		for i := range files {
			f := &files[i]
			if f.SourcePath == "" {
				continue
			}
			st, err := os.Stat(f.SourcePath)
			switch {
			case err == nil && st.Size() == f.Size:
				if err := os.Remove(f.SourcePath); err != nil {
					p.log.Warn("delete library file", "path", f.SourcePath, "err", err)
					continue
				}
			case err != nil && !errors.Is(err, os.ErrNotExist):
				p.log.Warn("stat library file", "path", f.SourcePath, "err", err)
				continue
			}
			if err := p.store.SetSourcePath(db, f.ID, ""); err == nil {
				f.SourcePath = ""
			}
		}
	}

	// 2. Album.
	if s.Album == "" {
		for i := range files {
			files[i].AlbumAdded = true
		}
	} else {
		p.addToAlbum(ctx, im, s, files)
	}

	// 3. RAW+JPEG stacks.
	for i := range files {
		f := &files[i]
		if f.Stacked {
			continue
		}
		if !s.Stack {
			f.Stacked = true
			continue
		}
		p.stackGroup(ctx, im, s, f, files)
	}

	// 4. Finish rows with nothing left to do.
	for _, f := range files {
		if (f.LocalDeleted || !s.DeleteLocal) && (f.SourcePath == "" || !s.DeleteLibrary) && f.AlbumAdded && f.Stacked {
			if err := p.store.Done(db, f.ID); err != nil && !errors.Is(err, store.ErrInvalidTransition) {
				p.log.Error("finish file", "id", f.ID, "err", err)
			}
		}
	}
}

func (p *pipeline) pruneEmptyDirs(dir string) {
	root := filepath.Clean(p.staging)
	for dir = filepath.Clean(dir); strings.HasPrefix(dir, root+string(filepath.Separator)); dir = filepath.Dir(dir) {
		if os.Remove(dir) != nil { // fails if not empty
			return
		}
	}
}

// postpone defers post-processing; without count it is just waiting (e.g.
// for the RAW sibling) and neither an attempt nor an error.
func (p *pipeline) postpone(s settings, f *store.File, cause string, count bool) {
	next := time.Now().Add(p.idle)
	if count {
		next = time.Now().Add(s.Retry.Delay(f.PostAttempts + 1))
	} else {
		cause = ""
	}
	if err := p.store.DeferPost(context.Background(), f.ID, cause, next, count); err != nil {
		p.log.Error("defer post-processing", "id", f.ID, "err", err)
	}
	if count {
		f.PostAttempts++
	}
}

func (p *pipeline) addToAlbum(ctx context.Context, im *Client, s settings, files []store.File) {
	db := context.WithoutCancel(ctx)
	var pending []*store.File
	for i := range files {
		if !files[i].AlbumAdded {
			pending = append(pending, &files[i])
		}
	}
	if len(pending) == 0 {
		return
	}
	p.mu.Lock()
	if p.album != s.Album {
		p.album, p.albumID = s.Album, ""
	}
	albumID := p.albumID
	p.mu.Unlock()
	if albumID == "" {
		id, err := im.FindOrCreateAlbum(ctx, s.Album)
		if err != nil {
			p.log.Warn("find or create album", "album", s.Album, "err", err)
			for _, f := range pending {
				p.postpone(s, f, "album: "+err.Error(), true)
			}
			return
		}
		p.mu.Lock()
		p.albumID = id
		p.mu.Unlock()
		albumID = id
		p.log.Info("using album", "album", s.Album, "album_id", id)
	}
	ids := make([]string, 0, len(pending))
	byAsset := map[string][]*store.File{}
	for _, f := range pending {
		if _, seen := byAsset[f.AssetID]; !seen {
			ids = append(ids, f.AssetID)
		}
		byAsset[f.AssetID] = append(byAsset[f.AssetID], f)
	}
	results, err := im.AddToAlbum(ctx, albumID, ids)
	if err != nil {
		var ae *APIError
		if errors.As(err, &ae) && ae.Status == 400 {
			// The album may have been deleted in Immich; look it up again next time.
			p.mu.Lock()
			p.albumID = ""
			p.mu.Unlock()
		}
		p.log.Warn("add to album", "err", err)
		for _, f := range pending {
			p.postpone(s, f, "album: "+err.Error(), true)
		}
		return
	}
	for _, r := range results {
		for _, f := range byAsset[r.ID] {
			if r.Success || r.Error == "duplicate" || f.PostAttempts >= maxPostAttempts {
				if !r.Success && r.Error != "duplicate" {
					p.log.Warn("giving up adding asset to album", "asset_id", r.ID, "error", r.Error)
				}
				if err := p.store.MarkAlbumAdded(db, f.ID); err == nil {
					f.AlbumAdded = true
				}
			} else {
				p.postpone(s, f, "album: "+r.Error, true)
			}
		}
	}
}

var primaryExt = map[string]int{".JPG": 0, ".JPEG": 0, ".HIF": 1, ".HEIF": 1, ".HEIC": 1}

func (p *pipeline) stackGroup(ctx context.Context, im *Client, s settings, f *store.File, batch []store.File) {
	db := context.WithoutCancel(ctx)
	members, err := p.store.GroupMembers(db, f.GroupKey)
	if err != nil {
		p.log.Error("load group", "err", err)
		return
	}
	markAll := func() {
		ids := make([]int64, 0, len(members))
		for _, m := range members {
			ids = append(ids, m.ID)
		}
		if err := p.store.MarkStacked(db, ids...); err != nil {
			p.log.Error("mark stacked", "err", err)
			return
		}
		for i := range batch {
			for _, m := range members {
				if batch[i].ID == m.ID {
					batch[i].Stacked = true
				}
			}
		}
	}
	var assets []store.File
	for _, m := range members {
		switch m.Status {
		case store.StatusPending, store.StatusDownloading, store.StatusDownloaded, store.StatusUploading:
			if time.Since(f.CreatedAt) > groupWaitLimit {
				p.log.Warn("sibling never uploaded; giving up on stack", "file", f.Path, "sibling", m.Path)
				markAll()
				return
			}
			p.postpone(s, f, "waiting for "+path.Base(m.Path)+" before stacking", false)
			return
		case store.StatusUploaded, store.StatusDone:
			if m.AssetID != "" {
				assets = append(assets, m)
			}
		}
	}
	sort.SliceStable(assets, func(i, j int) bool {
		ri, iok := primaryExt[strings.ToUpper(path.Ext(assets[i].Path))]
		rj, jok := primaryExt[strings.ToUpper(path.Ext(assets[j].Path))]
		if iok != jok {
			return iok
		}
		if ri != rj {
			return ri < rj
		}
		return assets[i].Path < assets[j].Path
	})
	var ids []string
	seen := map[string]bool{}
	for _, a := range assets {
		if !seen[a.AssetID] {
			seen[a.AssetID] = true
			ids = append(ids, a.AssetID)
		}
	}
	if len(ids) < 2 {
		markAll()
		return
	}
	if _, err := im.CreateStack(ctx, ids); err != nil {
		if IsClientError(err) && f.PostAttempts+1 >= maxPostAttempts {
			p.log.Warn("giving up on stack", "file", f.Path, "err", err)
			markAll()
			return
		}
		p.log.Warn("create stack", "file", f.Path, "err", err)
		p.postpone(s, f, "stack: "+err.Error(), true)
		return
	}
	p.Metrics.Stacks.Add(1)
	p.log.Info("stacked", "group", f.GroupKey, "primary", path.Base(assets[0].Path), "assets", len(ids))
	markAll()
}

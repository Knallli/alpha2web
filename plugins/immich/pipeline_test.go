package immich

import (
	"cmp"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/knallli/alpha2go/ptpip/ptpiptest"
	"github.com/knallli/alpha2web/internal/backoff"
	"github.com/knallli/alpha2web/internal/camera"
	"github.com/knallli/alpha2web/plugin"
	"github.com/knallli/alpha2web/plugins/immich/immichtest"
	"github.com/knallli/alpha2web/plugins/immich/store"
)

// ----- sample media with valid structure and unique content

func jpeg(seed string, n int) []byte {
	b := []byte{0xFF, 0xD8, 0xFF, 0xE1}
	b = append(b, []byte(seed)...)
	b = append(b, make([]byte, n)...)
	return append(b, 0xFF, 0xD9)
}

func arw(seed string, n int) []byte {
	b := []byte{'I', 'I', 0x2A, 0, 8, 0, 0, 0}
	b = append(b, []byte(seed)...)
	return append(b, make([]byte, n)...)
}

func mp4(seed string, n int) []byte {
	box := func(typ string, payload []byte) []byte {
		h := make([]byte, 8)
		binary.BigEndian.PutUint32(h, uint32(8+len(payload)))
		copy(h[4:], typ)
		return append(h, payload...)
	}
	return append(box("ftyp", []byte("XAVC"+seed)), box("mdat", make([]byte, n))...)
}

func put(t *testing.T, root, rel string, data []byte, mtime time.Time) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	// Written beside the card and renamed in: a sync must never see half a file.
	tmp := filepath.Join(filepath.Dir(root), ".put")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(tmp, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, p); err != nil {
		t.Fatal(err)
	}
}

var shot = time.Date(2026, 9, 30, 8, 15, 30, 0, time.UTC)

// sampleCard has four uploadable files in three contents plus an XML sidecar.
func sampleCard(t *testing.T) string {
	dir := t.TempDir()
	put(t, dir, "slot1/DCIM/100MSDCF/DSC00001.JPG", jpeg("1", 300_000), shot)
	put(t, dir, "slot1/DCIM/100MSDCF/DSC00001.ARW", arw("1", 900_000), shot)
	put(t, dir, "slot1/DCIM/100MSDCF/DSC00002.JPG", jpeg("2", 200_000), shot.Add(time.Minute))
	put(t, dir, "slot2/PRIVATE/M4ROOT/CLIP/C0001.MP4", mp4("1", 1_500_000), shot.Add(2*time.Minute))
	put(t, dir, "slot2/PRIVATE/M4ROOT/CLIP/C0001M01.XML", []byte("<xml/>"), shot.Add(2*time.Minute))
	return dir
}

func startCamera(t *testing.T, opts ptpiptest.Options) *ptpiptest.Server {
	t.Helper()
	opts.A6700 = true
	cam, err := ptpiptest.Start(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cam.Close)
	return cam
}

// ----- harness

type harness struct {
	t       *testing.T
	srv     *immichtest.Server
	staging string
	dbPath  string
	data    string
	st      *store.Store
	p       *pipeline
	sup     *camera.Supervisor
	cancel  context.CancelFunc
	linger  time.Duration // camera session linger; 0 = an hour
	done    chan struct{}

	mu  sync.Mutex
	cfg map[string]string
}

func quietLog() *slog.Logger {
	if os.Getenv("TEST_VERBOSE") != "" {
		return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newHarness(t *testing.T, srv *immichtest.Server, staging, dbPath string) *harness {
	if srv == nil {
		srv = immichtest.New(t)
	}
	if staging == "" {
		staging = t.TempDir()
	}
	if dbPath == "" {
		dbPath = filepath.Join(t.TempDir(), "immich.db")
	}
	return &harness{t: t, srv: srv, staging: staging, dbPath: dbPath, data: t.TempDir(), cfg: map[string]string{}}
}

// set changes one setting; a running pipeline sees it on its next iteration.
func (h *harness) set(k, v string) { h.mu.Lock(); h.cfg[k] = v; h.mu.Unlock() }

func (h *harness) config() map[string]string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := map[string]string{
		"url": h.srv.URL, "api_key": h.srv.APIKey,
		"retry_base_delay": "20ms", "retry_max_delay": "100ms", "min_free_space_mb": "0",
	}
	for k, v := range h.cfg {
		out[k] = v
	}
	return out
}

func (h *harness) start(cam *ptpiptest.Server, tweak ...func(*pipeline)) {
	h.t.Helper()
	h.startAt(cam.Addr(), tweak...)
}

func (h *harness) startAt(addr string, tweak ...func(*pipeline)) {
	h.t.Helper()
	st, err := store.Open(h.dbPath)
	if err != nil {
		h.t.Fatal(err)
	}
	if _, err := st.RecoverInterrupted(context.Background()); err != nil {
		h.t.Fatal(err)
	}
	h.st = st
	log := quietLog()
	h.sup = camera.New(camera.Config{Host: addr, Linger: cmp.Or(h.linger, time.Hour)}, backoff.Policy{Base: 10 * time.Millisecond, Max: 50 * time.Millisecond}, log)
	env := plugin.Env{Camera: h.sup, Log: log, DataDir: h.data, Config: h.config}
	h.p = newPipeline(env, st, h.staging)
	h.p.idle = 20 * time.Millisecond
	for _, f := range tweak {
		f(h.p)
	}
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	h.done = make(chan struct{})
	go func() {
		supDone := make(chan struct{})
		go func() { h.sup.Run(ctx); close(supDone) }()
		h.p.run(ctx)
		<-supDone
		close(h.done)
	}()
}

func (h *harness) stop() {
	h.cancel()
	<-h.done
	h.st.Close()
}

func (h *harness) counts() map[store.Status]int {
	c, err := h.st.Counts(context.Background())
	if err != nil {
		h.t.Fatal(err)
	}
	return c
}

func (h *harness) waitCounts(want map[store.Status]int) {
	h.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		c := h.counts()
		ok := true
		for st, n := range want {
			ok = ok && c[st] == n
		}
		if ok {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	h.t.Fatalf("timed out; want %v, counts=%v", want, h.counts())
}

// syncWaitFor asks for a sync again and again (a request while one runs is
// ignored) until cond holds.
func (h *harness) syncWaitFor(what string, cond func() bool) {
	h.t.Helper()
	waitFor(h.t, what, func() bool {
		h.p.StartSync()
		return cond()
	})
}

// syncUntil syncs until the file counts match want.
func (h *harness) syncUntil(want map[store.Status]int) {
	h.t.Helper()
	h.syncWaitFor(fmt.Sprint(want), func() bool {
		c := h.counts()
		for st, n := range want {
			if c[st] != n {
				return false
			}
		}
		return true
	})
}

func (h *harness) waitDone(n int) {
	h.t.Helper()
	h.syncUntil(map[store.Status]int{store.StatusDone: n})
}

func stagedFiles(t *testing.T, root string) []string {
	var out []string
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			out = append(out, p)
		}
		return nil
	})
	return out
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for i := 0; i < 500; i++ {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// ----- tests

func TestEndToEnd(t *testing.T) {
	cam := startCamera(t, ptpiptest.Options{Dir: sampleCard(t)})
	h := newHarness(t, nil, "", "")
	h.set("album", "Sony a6700")
	h.start(cam)
	defer h.stop()
	h.waitDone(4)

	assets := h.srv.Assets()
	if len(assets) != 4 {
		t.Fatalf("want 4 assets, got %d", len(assets))
	}
	for _, a := range assets {
		if a.Filename == "DSC00001.JPG" && a.CreatedAt != "2026-09-30T08:15:30.000Z" {
			t.Errorf("fileCreatedAt = %s", a.CreatedAt)
		}
		if strings.HasSuffix(a.Filename, ".XML") {
			t.Error("XML sidecar should be skipped by default")
		}
	}
	if got := len(h.srv.AlbumAssets("Sony a6700")); got != 4 {
		t.Errorf("album has %d assets", got)
	}
	stacks := h.srv.Stacks()
	if len(stacks) != 1 || len(stacks[0]) != 2 {
		t.Fatalf("stacks = %v", stacks)
	}
	if primary, _ := h.srv.AssetByID(stacks[0][0]); primary.Filename != "DSC00001.JPG" {
		t.Errorf("stack primary = %s, want the JPEG", primary.Filename)
	}
	if left := stagedFiles(t, h.staging); len(left) != 0 {
		t.Errorf("staging not cleaned: %v", left)
	}
	if d := cam.Deletes(); len(d) != 0 {
		t.Errorf("camera deletes by default: %v", d)
	}
}

func TestStackingOffAndNoSkip(t *testing.T) {
	cam := startCamera(t, ptpiptest.Options{Dir: sampleCard(t)})
	h := newHarness(t, nil, "", "")
	h.set("stack_raw_jpeg", "false")
	h.set("skip_extensions", "none")
	h.start(cam)
	defer h.stop()
	h.waitDone(5)
	if n := len(h.srv.Stacks()); n != 0 {
		t.Errorf("stacks = %d", n)
	}
}

func TestSyncFindsNewCapture(t *testing.T) {
	card := sampleCard(t)
	cam := startCamera(t, ptpiptest.Options{Dir: card})
	h := newHarness(t, nil, "", "")
	h.start(cam)
	defer h.stop()
	h.waitDone(4)
	n := h.p.Metrics.Reconciles.Load()
	put(t, card, "slot1/DCIM/100MSDCF/DSC00003.JPG", jpeg("3", 1000), shot.Add(time.Hour))
	h.waitDone(5)
	if h.p.Metrics.Reconciles.Load() == n {
		t.Error("new shot was not noticed by a new listing")
	}
}

func TestImmichUnavailableThenRetry(t *testing.T) {
	cam := startCamera(t, ptpiptest.Options{Dir: sampleCard(t)})
	h := newHarness(t, nil, "", "")
	h.srv.FailNext(6)
	h.start(cam)
	defer h.stop()
	h.waitDone(4)
	if n := len(h.srv.Assets()); n != 4 {
		t.Fatalf("assets = %d", n)
	}
	if h.p.Metrics.UploadFailures.Load() == 0 {
		t.Error("expected injected upload failures")
	}
}

func TestDownloadInterruptedIsRetried(t *testing.T) {
	card := t.TempDir()
	put(t, card, "slot1/DCIM/100MSDCF/DSC00001.JPG", jpeg("1", 300_000), shot)
	cam := startCamera(t, ptpiptest.Options{Dir: card, DropAfter: 100_000})
	h := newHarness(t, nil, "", "")
	h.start(cam)
	defer h.stop()
	h.waitDone(1)
	if n := len(h.srv.Assets()); n != 1 {
		t.Fatalf("assets = %d", n)
	}
	if n := len(cam.ContentRequests()); n < 2 {
		t.Errorf("only %d content requests; the drop never happened", n)
	}
	if h.sup.Sessions() < 2 {
		t.Error("camera did not reconnect")
	}
	for _, f := range stagedFiles(t, h.staging) {
		t.Errorf("left behind: %s", f)
	}
}

// proxy forwards to the camera but stops delivering camera data on the first
// session once limit bytes have passed: a transfer that hangs.
type proxy struct {
	ln    net.Listener
	limit int64
	sent  atomic.Int64
	conns atomic.Int32
}

func startProxy(t *testing.T, target string, limit int64) *proxy {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &proxy{ln: ln, limit: limit}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go p.serve(c, target, p.conns.Add(1) <= 2)
		}
	}()
	return p
}

func (p *proxy) serve(c net.Conn, target string, first bool) {
	up, err := net.Dial("tcp", target)
	if err != nil {
		c.Close()
		return
	}
	closed := make(chan struct{})
	go func() {
		io.Copy(up, c)
		close(closed)
		up.Close()
		c.Close()
	}()
	buf := make([]byte, 32<<10)
	for {
		n, err := up.Read(buf)
		if n > 0 {
			if first && p.sent.Add(int64(n)) > p.limit {
				<-closed
				return
			}
			c.Write(buf[:n])
		}
		if err != nil {
			c.Close()
			return
		}
	}
}

func TestStalledDownloadIsCancelledAndRetried(t *testing.T) {
	card := t.TempDir()
	put(t, card, "slot1/DCIM/100MSDCF/DSC00001.JPG", jpeg("1", 3_000_000), shot)
	cam := startCamera(t, ptpiptest.Options{Dir: card})
	px := startProxy(t, cam.Addr(), 1_000_000)
	h := newHarness(t, nil, "", "")
	h.set("stall_timeout", "300ms")
	h.startAt(px.ln.Addr().String())
	defer h.stop()
	h.waitDone(1)
	if h.p.Metrics.DownloadFailures.Load() == 0 {
		t.Error("the stall was not counted as a failed attempt")
	}
	if n := len(h.srv.Assets()); n != 1 {
		t.Fatalf("assets = %d", n)
	}
}

func TestMinFreeSpaceGatesDownloads(t *testing.T) {
	cam := startCamera(t, ptpiptest.Options{Dir: sampleCard(t)})
	var free atomic.Int64
	h := newHarness(t, nil, "", "")
	h.set("min_free_space_mb", "100")
	h.start(cam, func(p *pipeline) {
		p.statfs = func(string) (int64, error) { return free.Load(), nil }
	})
	defer h.stop()
	h.syncWaitFor("listing", func() bool { return h.counts()[store.StatusPending] == 4 })
	time.Sleep(300 * time.Millisecond)
	if n := h.p.Metrics.Downloads.Load(); n != 0 || h.counts()[store.StatusPending] != 4 {
		t.Fatalf("downloaded %d files with the disk full", n)
	}
	free.Store(200 << 20)
	h.waitDone(4)
}

func TestDeleteLocalOffKeepsStagedFiles(t *testing.T) {
	cam := startCamera(t, ptpiptest.Options{Dir: sampleCard(t)})
	h := newHarness(t, nil, "", "")
	h.set("delete_local", "false")
	h.start(cam)
	defer h.stop()
	h.waitDone(4)
	if left := stagedFiles(t, h.staging); len(left) != 4 {
		t.Errorf("staged files = %v", left)
	}
}

func TestDeleteOnCameraWaitsForAllFiles(t *testing.T) {
	cam := startCamera(t, ptpiptest.Options{Dir: sampleCard(t)})
	h := newHarness(t, nil, "", "")
	h.srv.RejectExtension(".ARW") // the RAW never gets done
	h.set("delete_on_camera", "true")
	h.start(cam)
	defer h.stop()
	h.syncUntil(map[store.Status]int{store.StatusDone: 3, store.StatusRejected: 1})
	// the delete step of a sync only sees files finished before it
	h.syncWaitFor("two deletes", func() bool { return len(cam.Deletes()) == 2 })
	for range 3 { // later syncs must not delete again
		h.p.StartSync()
		time.Sleep(100 * time.Millisecond)
	}
	got := cam.Deletes()
	if len(got) != 2 {
		t.Fatalf("deletes = %v", got)
	}
	ids := map[uint32]bool{}
	for _, d := range got {
		ids[d.ContentID] = true
	}
	// The DSC00001 content (JPG done, ARW rejected) must still be on the card.
	c, err := h.sup.Client()
	if err != nil {
		t.Fatal(err)
	}
	cs, err := c.SonyListAllContents(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range cs {
		if len(x.Files) == 2 && ids[x.ID] {
			t.Errorf("content %d with a rejected RAW was deleted", x.ID)
		}
	}
}

func TestDeleteOnCameraOnceAfterEverythingUploaded(t *testing.T) {
	cam := startCamera(t, ptpiptest.Options{Dir: sampleCard(t)})
	h := newHarness(t, nil, "", "")
	h.set("delete_on_camera", "true")
	h.start(cam)
	defer h.stop()
	h.waitDone(4)
	h.syncWaitFor("three deletes", func() bool { return len(cam.Deletes()) == 3 })
	time.Sleep(300 * time.Millisecond)
	if n := len(cam.Deletes()); n != 3 {
		t.Fatalf("deletes = %v", cam.Deletes())
	}
	if len(h.srv.Assets()) != 4 {
		t.Errorf("assets = %d", len(h.srv.Assets()))
	}
}

func TestRestartIsIdempotent(t *testing.T) {
	cam := startCamera(t, ptpiptest.Options{Dir: sampleCard(t)})
	srv := immichtest.New(t)
	staging := t.TempDir()
	db := filepath.Join(t.TempDir(), "immich.db")

	h := newHarness(t, srv, staging, db)
	h.start(cam)
	h.waitDone(4)
	h.stop()
	uploads := srv.UploadCount()

	// Same database: nothing is downloaded or uploaded again.
	h2 := newHarness(t, srv, staging, db)
	h2.start(cam)
	time.Sleep(300 * time.Millisecond)
	h2.waitDone(4)
	if h2.p.Metrics.Downloads.Load() != 0 || srv.UploadCount() != uploads {
		t.Fatalf("restart re-transferred files: downloads=%d uploads=%d->%d", h2.p.Metrics.Downloads.Load(), uploads, srv.UploadCount())
	}
	h2.stop()

	// Lost database: files are pulled again but Immich deduplicates them.
	h3 := newHarness(t, srv, staging, filepath.Join(t.TempDir(), "fresh.db"))
	h3.start(cam)
	h3.waitDone(4)
	if n := len(srv.Assets()); n != 4 {
		t.Fatalf("duplicates created after state loss: %d assets", n)
	}
	if h3.p.Metrics.UploadDuplicates.Load() != 4 || srv.UploadCount() != uploads {
		t.Fatalf("expected dedupe via bulk-upload-check: dups=%d uploads=%d", h3.p.Metrics.UploadDuplicates.Load(), srv.UploadCount())
	}
	h3.stop()
}

func TestRestartResumesDownloadedFiles(t *testing.T) {
	cam := startCamera(t, ptpiptest.Options{Dir: sampleCard(t)})
	srv := immichtest.New(t)
	staging := t.TempDir()
	db := filepath.Join(t.TempDir(), "immich.db")

	// Immich is down: everything ends up downloaded in staging.
	srv.FailNext(1 << 20)
	h := newHarness(t, srv, staging, db)
	h.start(cam)
	h.syncUntil(map[store.Status]int{store.StatusDownloaded: 4})
	h.stop()

	srv.FailNext(0)
	h2 := newHarness(t, srv, staging, db)
	h2.start(cam)
	defer h2.stop()
	h2.waitDone(4)
	if n := h2.p.Metrics.Downloads.Load(); n != 0 {
		t.Errorf("staged files were downloaded again: %d", n)
	}
	if n := len(srv.Assets()); n != 4 {
		t.Errorf("assets = %d", n)
	}
}

func TestCrashRecovery(t *testing.T) {
	cam := startCamera(t, ptpiptest.Options{Dir: sampleCard(t)})
	db := filepath.Join(t.TempDir(), "immich.db")
	// Simulate a crash: a row stuck in downloading, identified like the listing does.
	s, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	s.UpsertDiscovered(ctx, []store.Discovered{{
		Camera: "ILCE-6700", Slot: 1, Path: "DCIM/100MSDCF/DSC00002.JPG",
		CapturedAt: shot.Add(time.Minute), Size: int64(len(jpeg("2", 200_000))), GroupKey: "x",
	}})
	due, _ := s.DuePending(ctx, "ILCE-6700", 10)
	if len(due) != 1 {
		t.Fatalf("setup: %v", due)
	}
	s.StartDownload(ctx, due[0].ID)
	s.Close()

	h := newHarness(t, nil, "", db)
	h.start(cam)
	defer h.stop()
	h.waitDone(4)
	var total int
	for _, n := range h.counts() {
		total += n
	}
	if total != 4 {
		t.Errorf("rows = %d; the seeded row did not match the listing", total)
	}
}

func TestUnsupportedFormatIsRejected(t *testing.T) {
	card := t.TempDir()
	put(t, card, "slot1/DCIM/100MSDCF/DSC00001.JPG", jpeg("a", 100), shot)
	put(t, card, "slot1/DCIM/100MSDCF/NOTES.XYZ", []byte("hello"), shot.Add(time.Minute))
	cam := startCamera(t, ptpiptest.Options{Dir: card})
	h := newHarness(t, nil, "", "")
	h.srv.RejectExtension(".XYZ")
	h.start(cam)
	defer h.stop()
	h.syncUntil(map[store.Status]int{store.StatusDone: 1, store.StatusRejected: 1})
}

func TestLateRAWStacksWithFinishedJPEG(t *testing.T) {
	card := t.TempDir()
	put(t, card, "slot1/DCIM/100MSDCF/DSC00001.JPG", jpeg("1", 300_000), shot)
	cam := startCamera(t, ptpiptest.Options{Dir: card})
	h := newHarness(t, nil, "", "")
	h.start(cam)
	defer h.stop()
	h.waitDone(1)

	put(t, card, "slot1/DCIM/100MSDCF/DSC00002.JPG", jpeg("2", 200_000), shot.Add(time.Minute))
	h.waitDone(2)
	put(t, card, "slot1/DCIM/100MSDCF/DSC00001.ARW", arw("1", 900_000), shot)
	h.waitDone(3)

	stacks := h.srv.Stacks()
	if len(stacks) != 1 || len(stacks[0]) != 2 {
		t.Fatalf("stacks = %v", stacks)
	}
	if primary, _ := h.srv.AssetByID(stacks[0][0]); primary.Filename != "DSC00001.JPG" {
		t.Errorf("stack primary = %s, want the JPEG", primary.Filename)
	}
}

func TestCameraReconnect(t *testing.T) {
	card := sampleCard(t)
	cam := startCamera(t, ptpiptest.Options{Dir: card})
	h := newHarness(t, nil, "", "")
	h.start(cam)
	defer h.stop()
	h.waitDone(4)
	n := h.p.Metrics.Reconciles.Load()
	cam.DropConnections()
	put(t, card, "slot1/DCIM/100MSDCF/DSC00003.JPG", jpeg("3", 1000), shot.Add(time.Hour))
	h.waitDone(5)
	if h.p.Metrics.Reconciles.Load() <= n {
		t.Error("no new listing after reconnect")
	}
}

func TestCurrentTransferIsReported(t *testing.T) {
	card := t.TempDir()
	put(t, card, "slot1/DCIM/100MSDCF/DSC00001.JPG", jpeg("1", 100_000), shot)
	cam := startCamera(t, ptpiptest.Options{Dir: card})
	h := newHarness(t, nil, "", "")
	// Immich stays down so only the download side runs; the transfer is
	// short, so look at the type directly as well.
	h.start(cam)
	defer h.stop()
	h.waitDone(1)
	if h.p.CurrentTransfer() != nil {
		t.Error("transfer still set after the download")
	}
	tr := newTransfer("x", 200)
	tr.add(50)
	if s := tr.snapshot(); s.Percent != 25 || s.Done != 50 || s.Size != 200 {
		t.Errorf("%+v", s)
	}
}

func (h *harness) waitSyncEnd() {
	h.t.Helper()
	waitFor(h.t, "sync to end", func() bool { return !h.p.syncing.Load() })
}

func (h *harness) lastResult() string {
	h.p.mu.Lock()
	defer h.p.mu.Unlock()
	return h.p.lastResult
}

func TestNoCameraAccessWithoutSync(t *testing.T) {
	cam := startCamera(t, ptpiptest.Options{Dir: sampleCard(t)})
	h := newHarness(t, nil, "", "")
	h.start(cam)
	defer h.stop()
	time.Sleep(300 * time.Millisecond)
	if n := cam.Ops.Load(); n != 0 {
		t.Errorf("%d camera operations without a sync", n)
	}
	if h.sup.Status().Connected || h.sup.Sessions() != 0 {
		t.Error("connected without a sync")
	}
	if n := h.counts()[store.StatusPending]; n != 0 {
		t.Errorf("%d files listed without a sync", n)
	}
}

func TestSyncConnectsTransfersAndReleases(t *testing.T) {
	cam := startCamera(t, ptpiptest.Options{Dir: sampleCard(t)})
	h := newHarness(t, nil, "", "")
	h.linger = 200 * time.Millisecond
	h.start(cam)
	defer h.stop()
	if !h.p.StartSync() || h.p.StartSync() {
		t.Fatal("second request while one is pending must be refused")
	}
	h.waitCounts(map[store.Status]int{store.StatusDone: 4})
	h.waitSyncEnd()
	if len(h.srv.Assets()) != 4 || h.sup.Sessions() != 1 {
		t.Errorf("assets=%d sessions=%d", len(h.srv.Assets()), h.sup.Sessions())
	}
	if got := h.lastResult(); !strings.HasPrefix(got, "4 new, 4 downloaded") {
		t.Errorf("result = %q", got)
	}
	waitFor(t, "session to close after the linger time", func() bool { return !h.sup.Status().Connected })
}

func TestCardUnavailableEndsSyncOnce(t *testing.T) {
	cam := startCamera(t, ptpiptest.Options{Dir: sampleCard(t), CardBusy: true})
	h := newHarness(t, nil, "", "")
	h.start(cam)
	defer h.stop()
	h.p.StartSync()
	waitFor(t, "result", func() bool { return h.lastResult() != "" })
	if got := h.lastResult(); got != cardUnavailableMsg {
		t.Errorf("result = %q", got)
	}
	ops := cam.Ops.Load()
	time.Sleep(300 * time.Millisecond)
	if n := cam.Ops.Load(); n != ops {
		t.Errorf("camera still being asked: %d -> %d operations", ops, n)
	}
	if c := h.counts(); c[store.StatusPending] != 0 {
		t.Errorf("counts = %v", c)
	}
}

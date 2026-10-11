package webui

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"image"
	"image/jpeg"
	"log/slog"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gen2brain/h265/heic"
	"github.com/knallli/alpha2go/ptpip/ptpiptest"
	"github.com/knallli/alpha2web/internal/backoff"
	"github.com/knallli/alpha2web/internal/camera"
	"github.com/knallli/alpha2web/internal/export"
	"github.com/knallli/alpha2web/internal/logbuf"
	"github.com/knallli/alpha2web/plugin"
)

var shotTime = time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)

func testExport(dir string) func() export.Config {
	return func() export.Config {
		return export.New(dir, "{camera}/slot{slot}/{folder}/{name}{ext}", "", "", "", "")
	}
}

// galleryFixture serves files from a fake camera; nothing connects until a request needs it.
func galleryFixture(t *testing.T, files map[string]string, opts ...func(*ptpiptest.Options)) (*Handler, *ptpiptest.Server) {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		p := filepath.Join(dir, "slot1/DCIM/100MSDCF", name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(body), 0o644)
		ptpiptest.Touch(p, shotTime)
	}
	o := ptpiptest.Options{Dir: dir, A6700: true}
	for _, f := range opts {
		f(&o)
	}
	srv, err := ptpiptest.Start(o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	sup := camera.New(camera.Config{Host: srv.Addr(), Linger: time.Hour}, backoff.Default, slog.Default())
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go sup.Run(ctx)
	return &Handler{Camera: sup, Logs: logbuf.NewRing(1), CacheDir: filepath.Join(t.TempDir(), "cache"), Export: testExport(filepath.Join(t.TempDir(), "lib"))}, srv
}

func get(h *Handler, u string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", u, nil))
	return w
}

func post(h *Handler, u string, form url.Values) *httptest.ResponseRecorder {
	if form == nil {
		form = url.Values{}
	}
	form.Set("csrf", h.csrfToken())
	r := httptest.NewRequest("POST", u, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// runJob starts a job and waits until it has finished.
func runJob(t *testing.T, h *Handler, u string, form url.Values) job {
	t.Helper()
	if w := post(h, u, form); w.Code != 202 {
		t.Fatalf("%s: %d %s", u, w.Code, w.Body)
	}
	for i := 0; i < 500; i++ {
		var j job
		json.Unmarshal(get(h, "/gallery/job").Body.Bytes(), &j)
		if j.Finished {
			return j
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("job did not finish")
	return job{}
}

var imgRe = regexp.MustCompile(`/gallery/thumb\?[^"]+`)

// thumbURLs returns the thumbnail URLs on a gallery page, in page order.
func thumbURLs(body string) []string {
	var out []string
	for _, m := range imgRe.FindAllString(body, -1) {
		out = append(out, html.UnescapeString(m))
	}
	return out
}

// itemKeys returns the item keys of a slot's first page.
func itemKeys(h *Handler, slot int) []string {
	var out []string
	for _, u := range thumbURLs(get(h, fmt.Sprintf("/gallery?slot=%d", slot)).Body.String()) {
		out = append(out, u[strings.Index(u, "k=")+2:])
	}
	return out
}

func TestGalleryNeverTouchesCamera(t *testing.T) {
	h, srv := galleryFixture(t, map[string]string{"DSC00001.JPG": "\xff\xd8x\xff\xd9"})
	w := get(h, "/gallery")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Load from camera") {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	get(h, "/gallery/thumb?slot=1&k=abc")
	if srv.Ops.Load() != 0 || h.Camera.Status().Connected {
		t.Fatalf("camera touched: ops %d", srv.Ops.Load())
	}
}

func TestGalleryRefreshThenOffline(t *testing.T) {
	jpg := "\xff\xd8hello\xff\xd9"
	h, srv := galleryFixture(t, map[string]string{"DSC00001.JPG": jpg, "DSC00001.ARW": "rawdata", "DSC00002.ARW": "rawonly"})
	j := runJob(t, h, "/gallery/refresh", nil)
	if j.Err != "" || j.Total != 2 || j.Done != 2 {
		t.Fatalf("%+v", j)
	}
	w := get(h, "/gallery")
	b := w.Body.String()
	if !strings.Contains(b, `class="badge">ARW`) || !strings.Contains(b, `class="badge">JPG`) || !strings.Contains(b, "Refresh from camera") || !strings.Contains(b, "No card in slot 2") && !strings.Contains(get(h, "/gallery?slot=2").Body.String(), "No card in slot 2") {
		t.Fatal(b)
	}
	urls := thumbURLs(b)
	if len(urls) != 2 {
		t.Fatalf("thumbs: %v", urls)
	}
	ops := srv.Ops.Load()
	for _, u := range urls {
		if w := get(h, u); w.Code != 200 || w.Header().Get("Cache-Control") != "private, max-age=86400" || w.Body.Len() == 0 {
			t.Fatalf("thumb %d %q", w.Code, w.Body)
		}
	}
	// a restarted process reads the catalog from disk
	h2 := &Handler{Camera: h.Camera, Logs: h.Logs, CacheDir: h.CacheDir}
	if n := len(thumbURLs(get(h2, "/gallery").Body.String())); n != 2 {
		t.Fatalf("catalog not persisted: %d", n)
	}
	if srv.Ops.Load() != ops {
		t.Fatalf("serving used the camera: %d -> %d", ops, srv.Ops.Load())
	}
	// the second refresh finds every thumbnail cached
	if j := runJob(t, h, "/gallery/refresh", nil); j.Total != 0 {
		t.Fatalf("%+v", j)
	}
}

func TestGalleryCardBusy(t *testing.T) {
	h, srv := galleryFixture(t, map[string]string{"DSC00001.JPG": "\xff\xd8x\xff\xd9"}, func(o *ptpiptest.Options) { o.CardBusy = true })
	j := runJob(t, h, "/gallery/refresh", nil)
	if !j.Finished || j.Err != "" {
		t.Fatalf("%+v", j)
	}
	for _, slot := range []string{"1", "2"} {
		if b := get(h, "/gallery?slot="+slot).Body.String(); !strings.Contains(b, "did not allow access to the memory card") {
			t.Fatalf("slot %s: %s", slot, b)
		}
	}
	ops := srv.Ops.Load()
	for i := 0; i < 5; i++ {
		get(h, "/gallery")
		get(h, "/gallery/thumb?slot=1&k=x")
	}
	if srv.Ops.Load() != ops {
		t.Fatal("pages caused camera operations")
	}
}

type recImporter struct {
	mu    sync.Mutex
	files []plugin.LocalFile
}

func (*recImporter) Name() string                                { return "recimporter" }
func (*recImporter) Fields() []plugin.Field                      { return nil }
func (*recImporter) Run(ctx context.Context, _ plugin.Env) error { <-ctx.Done(); return nil }
func (r *recImporter) Import(_ context.Context, f []plugin.LocalFile) error {
	r.mu.Lock()
	r.files = append(r.files, f...)
	r.mu.Unlock()
	return nil
}

func TestGalleryPull(t *testing.T) {
	imp := &recImporter{}
	plugin.Register(imp)
	jpg := "\xff\xd8hello\xff\xd9"
	h, srv := galleryFixture(t, map[string]string{"DSC00001.JPG": jpg, "DSC00001.ARW": "rawdata", "DSC00002.ARW": "rawonly"})
	runJob(t, h, "/gallery/refresh", nil)
	keys := itemKeys(h, 1)
	if len(keys) != 2 {
		t.Fatal(keys)
	}
	j := runJob(t, h, "/gallery/pull", url.Values{"slot": {"1"}, "k": keys})
	if j.Err != "" || j.Done != 2 || !strings.Contains(j.Result, "3 files copied") {
		t.Fatalf("%+v", j)
	}
	dir := filepath.Join(h.exportCfg().Dir, "ILCE-6700", "slot1", "100MSDCF")
	for name, want := range map[string]string{"DSC00001.JPG": jpg, "DSC00001.ARW": "rawdata", "DSC00002.ARW": "rawonly"} {
		if got, err := os.ReadFile(filepath.Join(dir, name)); err != nil || string(got) != want {
			t.Fatalf("%s: %q %v", name, got, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "DSC00001.JPG.partial")); err == nil {
		t.Fatal("partial file left behind")
	}
	imp.mu.Lock()
	if len(imp.files) != 3 || imp.files[0].Camera != "ILCE-6700" || imp.files[0].Slot != 1 || !strings.HasPrefix(imp.files[0].Path, "DCIM/100MSDCF/") || !filepath.IsAbs(imp.files[0].Local) && !strings.HasPrefix(imp.files[0].Local, h.exportCfg().Dir) {
		t.Fatalf("%+v", imp.files)
	}
	imp.mu.Unlock()
	if !strings.Contains(get(h, "/gallery").Body.String(), "pulled") {
		t.Fatal("not marked pulled")
	}
	// second pull: nothing is downloaded again
	n := len(srv.ContentRequests())
	j = runJob(t, h, "/gallery/pull", url.Values{"slot": {"1"}, "k": keys})
	if !strings.Contains(j.Result, "0 files copied, 3 already") || len(srv.ContentRequests()) != n {
		t.Fatalf("%+v", j)
	}
	// a pulled file is downloaded from disk
	if w := get(h, "/gallery/file?slot=1&k="+keys[0]+"&f=0"); w.Code != 200 || w.Header().Get("Content-Disposition") == "" {
		t.Fatalf("%d", w.Code)
	}
	if w := post(h, "/gallery/pull", url.Values{"slot": {"1"}}); w.Code != 400 {
		t.Fatalf("no keys: %d", w.Code)
	}
}

func TestGalleryPreviewFromCameraOnce(t *testing.T) {
	jpg := "\xff\xd8hello\xff\xd9"
	h, srv := galleryFixture(t, map[string]string{"DSC00001.JPG": jpg, "DSC00002.ARW": "rawonly"})
	runJob(t, h, "/gallery/refresh", nil)
	for _, k := range itemKeys(h, 1) {
		w := get(h, "/gallery/preview?slot=1&k="+k)
		if w.Code != 200 {
			t.Fatalf("%d %s", w.Code, w.Body)
		}
		if w.Body.String() != jpg && w.Body.String() != string(ptpiptest.CompressedData(2)) {
			t.Fatalf("unexpected preview %q", w.Body)
		}
	}
	ops := srv.Ops.Load()
	for _, k := range itemKeys(h, 1) {
		if w := get(h, "/gallery/preview?slot=1&k="+k); w.Code != 200 {
			t.Fatal(w.Code)
		}
	}
	if srv.Ops.Load() != ops {
		t.Fatalf("second preview used the camera: %d -> %d", ops, srv.Ops.Load())
	}
	if w := get(h, "/gallery/preview?slot=1&k=nope"); w.Code != 404 {
		t.Fatal(w.Code)
	}
}

func TestGalleryFileAndDelete(t *testing.T) {
	jpg := "\xff\xd8hello\xff\xd9"
	h, srv := galleryFixture(t, map[string]string{"DSC00001.JPG": jpg})
	runJob(t, h, "/gallery/refresh", nil)
	k := itemKeys(h, 1)[0]
	w := get(h, "/gallery/file?slot=1&k="+k+"&f=0")
	if w.Code != 200 || w.Body.String() != jpg || !strings.Contains(w.Header().Get("Content-Disposition"), "DSC00001.JPG") {
		t.Fatalf("%d %q %v", w.Code, w.Body, w.Header())
	}
	if w := post(h, "/gallery/delete", url.Values{"slot": {"1"}, "k": {k}}); w.Code != 303 || !strings.Contains(w.Header().Get("Location"), "done=deleted") {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if len(srv.Deletes()) != 1 || len(itemKeys(h, 1)) != 0 {
		t.Fatalf("deletes %v", srv.Deletes())
	}
}

// a pull records where files went, so changing the template later does not make them "new"
func TestGalleryPullSettings(t *testing.T) {
	h, srv := galleryFixture(t, map[string]string{"DSC00001.JPG": "\xff\xd8x\xff\xd9", "DSC00001.ARW": "raw"})
	dir := filepath.Join(t.TempDir(), "out")
	tpl := "{type}/{seq}_{name}{ext}"
	h.Export = func() export.Config { return export.New(dir, tpl, "lower", "jpeg,raw", "", "") }
	runJob(t, h, "/gallery/refresh", nil)
	k := itemKeys(h, 1)
	j := runJob(t, h, "/gallery/pull", url.Values{"slot": {"1"}, "k": k})
	if !strings.Contains(j.Result, "2 files copied") {
		t.Fatalf("%+v", j)
	}
	jp, _ := filepath.Glob(filepath.Join(dir, "jpeg", "000?_dsc00001.jpg"))
	rw, _ := filepath.Glob(filepath.Join(dir, "raw", "000?_dsc00001.arw"))
	if len(jp) != 1 || len(rw) != 1 {
		t.Fatal(jp, rw)
	}
	if st, _ := os.Stat(jp[0]); !st.ModTime().Equal(shotTime) {
		t.Fatal("mtime not set to the capture time", st.ModTime())
	}
	n := len(srv.ContentRequests())
	tpl = "other/{name}{ext}"
	if j := runJob(t, h, "/gallery/pull", url.Values{"slot": {"1"}, "k": k}); !strings.Contains(j.Result, "0 files copied, 2 already") || len(srv.ContentRequests()) != n {
		t.Fatalf("%+v", j)
	}
	if !strings.Contains(get(h, "/gallery").Body.String(), ">pulled<") {
		t.Fatal("not marked pulled")
	}
	// only JPEG selected: the RAW is reported as skipped
	h.Export = func() export.Config {
		return export.New(filepath.Join(t.TempDir(), "o2"), "{name}{ext}", "", "JPEG", "", "")
	}
	if j := runJob(t, h, "/gallery/refresh", nil); j.Err != "" { // the catalog keeps the records across a refresh
		t.Fatal(j.Err)
	}
	if !strings.Contains(get(h, "/gallery").Body.String(), ">pulled<") {
		t.Fatal("record lost on refresh")
	}
	// an unwritable folder is refused with a clear text
	blocker := filepath.Join(t.TempDir(), "file")
	os.WriteFile(blocker, nil, 0o644)
	h.Export = func() export.Config { return export.New(filepath.Join(blocker, "x"), "", "", "", "", "") }
	if w := post(h, "/gallery/pull", url.Values{"slot": {"1"}, "k": k}); w.Code != 500 || !strings.Contains(w.Body.String(), "not writable") {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
}

func TestGalleryPaging(t *testing.T) {
	files := map[string]string{}
	for i := 0; i < 50; i++ {
		files[fmt.Sprintf("DSC%05d.JPG", i)] = "\xff\xd8x\xff\xd9"
	}
	h, _ := galleryFixture(t, files)
	runJob(t, h, "/gallery/refresh", nil)
	p1 := get(h, "/gallery").Body.String()
	p2 := get(h, "/gallery?page=2").Body.String()
	if n := strings.Count(p1, `class="g-card"`); n != 48 {
		t.Fatalf("page 1 has %d cards", n)
	}
	if n := strings.Count(p2, `class="g-card"`); n != 2 {
		t.Fatalf("page 2 has %d cards", n)
	}
	if !strings.Contains(p1, "page=2") || strings.Contains(p2, "Older") || !strings.Contains(p1, "Select all in slot (50)") {
		t.Fatal("pager links wrong")
	}
}

func heicFixture(t *testing.T) string {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 64, 48))
	for i := range img.Pix {
		img.Pix[i] = byte(i)
	}
	var buf bytes.Buffer
	if err := heic.Encode(&buf, img, heic.EncodeOptions{Quality: 80}); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

// The fake camera cannot serve custom thumbnails, so a .JPG that really holds
// HEIF bytes exercises the same fetch, convert and cache path via the file download.
func TestGalleryHEICConverted(t *testing.T) {
	h, srv := galleryFixture(t, map[string]string{"DSC00001.JPG": heicFixture(t)})
	runJob(t, h, "/gallery/refresh", nil)
	u := "/gallery/preview?slot=1&k=" + itemKeys(h, 1)[0]
	w := get(h, u)
	if w.Code != 200 || w.Header().Get("Content-Type") != "image/jpeg" {
		t.Fatalf("%d %v", w.Code, w.Header())
	}
	cfg, err := jpeg.DecodeConfig(w.Body)
	if err != nil || cfg.Width != 64 || cfg.Height != 48 {
		t.Fatalf("%v %+v", err, cfg)
	}
	ops := srv.Ops.Load()
	if w2 := get(h, u); w2.Code != 200 || w2.Header().Get("Content-Type") != "image/jpeg" || srv.Ops.Load() != ops {
		t.Fatalf("second request not served from cache: %d ops %d -> %d", w2.Code, ops, srv.Ops.Load())
	}
}

func TestGalleryHEICFallback(t *testing.T) {
	bad := "\x00\x00\x00\x18ftypheic" + strings.Repeat("x", 40)
	h, _ := galleryFixture(t, map[string]string{"DSC00001.JPG": bad})
	runJob(t, h, "/gallery/refresh", nil)
	w := get(h, "/gallery/preview?slot=1&k="+itemKeys(h, 1)[0])
	if w.Code != 200 || w.Body.String() != bad || w.Header().Get("Content-Type") != "image/heic" {
		t.Fatalf("%d %v %q", w.Code, w.Header(), w.Body.String())
	}
}

func TestStatForNeedsCaptureTime(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.ARW")
	os.WriteFile(p, make([]byte, 5), 0o644)
	shot := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	os.Chtimes(p, shot, shot)
	if n, ok := statFor(nil, shot)(p); !ok || n != 5 {
		t.Fatal("same capture time is this shot", n, ok)
	}
	if n, _ := statFor(nil, shot.Add(time.Hour))(p); n == 5 {
		t.Fatal("same size from another shot must not match")
	}
	if n, _ := statFor(map[string]bool{p: true}, shot)(p); n != export.Taken {
		t.Fatal("a path owned by the catalog is taken", n)
	}
}

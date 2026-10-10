package webui

import (
	"bytes"
	"context"
	"fmt"
	"html"
	"image"
	"image/jpeg"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/gen2brain/h265/heic"
	"github.com/knallli/alpha2go/ptpip/ptpiptest"
	"github.com/knallli/alpha2web/internal/backoff"
	"github.com/knallli/alpha2web/internal/camera"
	"github.com/knallli/alpha2web/internal/logbuf"
)

func galleryFixture(t *testing.T, files map[string]string) (*Handler, *ptpiptest.Server) {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		p := filepath.Join(dir, "slot1/DCIM/100MSDCF", name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(body), 0o644)
	}
	srv, err := ptpiptest.Start(ptpiptest.Options{Dir: dir, A6700: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	sup := camera.New(camera.Config{Host: srv.Addr()}, backoff.Default, slog.Default())
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go sup.Run(ctx)
	for i := 0; !sup.Status().Connected; i++ {
		if i > 200 {
			t.Fatal("not connected")
		}
		time.Sleep(20 * time.Millisecond)
	}
	return &Handler{Camera: sup, Logs: logbuf.NewRing(1), CacheDir: filepath.Join(t.TempDir(), "cache")}, srv
}

func get(h *Handler, u string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", u, nil))
	return w
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

func TestGalleryThumbAndPreview(t *testing.T) {
	jpg := "\xff\xd8hello\xff\xd9"
	h, srv := galleryFixture(t, map[string]string{"DSC00001.JPG": jpg, "DSC00001.ARW": "rawdata", "DSC00002.ARW": "rawonly"})
	w := get(h, "/gallery")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `class="badge">ARW`) || !strings.Contains(w.Body.String(), `class="badge">JPG`) {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	urls := thumbURLs(w.Body.String())
	if len(urls) != 2 {
		t.Fatalf("thumbs: %v", urls)
	}
	var jpgThumb, rawThumb string
	for _, u := range urls {
		// the JPG content's cover file is the .JPG (file id 1 or 2 depending on walk order)
		if strings.Contains(u, "file=") {
			if jpgThumb == "" {
				jpgThumb = u
			} else {
				rawThumb = u
			}
		}
	}

	w = get(h, jpgThumb)
	if w.Code != 200 || w.Header().Get("Cache-Control") != "private, max-age=86400" || w.Body.Len() == 0 {
		t.Fatalf("thumb %d %q", w.Code, w.Body.String())
	}
	first := w.Body.String()
	ops := srv.Ops.Load()
	w = get(h, jpgThumb)
	if w.Body.String() != first || srv.Ops.Load() != ops {
		t.Fatalf("second thumb not served from cache: ops %d -> %d", ops, srv.Ops.Load())
	}

	// preview: whichever content has the JPG must return the file bytes
	var sawJPG, sawRaw bool
	for _, u := range []string{jpgThumb, rawThumb} {
		p := strings.Replace(u, "/gallery/thumb", "/gallery/preview", 1)
		w = get(h, p)
		if w.Code != 200 {
			t.Fatalf("preview %s: %d", p, w.Code)
		}
		switch w.Body.String() {
		case jpg:
			sawJPG = true
			if w.Header().Get("Content-Type") != "image/jpeg" {
				t.Fatal(w.Header())
			}
		case string(ptpiptest.CompressedData(2)):
			sawRaw = true
		default:
			t.Fatalf("unexpected preview %q", w.Body.String())
		}
	}
	if !sawJPG || !sawRaw {
		t.Fatalf("jpg=%v raw=%v", sawJPG, sawRaw)
	}
	if n := len(srv.ContentRequests()); n == 0 {
		t.Fatal("expected a 0x923D download for the JPG preview")
	}

	for _, u := range []string{"/gallery/thumb?slot=1&id=999999&file=1", "/gallery/preview?slot=1&id=999999&file=1"} {
		if w := get(h, u); w.Code != 404 {
			t.Fatalf("%s: %d", u, w.Code)
		}
	}
}

func TestGalleryPaging(t *testing.T) {
	files := map[string]string{}
	for i := 0; i < 50; i++ {
		files[fmt.Sprintf("DSC%05d.JPG", i)] = "\xff\xd8x\xff\xd9"
	}
	h, _ := galleryFixture(t, files)
	p1 := get(h, "/gallery").Body.String()
	p2 := get(h, "/gallery?page=2").Body.String()
	if n := strings.Count(p1, `class="g-card"`); n != 48 {
		t.Fatalf("page 1 has %d cards", n)
	}
	if n := strings.Count(p2, `class="g-card"`); n != 2 {
		t.Fatalf("page 2 has %d cards", n)
	}
	if !strings.Contains(p1, "page=2") || strings.Contains(p2, "Older") {
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
	u := strings.Replace(thumbURLs(get(h, "/gallery").Body.String())[0], "/gallery/thumb", "/gallery/preview", 1)
	w := get(h, u)
	if w.Code != 200 || w.Header().Get("Content-Type") != "image/jpeg" {
		t.Fatalf("%d %v", w.Code, w.Header())
	}
	cfg, err := jpeg.DecodeConfig(w.Body)
	if err != nil || cfg.Width != 64 || cfg.Height != 48 {
		t.Fatalf("%v %+v", err, cfg)
	}
	first, ops := w.Body.Len(), srv.Ops.Load()
	if w2 := get(h, u); w2.Code != 200 || w2.Header().Get("Content-Type") != "image/jpeg" || srv.Ops.Load() != ops {
		t.Fatalf("second request not served from cache: %d ops %d -> %d (%d)", w2.Code, ops, srv.Ops.Load(), first)
	}
}

func TestGalleryHEICFallback(t *testing.T) {
	bad := "\x00\x00\x00\x18ftypheic" + strings.Repeat("x", 40)
	h, _ := galleryFixture(t, map[string]string{"DSC00001.JPG": bad})
	u := strings.Replace(thumbURLs(get(h, "/gallery").Body.String())[0], "/gallery/thumb", "/gallery/preview", 1)
	w := get(h, u)
	if w.Code != 200 || w.Body.String() != bad || w.Header().Get("Content-Type") != "image/heic" {
		t.Fatalf("%d %v %q", w.Code, w.Header(), w.Body.String())
	}
}

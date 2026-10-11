package webui

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/knallli/alpha2go/ptpip"
	"github.com/knallli/alpha2go/ptpip/ptpiptest"
	"github.com/knallli/alpha2web/internal/backoff"
	"github.com/knallli/alpha2web/internal/camera"
	"github.com/knallli/alpha2web/internal/logbuf"
)

func TestStreamMJPEG(t *testing.T) {
	frames := [][]byte{{0xFF, 0xD8, 1}, {0xFF, 0xD8, 2}}
	w := httptest.NewRecorder()
	_, _ = streamMJPEG(w, httptest.NewRequest("GET", "/", nil), func() ([]byte, error) {
		if len(frames) == 0 {
			return nil, io.EOF
		}
		f := frames[0]
		frames = frames[1:]
		return f, nil
	})
	if n := strings.Count(w.Body.String(), "--frame"); n != 2 {
		t.Fatalf("frames: %d", n)
	}
	if !strings.HasPrefix(w.Header().Get("Content-Type"), "multipart/x-mixed-replace") {
		t.Fatal("content type")
	}
}

// camFrame builds a frame in the camera's layout: header, metadata, JPEG.
func camFrame(jpeg, meta []byte) []byte {
	le := binary.LittleEndian
	b := le.AppendUint32(nil, uint32(16+len(meta)))
	b = le.AppendUint32(b, uint32(len(jpeg)))
	b = le.AppendUint32(b, 16)
	b = le.AppendUint32(b, uint32(len(meta)))
	return append(append(b, meta...), jpeg...)
}

// liveFixture is a fake camera (PTP/IP + live-view HTTP) behind a web UI server.
type liveFixture struct {
	srv   *ptpiptest.Server
	cam   *httptest.Server
	opens atomic.Int64
	sup   *camera.Supervisor
	web   *httptest.Server
}

func newLiveFixture(t *testing.T) *liveFixture {
	t.Helper()
	f := &liveFixture{}
	f.cam = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.opens.Add(1)
		for i := 0; r.Context().Err() == nil; i++ {
			w.Write(camFrame([]byte{0xFF, 0xD8, byte(i), 0xFF, 0xD9}, make([]byte, 136)))
			w.(http.Flusher).Flush()
			time.Sleep(10 * time.Millisecond)
		}
	}))
	t.Cleanup(f.cam.Close)
	_, port, _ := net.SplitHostPort(f.cam.Listener.Addr().String())
	var err error
	f.srv, err = ptpiptest.Start(ptpiptest.Options{Dir: t.TempDir(), A6700: true, Props: []ptpip.SonyProp{
		{Code: ptpip.SonyPropLiveViewURL, Type: 0xFFFF, Enabled: 1, Current: ptpip.SonyValue{Str: "http://localhost:" + port + "/liveviewstream?x=1", NoNUL: true}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.srv.Close)
	f.sup = camera.New(camera.Config{Host: f.srv.Addr(), Linger: time.Hour}, backoff.Policy{Base: 50 * time.Millisecond, Max: 100 * time.Millisecond}, slog.Default())
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go f.sup.Run(ctx)
	f.sup.Connect()
	for i := 0; !f.sup.Status().Connected; i++ {
		if i > 200 {
			t.Fatal("not connected")
		}
		time.Sleep(20 * time.Millisecond)
	}
	f.web = httptest.NewServer(&Handler{Camera: f.sup, Logs: logbuf.NewRing(1)})
	t.Cleanup(f.web.Close)
	return f
}

func parts(t *testing.T, resp *http.Response) *multipart.Reader {
	t.Helper()
	mt, params, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || mt != "multipart/x-mixed-replace" {
		t.Fatalf("content type %q", resp.Header.Get("Content-Type"))
	}
	return multipart.NewReader(resp.Body, params["boundary"])
}

func readFrames(t *testing.T, mr *multipart.Reader, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		p, err := mr.NextPart()
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		b, err := io.ReadAll(p)
		if err != nil || len(b) != 5 || b[0] != 0xFF || b[1] != 0xD8 || p.Header.Get("Content-Type") != "image/jpeg" {
			t.Fatalf("frame %d: % x %v", i, b, err)
		}
	}
}

func TestLiveViewEndToEnd(t *testing.T) {
	f := newLiveFixture(t)
	resp, err := http.Get(f.web.URL + "/liveview.mjpeg")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	mr := parts(t, resp)
	readFrames(t, mr, 3)

	r2, err := http.Get(f.web.URL + "/liveview.mjpeg")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(r2.Body)
	r2.Body.Close()
	if r2.StatusCode != http.StatusConflict || !strings.Contains(string(b), "live view already open in another tab") {
		t.Fatalf("second stream: %d %q", r2.StatusCode, b)
	}

	rs, err := http.Get(f.web.URL + "/liveview/status")
	if err != nil {
		t.Fatal(err)
	}
	var st struct {
		Connected bool
		Frames    uint64
		Drops     int
	}
	json.NewDecoder(rs.Body).Decode(&st)
	rs.Body.Close()
	if !st.Connected || st.Frames < 3 {
		t.Fatalf("status %+v", st)
	}

	// Camera side loses the session and the stream; the response must go on.
	f.srv.DropConnections()
	f.cam.CloseClientConnections()
	readFrames(t, mr, 10)
	if f.opens.Load() < 2 || f.sup.Sessions() < 2 || f.sup.Status().Drops < 1 {
		t.Fatalf("opens %d sessions %d drops %d", f.opens.Load(), f.sup.Sessions(), f.sup.Status().Drops)
	}
}

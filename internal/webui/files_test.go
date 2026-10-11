package webui

import (
	"context"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/knallli/alpha2go/ptpip/ptpiptest"
	"github.com/knallli/alpha2web/internal/backoff"
	"github.com/knallli/alpha2web/internal/camera"
	"github.com/knallli/alpha2web/internal/logbuf"
)

func TestFiles(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "slot1/DCIM/100MSDCF/DSC00001.JPG")
	os.MkdirAll(filepath.Dir(p), 0o755)
	os.WriteFile(p, []byte("\xff\xd8hello\xff\xd9"), 0o644)
	srv, err := ptpiptest.Start(ptpiptest.Options{Dir: dir, A6700: true})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	sup := camera.New(camera.Config{Host: srv.Addr(), Linger: time.Hour}, backoff.Default, slog.Default())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sup.Run(ctx)
	sup.Connect()
	for i := 0; !sup.Status().Connected; i++ {
		if i > 200 {
			t.Fatal("not connected")
		}
		time.Sleep(20 * time.Millisecond)
	}
	h := &Handler{Camera: sup, Logs: logbuf.NewRing(1)}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/files", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "No plugin tracks files.") || strings.Contains(w.Body.String(), "/files/thumb") {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
}

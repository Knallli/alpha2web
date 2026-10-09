package webui

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/knallli/alpha2go/ptpip/ptpiptest"
	"github.com/knallli/alpha2web/internal/backoff"
	"github.com/knallli/alpha2web/internal/camera"
	"github.com/knallli/alpha2web/internal/logbuf"
)

func TestControl(t *testing.T) {
	srv, err := ptpiptest.Start(ptpiptest.Options{Dir: t.TempDir(), A6700: true})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	sup := camera.New(camera.Config{Host: srv.Addr()}, backoff.Default, slog.Default())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sup.Run(ctx)
	for i := 0; !sup.Status().Connected; i++ {
		if i > 200 {
			t.Fatal("not connected")
		}
		time.Sleep(20 * time.Millisecond)
	}
	h := &Handler{Camera: sup, Logs: logbuf.NewRing(1)}

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/control", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Properties") || strings.Contains(w.Body.String(), "msg bad") {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}

	post := func(path string, v url.Values, token string) int {
		if v == nil {
			v = url.Values{}
		}
		v.Set("csrf", token)
		r := httptest.NewRequest("POST", path, strings.NewReader(v.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	if c := post("/actions/capture", url.Values{"af": {"1"}}, h.csrfToken()); c != http.StatusSeeOther {
		t.Fatal("capture", c)
	}
	if len(srv.Controls()) == 0 {
		t.Fatal("camera saw no control calls")
	}
	if c := post("/actions/capture", nil, "bad"); c != http.StatusBadRequest {
		t.Fatal("bad token", c)
	}
	if c := post("/actions/setprop", url.Values{"code": {"1"}, "value": {"1"}}, h.csrfToken()); c != http.StatusBadGateway {
		t.Fatal("unknown prop", c)
	}
}

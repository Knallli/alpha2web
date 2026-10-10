package webui

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/knallli/alpha2go/ptpip"
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
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Settings") || strings.Contains(w.Body.String(), "msg bad") {
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
	if c := post("/actions/setprop", url.Values{"code": {"1"}, "value": {"1"}}, h.csrfToken()); c != http.StatusNotFound {
		t.Fatal("unknown prop", c)
	}
}

func TestControlBannerWithoutCamera(t *testing.T) {
	h := &Handler{Camera: camera.New(camera.Config{}, backoff.Default, slog.Default()), Logs: logbuf.NewRing(1)}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/control", nil))
	b := w.Body.String()
	if w.Code != 200 || !strings.Contains(b, `msg wait`) || !strings.Contains(b, "The camera is not connected.") || strings.Contains(b, "msg bad") {
		t.Fatalf("%d %s", w.Code, b)
	}
}

func enumProp(code, typ uint16, cur int64, set ...int64) ptpip.SonyProp {
	p := ptpip.SonyProp{Code: code, Type: typ, Writable: true, Enabled: 1, Current: ptpip.SonyValue{Int: cur}, Form: 2}
	for _, v := range set {
		p.Settable = append(p.Settable, ptpip.SonyValue{Int: v})
	}
	return p
}

func TestControlProps(t *testing.T) {
	disabled := enumProp(0x5007, 4, 560, 560, 800)
	disabled.Enabled = 0
	slider := ptpip.SonyProp{Code: 0xD20F, Type: 4, Writable: true, Enabled: 1, Current: ptpip.SonyValue{Int: 2500}, Form: 1,
		Min: ptpip.SonyValue{Int: 2500}, Max: ptpip.SonyValue{Int: 9900}, Step: ptpip.SonyValue{Int: 100}}
	srv, err := ptpiptest.Start(ptpiptest.Options{Dir: t.TempDir(), A6700: true, Props: []ptpip.SonyProp{
		enumProp(0x5005, 4, 2, 2, 4, 5), disabled, slider,
		enumProp(0xD222, 4, 0x11, 1, 0x10, 0x11),
		enumProp(0xD207, 2, 0, 0, 1),
		{Code: 0xD278, Type: 0xFFFF, Writable: true, Enabled: 1, Current: ptpip.SonyValue{Str: "http://localhost/x"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	sup := camera.New(camera.Config{Host: srv.Addr()}, backoff.Default, slog.Default())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sup.Run(ctx)
	for i := 0; ; i++ {
		if ps, _ := sup.Props(); len(ps) == 6 {
			break
		}
		if i > 200 {
			t.Fatal("no props")
		}
		time.Sleep(20 * time.Millisecond)
	}
	h := &Handler{Camera: sup, Logs: logbuf.NewRing(1)}

	get := func(u string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", u, nil))
		return w
	}
	w := get("/control/props")
	if w.Code != 200 || w.Header().Get("Content-Type") != "application/json" || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("%d %v", w.Code, w.Header())
	}
	var list struct {
		Version   uint64
		Connected bool
		Props     []propView
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || !list.Connected {
		t.Fatal(err, w.Body.String())
	}
	by := map[uint16]propView{}
	for _, p := range list.Props {
		by[p.Code] = p
	}
	for code, want := range map[uint16]string{0x5005: "select", 0x5007: "readonly", 0xD20F: "slider", 0xD222: "select", 0xD207: "switch", 0xD278: "readonly"} {
		if by[code].Widget != want {
			t.Errorf("0x%04X widget %q, want %q", code, by[code].Widget, want)
		}
	}
	if p := by[0x5005]; p.Name != "White balance" || p.Desc == "" || p.Group != "White balance" || p.Options[1].Label != "Daylight" || p.Display != "Automatic" {
		t.Errorf("%+v", p)
	}
	if p := by[0x5007]; p.Disabled == "" || p.Display != "5.6" {
		t.Errorf("%+v", p)
	}
	if p := by[0xD20F]; p.Documented || p.Group != "Undocumented" || p.Step != 100 || p.Max != 9900 {
		t.Errorf("%+v", p)
	}
	if p := by[0xD222]; p.Danger != "danger" || p.Note == "" {
		t.Errorf("%+v", p)
	}
	if c := get("/control/props?since=" + strconv.FormatUint(list.Version, 10)).Code; c != http.StatusNoContent {
		t.Errorf("unchanged since: %d", c)
	}
	if c := get("/control/props?since=0").Code; c != 200 {
		t.Errorf("old since: %d", c)
	}
	if c := get("/control").Code; c != 200 {
		t.Errorf("page: %d", c)
	}

	set := func(v url.Values) (int, struct {
		OK    bool
		Error string
		Prop  propView
	}) {
		v.Set("csrf", h.csrfToken())
		r := httptest.NewRequest("POST", "/actions/setprop", strings.NewReader(v.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Accept", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		var out struct {
			OK    bool
			Error string
			Prop  propView
		}
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err, w.Body.String())
		}
		return w.Code, out
	}
	for _, c := range []struct {
		name string
		v    url.Values
		want int
	}{
		{"disabled", url.Values{"code": {"20487"}, "value": {"800"}}, 422},
		{"not offered", url.Values{"code": {"20485"}, "value": {"9"}}, 422},
		{"string", url.Values{"code": {"53880"}, "value": {"1"}}, 422},
		{"undocumented", url.Values{"code": {"53775"}, "value": {"5000"}}, 403},
		{"out of range", url.Values{"code": {"53775"}, "value": {"20000"}, "unlock": {"1"}}, 422},
		{"off step", url.Values{"code": {"53775"}, "value": {"5050"}, "unlock": {"1"}}, 422},
		{"danger", url.Values{"code": {"53794"}, "value": {"1"}}, 409},
		{"unknown", url.Values{"code": {"1"}, "value": {"1"}}, 404},
	} {
		if code, out := set(c.v); code != c.want || out.OK || out.Error == "" {
			t.Errorf("%s: %d %+v", c.name, code, out)
		}
	}
	if len(srv.Controls()) != 0 {
		t.Error("nothing should have reached the camera")
	}
	for _, c := range []url.Values{
		{"code": {"20485"}, "value": {"4"}},
		{"code": {"53775"}, "value": {"5000"}, "unlock": {"1"}},
		{"code": {"53794"}, "value": {"1"}, "confirm": {"1"}},
	} {
		code, out := set(c)
		if v, _ := strconv.ParseInt(c.Get("value"), 10, 64); code != 200 || !out.OK || out.Prop.Current != v {
			t.Errorf("%v: %d %+v", c, code, out)
		}
	}
}

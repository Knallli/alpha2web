package immich

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/knallli/alpha2go/ptpip/ptpiptest"
	"github.com/knallli/alpha2web/internal/backoff"
	"github.com/knallli/alpha2web/internal/camera"
	"github.com/knallli/alpha2web/plugin"
	"github.com/knallli/alpha2web/plugins/immich/immichtest"
)

// startPlugin runs the plugin against a fake camera holding one JPEG.
func startPlugin(t *testing.T, cfg map[string]string) *Plugin {
	t.Helper()
	card := t.TempDir()
	f := filepath.Join(card, "slot1/DCIM/100MSDCF/DSC00001.JPG")
	os.MkdirAll(filepath.Dir(f), 0o755)
	os.WriteFile(f, []byte("\xff\xd8photo\xff\xd9"), 0o644)
	cam, err := ptpiptest.Start(ptpiptest.Options{Dir: card, A6700: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cam.Close)
	sup := camera.New(camera.Config{Host: cam.Addr()}, backoff.Default, slog.Default())
	ctx, cancel := context.WithCancel(context.Background())
	go sup.Run(ctx)
	t.Setenv("STAGING_DIR", t.TempDir())
	data := t.TempDir()
	p := &Plugin{}
	done := make(chan struct{})
	go func() {
		p.Run(ctx, plugin.Env{Camera: sup, Log: slog.Default(), DataDir: data, Config: func() map[string]string { return cfg }})
		close(done)
	}()
	t.Cleanup(func() { cancel(); <-done })
	waitFor(t, "pipeline", func() bool { return p.pipe.Load() != nil })
	return p
}

func TestUIAfterUpload(t *testing.T) {
	im := immichtest.New(t)
	p := startPlugin(t, map[string]string{"url": im.URL, "api_key": im.APIKey})
	ctx := context.Background()
	counts := func() map[string]int64 {
		m := map[string]int64{}
		for _, c := range p.Status(ctx).Counts {
			m[c.Label] = c.N
		}
		return m
	}
	waitFor(t, "done", func() bool { return counts()["Done"] == 1 })
	st := p.Status(ctx)
	if st.Title != "Immich" || st.Level != "ok" || st.Headline != "Connected" || st.Notice != "" || len(st.Counts) != 7 || !st.Counts[6].Bad {
		t.Fatalf("%+v", st)
	}
	if st.Rows[1].Link != im.URL || len(st.Recent) != 1 || !strings.Contains(st.Recent[0].Link, "/photos/") {
		t.Fatalf("%+v", st)
	}
	if st.Totals[0].Value != "1 · 9 B" || st.Totals[len(st.Totals)-1].Value == "never" {
		t.Fatalf("%+v", st.Totals)
	}
	if fs := p.Files(ctx, "done", 10); len(fs) != 1 || fs[0].Attempts != "0/0" {
		t.Fatalf("%+v", fs)
	}
	if fs := p.Files(ctx, "pending", 10); len(fs) != 0 {
		t.Fatalf("%+v", fs)
	}
	if got := p.FileStatuses(); len(got) != 7 || got[0] != "pending" {
		t.Fatal(got)
	}

	if msg, err := p.Do(ctx, "retry"); err != nil || msg != "Waiting files will be retried now." {
		t.Fatal(msg, err)
	}
	before := p.pipe.Load().Metrics.Reconciles.Load()
	if msg, err := p.Do(ctx, "rescan"); err != nil || msg != "Reading the camera cards again." {
		t.Fatal(msg, err)
	}
	waitFor(t, "rescan", func() bool { return p.pipe.Load().Metrics.Reconciles.Load() > before })
	if _, err := p.Do(ctx, "nope"); err == nil {
		t.Fatal("unknown action accepted")
	}

	var b strings.Builder
	p.WriteMetrics(&b)
	for _, want := range []string{"alpha2web_immich_uploads_total 1\n", `alpha2web_immich_files{status="done"} 1`, "alpha2web_immich_reachable 1\n"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("metrics lack %q:\n%s", want, b.String())
		}
	}
	h := p.Health()
	if h["immich_reachable"] != true || h["immich_version"] == "" || h["queue"].(map[string]int)["done"] != 1 {
		t.Fatalf("%v", h)
	}
}

func TestUINotConfigured(t *testing.T) {
	p := startPlugin(t, nil)
	waitFor(t, "downloaded", func() bool { return len(p.Files(context.Background(), "downloaded", 10)) == 1 })
	st := p.Status(context.Background())
	if st.Level != "wait" || st.Headline != "Not set up" || st.NoticeLink != "/settings" || !strings.Contains(st.Notice, "not connected yet") {
		t.Fatalf("%+v", st)
	}
	if (&Plugin{}).Status(context.Background()).Headline == "" || (&Plugin{}).Files(context.Background(), "", 5) != nil {
		t.Fatal("not running must be harmless")
	}
}

func TestUIBadKey(t *testing.T) {
	im := immichtest.New(t)
	p := startPlugin(t, map[string]string{"url": im.URL, "api_key": "wrong", "album": "A"})
	waitFor(t, "bad", func() bool { return p.Status(context.Background()).Level == "bad" })
	st := p.Status(context.Background())
	if st.Headline != "Not reachable" {
		t.Fatalf("%+v", st)
	}
}

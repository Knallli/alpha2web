package immich

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/knallli/alpha2go/ptpip/ptpiptest"
	"github.com/knallli/alpha2web/internal/backoff"
	"github.com/knallli/alpha2web/internal/camera"
	"github.com/knallli/alpha2web/plugin"
	"github.com/knallli/alpha2web/plugins/immich/immichtest"
)

func TestPluginUploads(t *testing.T) {
	card := t.TempDir()
	p := filepath.Join(card, "slot1/DCIM/100MSDCF/DSC00001.JPG")
	os.MkdirAll(filepath.Dir(p), 0o755)
	os.WriteFile(p, []byte("\xff\xd8photo\xff\xd9"), 0o644)
	cam, err := ptpiptest.Start(ptpiptest.Options{Dir: card, A6700: true})
	if err != nil {
		t.Fatal(err)
	}
	defer cam.Close()
	im := immichtest.New(t)

	sup := camera.New(camera.Config{Host: cam.Addr(), Linger: time.Hour}, backoff.Default, slog.Default())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sup.Run(ctx)
	data := t.TempDir()
	staging := t.TempDir()
	t.Setenv("STAGING_DIR", staging)
	cfg := map[string]string{"url": im.URL, "api_key": im.APIKey, "album": "Cam", "delete_on_camera": "true"}
	done := make(chan struct{})
	pl := &Plugin{}
	go func() {
		pl.Run(ctx, plugin.Env{Camera: sup, Log: slog.Default(), DataDir: data, Config: func() map[string]string { return cfg }})
		close(done)
	}()
	waitFor(t, "pipeline", func() bool { return pl.pipe.Load() != nil })
	syncNow(t, pl)
	waitFor(t, "upload", func() bool { return len(im.Assets()) > 0 })
	if a := im.Assets()[0]; a.Filename != "DSC00001.JPG" {
		t.Fatalf("%+v", a)
	}
	waitFor(t, "album", func() bool { return len(im.AlbumAssets("Cam")) == 1 })
	// the camera delete step of a sync handles files finished before it
	waitFor(t, "camera delete", func() bool { pl.pipe.Load().StartSync(); return len(cam.Deletes()) == 1 })
	cancel()
	<-done
	if _, err := os.Stat(filepath.Join(data, "immich.db")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(staging, ".partial")); err != nil {
		t.Fatal("STAGING_DIR not used:", err)
	}
}

func TestSettings(t *testing.T) {
	s, err := parseSettings(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Stack || !s.DeleteLocal || s.DeleteOnCamera || s.Concurrency != 2 || s.MinFreeBytes != 1<<30 ||
		s.Stall != 3*time.Minute || s.Retry.Base != 5*time.Second || s.Retry.Max != 15*time.Minute ||
		strings.Join(s.Skip, ",") != ".XML,.THM" {
		t.Fatalf("defaults: %+v", s)
	}
	s, err = parseSettings(map[string]string{"skip_extensions": "none", "delete_on_camera": "true", "upload_concurrency": "4"})
	if err != nil || len(s.Skip) != 0 || !s.DeleteOnCamera || s.Concurrency != 4 {
		t.Fatalf("%+v %v", s, err)
	}
	s, err = parseSettings(map[string]string{"skip_extensions": "xml, .thm", "stall_timeout": "soon"})
	if err == nil || s.Stall != 3*time.Minute || strings.Join(s.Skip, ",") != ".XML,.THM" {
		t.Fatalf("%+v %v", s, err)
	}
}

func TestValidateAndFields(t *testing.T) {
	p := &Plugin{}
	for name, cfg := range map[string]map[string]string{
		"bad bool":     {"delete_on_camera": "maybe"},
		"zero stall":   {"stall_timeout": "0s"},
		"key only":     {"api_key": "k"},
		"not a url":    {"url": "immich", "api_key": "k"},
		"concurrency":  {"upload_concurrency": "0"},
		"negative MB":  {"min_free_space_mb": "-1"},
		"bad duration": {"retry_base_delay": "5"},
	} {
		if p.Validate(cfg) == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if err := p.Validate(map[string]string{"url": "http://immich:2283", "api_key": "k", "album": "x"}); err != nil {
		t.Error(err)
	}
	if err := p.Validate(nil); err != nil {
		t.Error("unconfigured must be valid:", err)
	}
	var _ plugin.ConfigValidator = p
	kinds := map[string]plugin.FieldKind{}
	for _, f := range p.Fields() {
		kinds[f.Key] = f.Kind
	}
	if len(kinds) != 12 || kinds["api_key"] != plugin.FieldSecret || kinds["delete_on_camera"] != plugin.FieldBool ||
		kinds["stall_timeout"] != plugin.FieldDuration || kinds["upload_concurrency"] != plugin.FieldInt {
		t.Errorf("fields: %v", kinds)
	}
}

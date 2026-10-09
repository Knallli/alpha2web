package immich

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
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

	sup := camera.New(camera.Config{Host: cam.Addr()}, backoff.Default, slog.Default())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sup.Run(ctx)
	data := t.TempDir()
	cfg := map[string]string{"url": im.URL, "api_key": im.APIKey, "album": "Cam", "interval": "10s", "delete_after_upload": "true"}
	done := make(chan struct{})
	go func() {
		(&Plugin{}).Run(ctx, plugin.Env{Camera: sup, Log: slog.Default(), DataDir: data, Config: func() map[string]string { return cfg }})
		close(done)
	}()
	for i := 0; len(im.Assets()) == 0; i++ {
		if i > 300 {
			t.Fatal("nothing uploaded")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if a := im.Assets()[0]; a.Filename != "DSC00001.JPG" {
		t.Fatalf("%+v", a)
	}
	if len(im.AlbumAssets("Cam")) != 1 {
		t.Fatal("not in album")
	}
	for i := 0; len(cam.Deletes()) == 0; i++ {
		if i > 100 {
			t.Fatal("not deleted from card")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
	if _, err := os.Stat(filepath.Join(data, "immich-uploaded.json")); err != nil {
		t.Fatal(err)
	}
}

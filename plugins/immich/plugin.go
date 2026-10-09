package immich

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/knallli/alpha2go/ptpip"
	"github.com/knallli/alpha2web/plugin"
)

func init() { plugin.Register(&Plugin{}) }

// Plugin uploads everything on the camera's cards to Immich.
type Plugin struct{}

func (*Plugin) Name() string { return "immich" }

func (*Plugin) Fields() []plugin.Field {
	return []plugin.Field{
		{Key: "url", Label: "Immich address", Help: "e.g. http://immich:2283"},
		{Key: "api_key", Label: "API key", Secret: true, Help: "needs asset.upload (plus album.read, album.create, albumAsset.create for an album)"},
		{Key: "album", Label: "Album (optional)"},
		{Key: "delete_after_upload", Label: "Delete from the card after upload (true/false)"},
		{Key: "interval", Label: "Check interval (default 2m)"},
	}
}

// Run polls the cards: the camera announces no new shots to the host.
func (p *Plugin) Run(ctx context.Context, env plugin.Env) error {
	events, stop := env.Camera.Subscribe()
	defer stop()
	s := &syncer{env: env, state: filepath.Join(env.DataDir, "immich-uploaded.json")}
	if err := s.load(); err != nil {
		return err
	}
	for {
		cfg := env.Config()
		iv, err := time.ParseDuration(cfg["interval"])
		if err != nil || iv < 10*time.Second {
			iv = 2 * time.Minute
		}
		if env.Camera.Status().Connected && cfg["url"] != "" && cfg["api_key"] != "" {
			if err := s.sync(ctx, cfg); err != nil && ctx.Err() == nil {
				env.Log.Warn("sync failed", "err", err)
			}
		}
		t := time.NewTimer(iv)
	wait:
		for {
			select {
			case <-ctx.Done():
				t.Stop()
				return nil
			case <-t.C:
				break wait
			case e := <-events:
				if e.Connected != nil && *e.Connected {
					t.Stop()
					break wait
				}
			}
		}
	}
}

type syncer struct {
	env   plugin.Env
	state string
	mu    sync.Mutex
	done  map[string]string // file key -> Immich asset id
}

func (s *syncer) load() error {
	s.done = map[string]string{}
	b, err := os.ReadFile(s.state)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(b, &s.done)
}

func (s *syncer) save() error {
	b, _ := json.Marshal(s.done)
	if err := os.MkdirAll(filepath.Dir(s.state), 0o755); err != nil {
		return err
	}
	tmp := s.state + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.state)
}

// key identifies a file across reconnects (content IDs are not stable).
func key(slot uint32, c ptpip.SonyContent, f ptpip.SonyContentFile) string {
	return fmt.Sprintf("%d|%s|%d|%d", slot, f.Path, f.Size, c.CreatedUTC.UnixMilli())
}

func (s *syncer) sync(ctx context.Context, cfg map[string]string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cam, err := s.env.Camera.Client()
	if err != nil {
		return err
	}
	ic := New(cfg["url"], cfg["api_key"], "alpha2web")
	var album string
	if cfg["album"] != "" {
		if album, err = ic.FindOrCreateAlbum(ctx, cfg["album"]); err != nil {
			return err
		}
	}
	for slot := uint32(1); slot <= 2; slot++ {
		cs, err := cam.SonyListAllContents(ctx, slot)
		if err != nil {
			s.env.Log.Debug("slot not readable", "slot", slot, "err", err)
			continue
		}
		for _, c := range cs {
			if err := s.content(ctx, cam, ic, album, cfg["delete_after_upload"] == "true", slot, c); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *syncer) content(ctx context.Context, cam *ptpip.Client, ic *Client, album string, del bool, slot uint32, c ptpip.SonyContent) error {
	for _, f := range c.Files {
		k := key(slot, c, f)
		if _, ok := s.done[k]; ok {
			continue
		}
		id, err := s.upload(ctx, cam, ic, slot, c, f)
		if err != nil {
			return fmt.Errorf("%s: %w", f.Path, err)
		}
		if album != "" {
			if _, err := ic.AddToAlbum(ctx, album, []string{id}); err != nil {
				return err
			}
		}
		s.done[k] = id
		if err := s.save(); err != nil {
			return err
		}
		s.env.Log.Info("uploaded", "file", f.Path)
	}
	if del && len(c.Files) > 0 && !c.Protected {
		if err := cam.SonyDeleteContent(ctx, slot, c.ID); err != nil {
			return fmt.Errorf("delete: %w", err)
		}
	}
	return nil
}

func (s *syncer) upload(ctx context.Context, cam *ptpip.Client, ic *Client, slot uint32, c ptpip.SonyContent, f ptpip.SonyContentFile) (string, error) {
	if err := os.MkdirAll(s.env.DataDir, 0o755); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(s.env.DataDir, "staging-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	h := sha1.New()
	n, err := cam.SonyDownloadContentFile(ctx, slot, c.ID, f.ID, f.Size, io.MultiWriter(tmp, h), nil)
	if err != nil {
		return "", err
	}
	if uint64(n) != f.Size {
		return "", fmt.Errorf("got %d of %d bytes", n, f.Size)
	}
	res, err := ic.Upload(ctx, UploadRequest{
		Path: tmp.Name(), Filename: path.Base(strings.ReplaceAll(f.Path, "\\", "/")),
		SHA1: hex.EncodeToString(h.Sum(nil)), CreatedAt: c.CreatedUTC, ModifiedAt: c.ModifiedUTC,
	})
	return res.ID, err
}

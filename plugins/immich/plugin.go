package immich

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sync/atomic"

	"github.com/knallli/alpha2web/plugin"
	"github.com/knallli/alpha2web/plugins/immich/store"
)

func init() { plugin.Register(&Plugin{}) }

// Plugin downloads new files from the camera's cards on request and uploads them to Immich.
type Plugin struct {
	pipe atomic.Pointer[pipeline]
}

func (*Plugin) Name() string { return "immich" }

const (
	groupImmich   = "Immich"
	groupFiles    = "Immich: files"
	groupTransfer = "Immich: transfer"
)

func (*Plugin) Fields() []plugin.Field {
	return []plugin.Field{
		{Key: "url", Label: "Immich address", Group: groupImmich, Placeholder: "http://immich:2283", Env: "IMMICH_URL"},
		{Key: "api_key", Label: "API key", Kind: plugin.FieldSecret, Group: groupImmich,
			Env:  "IMMICH_API_KEY",
			Help: "Create one in Immich under Account Settings → API Keys with these permissions: asset.upload, asset.view, asset.download, album.read, album.create, albumAsset.create, stack.create (view and download let the gallery show shots from Immich). The key is stored in the data volume and never shown again."},
		{Key: "album", Label: "Album (optional)", Group: groupImmich, Env: "IMMICH_ALBUM"},
		{Key: "stack_raw_jpeg", Label: "Stack RAW+JPEG pairs", Kind: plugin.FieldBool, Default: "true", Group: groupImmich,
			Env: "IMMICH_STACK_RAW_JPEG", Help: "Stack RAW+JPEG pairs in Immich (JPEG shown)"},

		{Key: "delete_local", Label: "Delete the downloaded copy after upload", Kind: plugin.FieldBool, Default: "true", Group: groupFiles,
			Env: "DELETE_AFTER_UPLOAD", Help: "Delete the local copy after a confirmed upload"},
		{Key: "delete_library", Label: "Delete the pulled library copy after upload", Kind: plugin.FieldBool, Default: "false", Group: groupFiles,
			Env:  "DELETE_LIBRARY_AFTER_UPLOAD",
			Help: "Files pulled in the gallery are also removed from the library folder once they are confirmed in Immich, so they live only there. Independent of deleting from the camera. Off by default."},
		{Key: "delete_on_camera", Label: "Delete from the camera once uploaded", Kind: plugin.FieldBool, Default: "false", Group: groupFiles,
			Env:  "IMMICH_DELETE_ON_CAMERA",
			Help: "Also delete the shot from the camera card after it is confirmed in Immich. Irreversible. Off by default; the old alpha2immich never deleted on the camera."},
		{Key: "skip_extensions", Label: "Skip extensions", Default: ".XML,.THM", Group: groupFiles, Env: "SKIP_EXTENSIONS", Help: `"none" skips nothing`},

		{Key: "upload_concurrency", Label: "Parallel uploads", Kind: plugin.FieldInt, Default: "2", Group: groupTransfer, Env: "UPLOAD_CONCURRENCY"},
		{Key: "stall_timeout", Label: "Abort a download without data after", Kind: plugin.FieldDuration, Default: "3m", Group: groupTransfer, Env: "DOWNLOAD_STALL_TIMEOUT"},
		{Key: "min_free_space_mb", Label: "Pause downloads below free space (MB)", Kind: plugin.FieldInt, Default: "1024", Group: groupTransfer, Env: "MIN_FREE_SPACE_MB"},
		{Key: "retry_base_delay", Label: "First retry delay", Kind: plugin.FieldDuration, Default: "5s", Group: groupTransfer, Env: "RETRY_BASE_DELAY"},
		{Key: "retry_max_delay", Label: "Longest retry delay", Kind: plugin.FieldDuration, Default: "15m", Group: groupTransfer, Env: "RETRY_MAX_DELAY"},
	}
}

// Validate rejects settings the pipeline would otherwise silently replace
// with defaults. Empty url and key are fine: files are still downloaded.
func (*Plugin) Validate(cfg map[string]string) error {
	s, err := parseSettings(cfg)
	if err != nil {
		return err
	}
	if (s.URL == "") != (s.APIKey == "") {
		return fmt.Errorf("set both the Immich address and the API key, or neither")
	}
	if s.URL != "" {
		if u, err := url.Parse(s.URL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("url: need an http:// or https:// address")
		}
	}
	return nil
}

// CurrentTransfer is the download in progress, or nil.
func (p *Plugin) CurrentTransfer() *plugin.Transfer {
	if pl := p.pipe.Load(); pl != nil {
		return pl.CurrentTransfer()
	}
	return nil
}

// Run blocks until ctx is done.
func (p *Plugin) Run(ctx context.Context, env plugin.Env) error {
	st, err := store.Open(filepath.Join(env.DataDir, "immich.db"))
	if err != nil {
		return err
	}
	defer st.Close()
	n, err := st.RecoverInterrupted(ctx)
	if err != nil {
		return err
	}
	if n > 0 {
		env.Log.Info("reset interrupted transfers", "files", n)
	}
	staging := os.Getenv("STAGING_DIR")
	if staging == "" {
		staging = filepath.Join(env.DataDir, "staging")
	}
	// Half-written downloads are never resumed.
	os.RemoveAll(filepath.Join(staging, ".partial"))
	if err := os.MkdirAll(staging, 0o755); err != nil {
		return err
	}
	pl := newPipeline(env, st, staging)
	p.pipe.Store(pl)
	defer p.pipe.Store(nil)
	pl.run(ctx)
	return nil
}

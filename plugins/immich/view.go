package immich

import (
	"context"
	"io"
	"net/url"

	"github.com/knallli/alpha2web/plugin"
	"github.com/knallli/alpha2web/plugins/immich/store"
)

var _ plugin.Viewer = (*Plugin)(nil)

// asset finds the Immich asset of a camera file that was uploaded.
func (p *Plugin) asset(ctx context.Context, f plugin.LocalFile) (*Client, string) {
	pl := p.pipe.Load()
	if pl == nil {
		return nil, ""
	}
	d := store.Discovered{Camera: sanitizeID(f.Camera), Slot: f.Slot, Path: cleanPath(f.Path), CapturedAt: f.CapturedAt, Size: f.Size}
	row, ok, err := pl.store.Lookup(ctx, d)
	if err != nil || !ok || row.AssetID == "" || (row.Status != store.StatusUploaded && row.Status != store.StatusDone) {
		return nil, ""
	}
	return pl.client(pl.cfg()), row.AssetID
}

// Stored reports whether the file is in Immich.
func (p *Plugin) Stored(ctx context.Context, f plugin.LocalFile) bool {
	im, id := p.asset(ctx, f)
	return im != nil && id != ""
}

// Open streams the asset (or its preview rendition) from Immich.
func (p *Plugin) Open(ctx context.Context, f plugin.LocalFile, preview bool) (io.ReadCloser, string, error) {
	im, id := p.asset(ctx, f)
	if im == nil || id == "" {
		return nil, "", plugin.ErrNotStored
	}
	path := "/assets/" + url.PathEscape(id) + "/original"
	if preview {
		path = "/assets/" + url.PathEscape(id) + "/thumbnail?size=preview"
	}
	return im.Fetch(ctx, path)
}

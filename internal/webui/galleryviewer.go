package webui

import (
	"context"
	"errors"
	"io"
	"strings"

	"github.com/knallli/alpha2web/plugin"
)

const maxViewBytes = 256 << 20 // a rendition is small; this only stops a runaway response

// localFile describes a catalog file the way plugins know it.
func (h *Handler) localFile(slot uint32, it catItem, f catFile) plugin.LocalFile {
	g := h.galleryState()
	g.mu.Lock()
	model := g.cat.Model
	g.mu.Unlock()
	p := drivePrefix.ReplaceAllString(strings.ReplaceAll(f.Path, "\\", "/"), "")
	return plugin.LocalFile{Camera: model, Slot: int(slot), Path: strings.TrimLeft(p, "/"), CapturedAt: it.Created, Size: int64(f.Size), Local: f.Local}
}

// stored reports whether a plugin (Immich) holds the file.
func (h *Handler) stored(ctx context.Context, slot uint32, it catItem, f catFile) bool {
	lf := h.localFile(slot, it, f)
	for _, p := range plugin.All() {
		if v, ok := p.(plugin.Viewer); ok && v.Stored(ctx, lf) {
			return true
		}
	}
	return false
}

// openStored streams the file from the first plugin that holds it.
func (h *Handler) openStored(ctx context.Context, slot uint32, it catItem, f catFile, size string) (io.ReadCloser, string, bool) {
	lf := h.localFile(slot, it, f)
	for _, p := range plugin.All() {
		v, ok := p.(plugin.Viewer)
		if !ok {
			continue
		}
		rc, ct, err := v.Open(ctx, lf, size)
		if err == nil {
			return rc, ct, true
		}
		if !errors.Is(err, plugin.ErrNotStored) {
			h.warnOnce("viewing a stored copy failed ("+p.Name()+"; Immich needs the asset.view and asset.download permissions)", err)
		}
	}
	return nil, "", false
}

// storedImage reads an image rendition ("thumbnail" or "preview") a plugin holds.
func (h *Handler) storedImage(ctx context.Context, slot uint32, it catItem, f catFile, size string) ([]byte, bool) {
	rc, _, ok := h.openStored(ctx, slot, it, f, size)
	if !ok {
		return nil, false
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, maxViewBytes))
	return data, err == nil && len(data) > 0
}

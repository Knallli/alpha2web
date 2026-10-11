package immich

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/knallli/alpha2web/plugin"
	"github.com/knallli/alpha2web/plugins/immich/store"
	"github.com/knallli/alpha2web/plugins/immich/verify"
)

var _ plugin.Importer = (*Plugin)(nil)

// Import queues files the gallery copied to the library for upload. It needs
// no camera session. The library files are only read (hard-linked or copied
// into staging) and only deleted when "delete_library" is on, after the upload
// is confirmed.
func (p *Plugin) Import(ctx context.Context, files []plugin.LocalFile) error {
	pl := p.pipe.Load()
	if pl == nil {
		return fmt.Errorf("immich: not running yet")
	}
	s := pl.cfg()
	var errs []error
	for _, lf := range files {
		if err := pl.importOne(ctx, s, lf); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", lf.Path, err))
		}
	}
	return errors.Join(errs...)
}

func (p *pipeline) importOne(ctx context.Context, s settings, lf plugin.LocalFile) error {
	d := store.Discovered{Camera: sanitizeID(lf.Camera), Slot: lf.Slot, Path: cleanPath(lf.Path), CapturedAt: lf.CapturedAt, Size: lf.Size}
	if s.skipped(d.Path) {
		return nil
	}
	d.GroupKey = groupKey(d)
	db := context.WithoutCancel(ctx)
	if _, err := p.store.UpsertDiscovered(db, []store.Discovered{d}); err != nil {
		return err
	}
	f, ok, err := p.store.Lookup(db, d)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("not recorded")
	}
	if f.Status != store.StatusPending {
		return nil
	}
	if err := p.store.StartDownload(db, f.ID); err != nil {
		if errors.Is(err, store.ErrInvalidTransition) {
			return nil // a sync claimed it first
		}
		return err
	}
	final := p.stagedPath(f)
	v, err := p.stage(lf.Local, final, f.Path)
	if err == nil {
		if err = p.store.FinishDownload(db, f.ID, final, v.Size, v.SHA1); err != nil {
			os.Remove(final)
		} else if err = p.store.SetSourcePath(db, f.ID, lf.Local); err != nil {
			p.log.Warn("record library path", "file", f.Path, "err", err) // the copy then just stays
			err = nil
		}
	}
	if err != nil {
		next := time.Now().Add(s.Retry.Delay(f.DownloadAttempts + 1))
		if ferr := p.store.FailDownload(db, f.ID, err.Error(), next, true); ferr != nil {
			p.log.Error("record import failure", "file", f.Path, "err", ferr)
		}
		return err
	}
	p.Metrics.Downloads.Add(1)
	p.Metrics.DownloadBytes.Add(v.Size)
	p.Metrics.lastDownload.Store(time.Now().Unix())
	p.log.Info("imported", "file", f.Path, "bytes", v.Size, "sha1", v.SHA1)
	wake(p.uploadWake)
	return nil
}

// stage links (or copies) src to dst and verifies it; on failure dst is gone.
func (p *pipeline) stage(src, dst, camPath string) (verify.Result, error) {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return verify.Result{}, err
	}
	os.Remove(dst)
	if os.Link(src, dst) != nil {
		if err := copyFile(src, dst); err != nil {
			os.Remove(dst)
			return verify.Result{}, err
		}
	}
	v, err := verify.File(dst, camPath)
	if err != nil {
		os.Remove(dst)
		return v, fmt.Errorf("verify: %w", err)
	}
	return v, nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

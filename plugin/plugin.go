// Package plugin is the extension point for alpha2web. Plugins are compiled in
// and register themselves from init.
package plugin

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/knallli/alpha2go/ptpip"
	"github.com/knallli/alpha2web/internal/camera"
)

// Camera is what a plugin sees of the camera. *camera.Supervisor implements it.
type Camera interface {
	Status() camera.Status
	// Client is only valid while the session lasts.
	Client() (*ptpip.Client, error)
	// Acquire opens the session if needed; call release when done, the
	// session closes after a linger time.
	Acquire(ctx context.Context) (*ptpip.Client, func(), error)
	Subscribe() (events <-chan camera.Event, stop func())
}

// LocalFile is a camera file the gallery copied to local disk.
type LocalFile struct {
	Camera     string // camera model, as in camera.Status.Model
	Slot       int
	Path       string    // camera path without drive, e.g. "DCIM/100MSDCF/DSC00001.JPG"
	CapturedAt time.Time // content creation time (SonyContent.CreatedUTC)
	Size       int64
	Local      string // absolute path of the copy; importers must not move or delete it unless the user opted in
}

// ErrNotStored means the plugin does not hold the file.
var ErrNotStored = errors.New("plugin does not hold this file")

// Viewer is optional: it serves a file the plugin has stored elsewhere (for
// example in Immich), so the gallery can show it after the library copy is gone.
type Viewer interface {
	// Stored reports whether the plugin holds the file.
	Stored(ctx context.Context, f LocalFile) bool
	// Open returns the original, or with preview a JPEG/WebP rendition, and its
	// content type. It returns ErrNotStored if the file is not held.
	Open(ctx context.Context, f LocalFile, preview bool) (io.ReadCloser, string, error)
}

// Importer is optional: files pulled in the gallery are handed to it.
type Importer interface {
	Import(ctx context.Context, files []LocalFile) error
}

// Field describes one setting of a plugin for the settings page.
type Field struct {
	Key, Label  string
	Kind        FieldKind // FieldSecret is shown as a password field, never echoed back
	Default     string    // shown as placeholder; the plugin applies it itself
	Group       string    // card heading; empty = "Plugin: <name>"
	Placeholder string
	Help        string
	// Env, if set and non-empty in the environment, overrides the stored
	// value; the settings page shows the field locked.
	Env string
}

// Overlay returns cfg with the values of non-empty environment variables named
// by the fields' Env applied. cfg itself is not modified.
func Overlay(fields []Field, cfg map[string]string, getenv func(string) string) map[string]string {
	out := make(map[string]string, len(cfg)+len(fields))
	for k, v := range cfg {
		out[k] = v
	}
	for _, f := range fields {
		if f.Env != "" {
			if v := getenv(f.Env); v != "" {
				out[f.Key] = v
			}
		}
	}
	return out
}

// Env is handed to Run.
type Env struct {
	Camera  Camera
	Log     *slog.Logger
	DataDir string
	// Config returns the plugin's current settings; call it again to see changes.
	Config func() map[string]string
}

// Plugin reacts to camera events, e.g. exports new images somewhere.
type Plugin interface {
	Name() string
	Fields() []Field
	// Run blocks until ctx is done.
	Run(ctx context.Context, env Env) error
}

var (
	mu       sync.Mutex
	registry = map[string]Plugin{}
)

// Register adds p; it panics on duplicate names.
func Register(p Plugin) {
	mu.Lock()
	defer mu.Unlock()
	if _, ok := registry[p.Name()]; ok {
		panic("plugin: duplicate " + p.Name())
	}
	registry[p.Name()] = p
}

// All returns the registered plugins sorted by name.
func All() []Plugin {
	mu.Lock()
	defer mu.Unlock()
	out := make([]Plugin, 0, len(registry))
	for _, p := range registry {
		out = append(out, p)
	}
	slices.SortFunc(out, func(a, b Plugin) int { return strings.Compare(a.Name(), b.Name()) })
	return out
}

// RunAll starts every registered plugin and returns when ctx is done and all
// of them have stopped. config(name) supplies each plugin's settings. A plugin
// that fails is logged, not restarted.
func RunAll(ctx context.Context, base Env, config func(name string) map[string]string) {
	var wg sync.WaitGroup
	for _, p := range All() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			env := base
			env.Log = base.Log.With("plugin", p.Name())
			env.Config = func() map[string]string { return config(p.Name()) }
			if err := p.Run(ctx, env); err != nil && ctx.Err() == nil {
				env.Log.Error("plugin stopped", "err", err)
			}
		}()
	}
	wg.Wait()
}

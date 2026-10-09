// Package plugin is the extension point for alpha2web. Plugins are compiled in
// and register themselves from init.
package plugin

import (
	"context"
	"log/slog"
	"sync"

	"github.com/knallli/alpha2go/ptpip"
	"github.com/knallli/alpha2web/internal/camera"
)

// Camera is what a plugin sees of the camera. *camera.Supervisor implements it.
type Camera interface {
	Status() camera.Status
	// Client is only valid while the session lasts.
	Client() (*ptpip.Client, error)
	Subscribe() (events <-chan camera.Event, stop func())
}

// Plugin reacts to camera events, e.g. exports new images somewhere.
type Plugin interface {
	Name() string
	// Run blocks until ctx is done.
	Run(ctx context.Context, cam Camera, log *slog.Logger) error
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

// All returns the registered plugins.
func All() []Plugin {
	mu.Lock()
	defer mu.Unlock()
	out := make([]Plugin, 0, len(registry))
	for _, p := range registry {
		out = append(out, p)
	}
	return out
}

// RunAll starts every registered plugin and returns when ctx is done and all
// of them have stopped. A plugin that fails is logged, not restarted.
func RunAll(ctx context.Context, cam Camera, log *slog.Logger) {
	var wg sync.WaitGroup
	for _, p := range All() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l := log.With("plugin", p.Name())
			if err := p.Run(ctx, cam, l); err != nil && ctx.Err() == nil {
				l.Error("plugin stopped", "err", err)
			}
		}()
	}
	wg.Wait()
}

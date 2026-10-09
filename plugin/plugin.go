// Package plugin is the extension point for alpha2web. Plugins are compiled in
// and register themselves from init.
package plugin

import (
	"context"
	"log/slog"
	"sync"
)

// Camera is what a plugin sees of the connected camera.
type Camera interface {
	Model() string
	Host() string
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

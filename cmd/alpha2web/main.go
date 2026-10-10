package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/knallli/alpha2go/ptpip"
	"github.com/knallli/alpha2go/wol"
	"github.com/knallli/alpha2web/internal/backoff"
	"github.com/knallli/alpha2web/internal/camera"
	"github.com/knallli/alpha2web/internal/logbuf"
	"github.com/knallli/alpha2web/internal/settings"
	"github.com/knallli/alpha2web/internal/webui"
	"github.com/knallli/alpha2web/plugin"
	_ "github.com/knallli/alpha2web/plugins/immich"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// envFor maps a settings field to the variable that overrides it.
var envFor = map[string]string{
	"host": "CAMERA_HOST", "mac": "CAMERA_MAC", "user": "CAMERA_USER", "password": "CAMERA_PASSWORD", "fingerprint": "CAMERA_FINGERPRINT",
}

func locked(field string) bool { return os.Getenv(envFor[field]) != "" }

// effective merges the saved settings with the environment (environment wins).
func effective(v settings.Values, dataDir string) camera.Config {
	pick := func(field, saved string) string { return env(envFor[field], saved) }
	cfg := camera.Config{Host: pick("host", v.CameraHost), MAC: pick("mac", v.CameraMAC), DataDir: dataDir, Keepalive: keepalive(os.Getenv("CAMERA_KEEPALIVE"))}
	if user := pick("user", v.CameraUser); user != "" {
		cfg.SSH = &ptpip.SSHConfig{User: user, Password: pick("password", v.CameraPassword), Fingerprint: pick("fingerprint", v.CameraFingerprint)}
	}
	return cfg
}

// keepalive parses CAMERA_KEEPALIVE: default 10s, "0" disables it.
func keepalive(v string) time.Duration {
	if d, err := time.ParseDuration(v); err == nil && d >= 0 {
		return d
	}
	return 10 * time.Second
}

// pluginConfig returns the stored settings of a plugin with its environment
// variables applied (environment wins).
func pluginConfig(v settings.Values, name string) map[string]string {
	for _, p := range plugin.All() {
		if p.Name() == name {
			return plugin.Overlay(p.Fields(), v.Plugins[name], os.Getenv)
		}
	}
	return v.Plugins[name]
}

var version = "dev"

// subcommand handles "version" and "healthcheck" (used by Docker's HEALTHCHECK).
func subcommand(arg string) {
	switch arg {
	case "version":
		fmt.Println(version)
	case "healthcheck":
		addr := env("HTTP_ADDR", ":8765")
		if strings.HasPrefix(addr, ":") {
			addr = "127.0.0.1" + addr
		}
		c := http.Client{Timeout: 5 * time.Second}
		resp, err := c.Get("http://" + addr + "/healthz")
		if err != nil || resp.StatusCode != 200 {
			os.Exit(1)
		}
	default:
		fmt.Fprintln(os.Stderr, "unknown command", arg)
		os.Exit(2)
	}
	os.Exit(0)
}

// newLogHandler builds the slog handler from LOG_LEVEL and LOG_FORMAT.
func newLogHandler(w io.Writer, level, format string) slog.Handler {
	var l slog.Level
	if l.UnmarshalText([]byte(level)) != nil {
		l = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: l}
	if format == "text" {
		return slog.NewTextHandler(w, opts)
	}
	return slog.NewJSONHandler(w, opts)
}

func envDuration(log *slog.Logger, k string, def time.Duration) time.Duration {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		log.Warn("bad duration, using default", "var", k, "value", v, "default", def)
		return def
	}
	return d
}

// wakeLoop runs run for the current MAC and restarts it when the MAC changes
// (settings page); with no MAC nothing runs.
func wakeLoop(ctx context.Context, mac func() string, poll time.Duration, run func(ctx context.Context, mac string)) {
	for ctx.Err() == nil {
		cur := mac()
		rctx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		if cur != "" {
			go func() { defer close(done); run(rctx, cur) }()
		} else {
			close(done)
		}
		for ctx.Err() == nil && mac() == cur {
			select {
			case <-ctx.Done():
			case <-time.After(poll):
			}
		}
		cancel()
		<-done
	}
}

func main() {
	if len(os.Args) > 1 {
		subcommand(os.Args[1])
	}
	ring := logbuf.NewRing(500)
	log := slog.New(logbuf.NewHandler(newLogHandler(os.Stderr, env("LOG_LEVEL", "info"), env("LOG_FORMAT", "json")), ring))
	slog.SetDefault(log)

	dataDir := env("DATA_DIR", "data")
	store, err := settings.Open(filepath.Join(dataDir, "settings.json"))
	if err != nil {
		log.Error("settings", "err", err)
		os.Exit(1)
	}
	sup := camera.New(effective(store.Get(), dataDir), backoff.Policy{Base: envDuration(log, "CAMERA_RECONNECT_INTERVAL", 5*time.Second), Max: time.Minute, Jitter: 0.2}, log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go sup.Run(ctx)
	if env("CAMERA_WAKE_ON_LAN", "") == "true" {
		addr := env("CAMERA_WAKE_ON_LAN_ADDR", "255.255.255.255:9")
		go wakeLoop(ctx, sup.MAC, 5*time.Second, func(c context.Context, mac string) {
			wol.Loop(c, mac, addr, 30*time.Second, func() bool { return sup.Status().Connected }, log)
		})
	}
	go plugin.RunAll(ctx, plugin.Env{Camera: sup, Log: log, DataDir: dataDir},
		func(name string) map[string]string { return pluginConfig(store.Get(), name) })

	srv := &http.Server{Addr: env("HTTP_ADDR", ":8765"), Handler: &webui.Handler{
		Version: version, Camera: sup, Logs: ring, Logger: log, Password: os.Getenv("WEBUI_PASSWORD"), CacheDir: filepath.Join(dataDir, "cache"),
		Settings: &webui.Settings{Store: store, Locked: locked, Apply: func(v settings.Values) { sup.SetConfig(effective(v, dataDir)) }},
	}}
	done := make(chan struct{})
	go func() {
		defer close(done)
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), envDuration(log, "SHUTDOWN_TIMEOUT", 20*time.Second))
		defer cancel()
		if err := srv.Shutdown(sctx); err != nil {
			srv.Close() // long-lived streams (live view) outlast the grace period
		}
	}()
	log.Info("listening", "addr", srv.Addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("server", "err", err)
		os.Exit(1)
	}
	<-done
}

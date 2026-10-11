package main

import (
	"cmp"
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
	"github.com/knallli/alpha2web/internal/export"
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
	"keepalive":  "CAMERA_KEEPALIVE",
	"export_dir": "LIBRARY_DIR", "export_template": "EXPORT_PATH_TEMPLATE", "export_case": "EXPORT_CASE", "export_types": "EXPORT_TYPES",
	"export_conflict": "EXPORT_CONFLICT", "export_mtime": "EXPORT_SET_MTIME",
}

// effectiveExport merges the saved export settings with the environment (environment wins).
func effectiveExport(v settings.Values, dataDir string) export.Config {
	pick := func(field, saved string) string { return env(envFor[field], saved) }
	return export.New(env("LIBRARY_DIR", cmp.Or(v.ExportDir, filepath.Join(dataDir, "library"))), pick("export_template", v.ExportTemplate), pick("export_case", v.ExportCase),
		pick("export_types", v.ExportTypes), pick("export_conflict", v.ExportConflict), pick("export_mtime", v.ExportMtime))
}

func locked(field string) bool { return os.Getenv(envFor[field]) != "" }

// effective merges the saved settings with the environment (environment wins).
func effective(v settings.Values, dataDir string) camera.Config {
	pick := func(field, saved string) string { return env(envFor[field], saved) }
	cfg := camera.Config{Host: pick("host", v.CameraHost), MAC: pick("mac", v.CameraMAC), DataDir: dataDir, Keepalive: keepalive(env("CAMERA_KEEPALIVE", v.Keepalive)), Linger: idleDisconnect(os.Getenv("CAMERA_IDLE_DISCONNECT"))}
	if user := pick("user", v.CameraUser); user != "" {
		cfg.SSH = &ptpip.SSHConfig{User: user, Password: pick("password", v.CameraPassword), Fingerprint: pick("fingerprint", v.CameraFingerprint)}
	}
	return cfg
}

// keepalive parses CAMERA_KEEPALIVE: default 10s, "0" turns off every
// property request of our own (negative for the supervisor).
func keepalive(v string) time.Duration {
	if d, err := time.ParseDuration(v); err == nil && d >= 0 {
		if d == 0 {
			return -1
		}
		return d
	}
	return 10 * time.Second
}

// idleDisconnect parses CAMERA_IDLE_DISCONNECT: how long an unused session
// stays open, default 30s.
func idleDisconnect(v string) time.Duration {
	if d, err := time.ParseDuration(v); err == nil && d > 0 {
		return d
	}
	return camera.DefaultLinger
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

// logLevel can be raised to debug from the settings page; envLevel is what
// LOG_LEVEL asked for.
var (
	logLevel slog.LevelVar
	envLevel slog.Level
)

func applyLogLevel(v settings.Values) {
	if v.DebugLog {
		logLevel.Set(slog.LevelDebug)
	} else {
		logLevel.Set(envLevel)
	}
}

// newLogHandler builds the slog handler from LOG_LEVEL and LOG_FORMAT.
func newLogHandler(w io.Writer, level, format string) slog.Handler {
	var l slog.Level
	if l.UnmarshalText([]byte(level)) != nil {
		l = slog.LevelInfo
	}
	envLevel = l
	logLevel.Set(l)
	opts := &slog.HandlerOptions{Level: &logLevel}
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
	applyLogLevel(store.Get())
	sup := camera.New(effective(store.Get(), dataDir), backoff.Policy{Base: envDuration(log, "CAMERA_RECONNECT_INTERVAL", 5*time.Second), Max: time.Minute, Jitter: 0.2}, log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go sup.Run(ctx)
	if env("CAMERA_WAKE_ON_LAN", "") == "true" {
		addr := env("CAMERA_WAKE_ON_LAN_ADDR", "255.255.255.255:9")
		go wakeLoop(ctx, sup.MAC, 5*time.Second, func(c context.Context, mac string) {
			wol.Loop(c, mac, addr, 30*time.Second, func() bool { return !sup.Status().Connecting }, log)
		})
	}
	go plugin.RunAll(ctx, plugin.Env{Camera: sup, Log: log, DataDir: dataDir},
		func(name string) map[string]string { return pluginConfig(store.Get(), name) })

	srv := &http.Server{Addr: env("HTTP_ADDR", ":8765"), Handler: &webui.Handler{
		Version: version, Camera: sup, Logs: ring, Logger: log, Password: os.Getenv("WEBUI_PASSWORD"), CacheDir: filepath.Join(dataDir, "cache"), Export: func() export.Config { return effectiveExport(store.Get(), dataDir) },
		Settings: &webui.Settings{Store: store, Locked: locked, Apply: func(v settings.Values) { applyLogLevel(v); sup.SetConfig(effective(v, dataDir)) }},
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

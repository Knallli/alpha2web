package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/knallli/alpha2go/ptpip"
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
	cfg := camera.Config{Host: pick("host", v.CameraHost), MAC: pick("mac", v.CameraMAC), DataDir: dataDir}
	if user := pick("user", v.CameraUser); user != "" {
		cfg.SSH = &ptpip.SSHConfig{User: user, Password: pick("password", v.CameraPassword), Fingerprint: pick("fingerprint", v.CameraFingerprint)}
	}
	return cfg
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

func main() {
	if len(os.Args) > 1 {
		subcommand(os.Args[1])
	}
	ring := logbuf.NewRing(500)
	log := slog.New(logbuf.NewHandler(slog.NewTextHandler(os.Stderr, nil), ring))
	slog.SetDefault(log)

	dataDir := env("DATA_DIR", "data")
	store, err := settings.Open(filepath.Join(dataDir, "settings.json"))
	if err != nil {
		log.Error("settings", "err", err)
		os.Exit(1)
	}
	sup := camera.New(effective(store.Get(), dataDir), backoff.Policy{Base: 5 * time.Second, Max: time.Minute, Jitter: 0.2}, log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go sup.Run(ctx)
	go plugin.RunAll(ctx, plugin.Env{Camera: sup, Log: log, DataDir: dataDir},
		func(name string) map[string]string { return store.Get().Plugins[name] })

	srv := &http.Server{Addr: env("HTTP_ADDR", ":8765"), Handler: &webui.Handler{
		Version: version, Camera: sup, Logs: ring, Logger: log, Password: os.Getenv("WEBUI_PASSWORD"), CacheDir: filepath.Join(dataDir, "cache"),
		Settings: &webui.Settings{Store: store, Locked: locked, Apply: func(v settings.Values) { sup.SetConfig(effective(v, dataDir)) }},
	}}
	go func() { <-ctx.Done(); srv.Close() }()
	log.Info("listening", "addr", srv.Addr)
	if err := srv.ListenAndServe(); err != nil && ctx.Err() == nil {
		log.Error("server", "err", err)
		os.Exit(1)
	}
}

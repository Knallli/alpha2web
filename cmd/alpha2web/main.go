package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

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
	"host": "CAMERA_HOST", "user": "CAMERA_USER", "password": "CAMERA_PASSWORD", "fingerprint": "CAMERA_FINGERPRINT",
}

func locked(field string) bool { return os.Getenv(envFor[field]) != "" }

// effective merges the saved settings with the environment (environment wins).
func effective(v settings.Values) camera.Config {
	pick := func(field, saved string) string { return env(envFor[field], saved) }
	cfg := camera.Config{Host: pick("host", v.CameraHost)}
	if user := pick("user", v.CameraUser); user != "" {
		cfg.SSH = &ptpip.SSHConfig{User: user, Password: pick("password", v.CameraPassword), Fingerprint: pick("fingerprint", v.CameraFingerprint)}
	}
	return cfg
}

func main() {
	ring := logbuf.NewRing(500)
	log := slog.New(logbuf.NewHandler(slog.NewTextHandler(os.Stderr, nil), ring))
	slog.SetDefault(log)

	dataDir := env("DATA_DIR", "data")
	store, err := settings.Open(filepath.Join(dataDir, "settings.json"))
	if err != nil {
		log.Error("settings", "err", err)
		os.Exit(1)
	}
	sup := camera.New(effective(store.Get()), backoff.Default, log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go sup.Run(ctx)
	go plugin.RunAll(ctx, plugin.Env{Camera: sup, Log: log, DataDir: dataDir},
		func(name string) map[string]string { return store.Get().Plugins[name] })

	srv := &http.Server{Addr: env("HTTP_ADDR", ":8080"), Handler: &webui.Handler{
		Version: "dev", Camera: sup, Logs: ring, Logger: log, Password: os.Getenv("WEBUI_PASSWORD"),
		Settings: &webui.Settings{Store: store, Locked: locked, Apply: func(v settings.Values) { sup.SetConfig(effective(v)) }},
	}}
	go func() { <-ctx.Done(); srv.Close() }()
	log.Info("listening", "addr", srv.Addr)
	if err := srv.ListenAndServe(); err != nil && ctx.Err() == nil {
		log.Error("server", "err", err)
		os.Exit(1)
	}
}

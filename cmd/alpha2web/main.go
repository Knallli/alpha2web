package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/knallli/alpha2go/ptpip"
	"github.com/knallli/alpha2web/internal/backoff"
	"github.com/knallli/alpha2web/internal/camera"
	"github.com/knallli/alpha2web/internal/logbuf"
	"github.com/knallli/alpha2web/internal/webui"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	ring := logbuf.NewRing(500)
	log := slog.New(logbuf.NewHandler(slog.NewTextHandler(os.Stderr, nil), ring))
	slog.SetDefault(log)
	host := os.Getenv("CAMERA_HOST")
	if host == "" {
		log.Error("CAMERA_HOST is required")
		os.Exit(1)
	}
	cfg := camera.Config{Host: host}
	if u := os.Getenv("CAMERA_USER"); u != "" {
		cfg.SSH = &ptpip.SSHConfig{User: u, Password: os.Getenv("CAMERA_PASSWORD"), Fingerprint: os.Getenv("CAMERA_FINGERPRINT")}
	}
	sup := camera.New(cfg, backoff.Default, log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go sup.Run(ctx)

	srv := &http.Server{Addr: env("HTTP_ADDR", ":8080"), Handler: &webui.Handler{Version: "dev", Camera: sup, Logs: ring, Password: os.Getenv("WEBUI_PASSWORD")}}
	go func() { <-ctx.Done(); srv.Close() }()
	log.Info("listening", "addr", srv.Addr)
	if err := srv.ListenAndServe(); err != nil && ctx.Err() == nil {
		log.Error("server", "err", err)
		os.Exit(1)
	}
}

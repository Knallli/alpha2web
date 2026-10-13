package main

import (
	"context"
	"github.com/knallli/alpha2web/internal/camera"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/knallli/alpha2web/internal/settings"
)

func TestEffective(t *testing.T) {
	v := settings.Values{CameraHost: "saved", CameraUser: "u", CameraPassword: "p"}
	if c := effective(v, ""); c.Host != "saved" || c.SSH == nil || c.SSH.Password != "p" {
		t.Fatalf("%+v", c)
	}
	t.Setenv("CAMERA_HOST", "env")
	if c := effective(v, ""); c.Host != "env" || !locked("host") {
		t.Fatalf("%+v", c)
	}
	if c := effective(settings.Values{}, ""); c.SSH != nil {
		t.Fatal("no user: no ssh")
	}
}

func TestWakeLoopFollowsMAC(t *testing.T) {
	var mu sync.Mutex
	mac := "aa"
	get := func() string { mu.Lock(); defer mu.Unlock(); return mac }
	started := make(chan string, 4)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		wakeLoop(ctx, get, 5*time.Millisecond, func(c context.Context, m string) { started <- m; <-c.Done() })
		close(done)
	}()
	recv := func() string {
		select {
		case m := <-started:
			return m
		case <-time.After(2 * time.Second):
			t.Fatal("timeout")
			return ""
		}
	}
	if m := recv(); m != "aa" {
		t.Fatal(m)
	}
	mu.Lock()
	mac = "bb"
	mu.Unlock()
	if m := recv(); m != "bb" {
		t.Fatal(m)
	}
	cancel()
	<-done
}

func TestLogHandler(t *testing.T) {
	var b strings.Builder
	log := slog.New(newLogHandler(&b, "warn", "json"))
	log.Info("hidden")
	log.Warn("shown")
	if s := b.String(); strings.Contains(s, "hidden") || !strings.Contains(s, `"msg":"shown"`) {
		t.Fatal(s)
	}
}

func TestPluginConfigOverlay(t *testing.T) {
	v := settings.Values{Plugins: map[string]map[string]string{"immich": {"url": "saved", "album": "A"}}}
	t.Setenv("IMMICH_URL", "env")
	t.Setenv("IMMICH_ALBUM", "")
	got := pluginConfig(v, "immich")
	if got["url"] != "env" || got["album"] != "A" {
		t.Fatalf("%v", got)
	}
	if v.Plugins["immich"]["url"] != "saved" {
		t.Fatal("stored settings modified")
	}
	if pluginConfig(v, "unknown") != nil {
		t.Fatal("unknown plugin")
	}
}

func TestKeepalive(t *testing.T) {
	for in, want := range map[string]time.Duration{"": 10 * time.Second, "0": -1, "3s": 3 * time.Second, "junk": 10 * time.Second, "-1s": 10 * time.Second} {
		if got := keepalive(in); got != want {
			t.Errorf("%q: %v", in, got)
		}
	}
}

func TestIdleDisconnect(t *testing.T) {
	for in, want := range map[string]time.Duration{"": camera.DefaultLinger, "0": -1, "5s": 5 * time.Second, "junk": camera.DefaultLinger, "-1s": camera.DefaultLinger} {
		if got := idleDisconnect(in); got != want {
			t.Errorf("%q: %v", in, got)
		}
	}
}

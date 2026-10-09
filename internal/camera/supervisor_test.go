package camera

import (
	"context"
	"github.com/knallli/alpha2go/discovery"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/knallli/alpha2go/ptpip/ptpiptest"
	"github.com/knallli/alpha2web/internal/backoff"
)

func TestReconnect(t *testing.T) {
	srv, err := ptpiptest.Start(ptpiptest.Options{Dir: t.TempDir(), A6700: true})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	s := New(Config{Host: srv.Addr()}, backoff.Policy{Base: 20 * time.Millisecond, Max: 50 * time.Millisecond}, slog.Default())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	wait := func(want bool) {
		t.Helper()
		for i := 0; i < 200; i++ {
			if s.Status().Connected == want {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("connected never became %v", want)
	}
	wait(true)
	if m := s.Status().Model; m != "ILCE-6700" {
		t.Fatalf("model %q", m)
	}
	if _, err := s.Client(); err != nil {
		t.Fatal(err)
	}
	srv.DropConnections()
	wait(false)
	wait(true)
	if s.Sessions() < 2 || s.Restarts() < 1 {
		t.Fatalf("sessions %d restarts %d", s.Sessions(), s.Restarts())
	}
}

func TestFailureCounter(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close() // nobody listens here now
	s := New(Config{Host: addr, MAC: "aa:bb:cc:dd:ee:ff"}, backoff.Policy{Base: 10 * time.Millisecond, Max: 20 * time.Millisecond}, slog.Default())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)
	for i := 0; s.Failures() < 2; i++ {
		if i > 200 {
			t.Fatal("no failures counted")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if s.Sessions() != 0 || s.MAC() != "aa:bb:cc:dd:ee:ff" {
		t.Fatal(s.Sessions(), s.MAC())
	}
}

func TestSubscribe(t *testing.T) {
	srv, err := ptpiptest.Start(ptpiptest.Options{Dir: t.TempDir(), A6700: true})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	s := New(Config{Host: srv.Addr()}, backoff.Default, slog.Default())
	ch, stop := s.Subscribe()
	defer stop()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)
	next := func() Event {
		select {
		case e := <-ch:
			return e
		case <-time.After(3 * time.Second):
			t.Fatal("no event")
			return Event{}
		}
	}
	if e := next(); e.Connected == nil || !*e.Connected {
		t.Fatalf("want connected, got %+v", e)
	}
	srv.Emit(0xC201, 7)
	if e := next(); e.Connected != nil || e.Code != 0xC201 || e.Params[0] != 7 {
		t.Fatalf("want ptp event, got %+v", e)
	}
}

func TestSetConfig(t *testing.T) {
	a, _ := ptpiptest.Start(ptpiptest.Options{Dir: t.TempDir(), A6700: true})
	b, _ := ptpiptest.Start(ptpiptest.Options{Dir: t.TempDir(), A6700: true, Model: "ILCE-7M4"})
	defer a.Close()
	defer b.Close()
	s := New(Config{}, backoff.Policy{Base: time.Hour, Max: time.Hour}, slog.Default())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)
	for _, c := range []struct{ addr, model string }{{a.Addr(), "ILCE-6700"}, {b.Addr(), "ILCE-7M4"}} {
		time.Sleep(50 * time.Millisecond)
		s.SetConfig(Config{Host: c.addr})
		ok := false
		for i := 0; i < 100 && !ok; i++ {
			st := s.Status()
			ok = st.Connected && st.Model == c.model
			time.Sleep(20 * time.Millisecond)
		}
		if !ok {
			t.Fatalf("never connected to %s: %+v", c.model, s.Status())
		}
	}
}

func TestDiscovery(t *testing.T) {
	srv, err := ptpiptest.Start(ptpiptest.Options{Dir: t.TempDir(), A6700: true})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	_, portStr, _ := net.SplitHostPort(srv.Addr())
	port, _ := strconv.Atoi(portStr)
	dir := t.TempDir()
	cfg := Config{DataDir: dir, Discovery: discovery.Options{
		Port: port, SSDPAddr: "-", Subnets: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")},
	}}
	s := New(cfg, backoff.Policy{Base: 20 * time.Millisecond, Max: 50 * time.Millisecond}, slog.Default())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)
	for i := 0; !s.Status().Connected; i++ {
		if i > 300 {
			t.Fatalf("not found: %+v", s.Status())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "camera-host")); !strings.HasPrefix(string(got), "127.0.0.1:") {
		t.Fatalf("cached host %q", got)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "ptpip-guid")); len(b) != 16 {
		t.Fatal("guid not persisted")
	}
}

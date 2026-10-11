package camera

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/knallli/alpha2go/discovery"
	"github.com/knallli/alpha2go/ptpip"
	"github.com/knallli/alpha2go/ptpip/ptpiptest"
	"github.com/knallli/alpha2web/internal/backoff"
)

func TestReconnect(t *testing.T) {
	srv, err := ptpiptest.Start(ptpiptest.Options{Dir: t.TempDir(), A6700: true})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	s := New(Config{Host: srv.Addr(), Linger: time.Hour}, backoff.Policy{Base: 20 * time.Millisecond, Max: 50 * time.Millisecond}, slog.Default())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)
	s.Connect()

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

func TestFailureNoRetry(t *testing.T) {
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
	t0 := time.Now()
	if _, _, err := s.Acquire(ctx); err == nil || time.Since(t0) > 5*time.Second {
		t.Fatalf("want a prompt error, got %v after %v", err, time.Since(t0))
	}
	// nothing wants the camera any more: no second attempt
	time.Sleep(300 * time.Millisecond)
	if st := s.Status(); s.Failures() != 1 || s.Sessions() != 0 || s.MAC() != "aa:bb:cc:dd:ee:ff" || st.Connecting || st.LastError == "" {
		t.Fatalf("failures %d sessions %d %+v", s.Failures(), s.Sessions(), st)
	}
}

func TestSubscribe(t *testing.T) {
	srv, err := ptpiptest.Start(ptpiptest.Options{Dir: t.TempDir(), A6700: true})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	s := New(Config{Host: srv.Addr(), Linger: time.Hour}, backoff.Default, slog.Default())
	ch, stop := s.Subscribe()
	defer stop()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)
	s.Connect()
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
	s := New(Config{Linger: time.Hour}, backoff.Policy{Base: time.Hour, Max: time.Hour}, slog.Default())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)
	s.Connect()
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
	cfg := Config{DataDir: dir, Linger: time.Hour, Discovery: discovery.Options{
		Port: port, SSDPAddr: "-", Subnets: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")},
	}}
	s := New(cfg, backoff.Policy{Base: 20 * time.Millisecond, Max: 50 * time.Millisecond}, slog.Default())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)
	s.Connect()
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

func startSup(t *testing.T, cfg Config, p backoff.Policy) (*Supervisor, *ptpiptest.Server) {
	t.Helper()
	srv, err := ptpiptest.Start(ptpiptest.Options{Dir: t.TempDir(), A6700: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	cfg.Host = srv.Addr()
	cfg.Linger = cmp.Or(cfg.Linger, time.Hour)
	s := New(cfg, p, slog.Default())
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go s.Run(ctx)
	return s, srv
}

func until(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for i := 0; i < 250; i++ {
		if ok() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("timeout waiting for " + what)
}

func TestQuickReconnect(t *testing.T) {
	// A long backoff proves the retry after an established session does not wait for it.
	s, srv := startSup(t, Config{}, backoff.Policy{Base: time.Hour, Max: time.Hour})
	s.Connect()
	until(t, "connect", func() bool { return s.Status().Connected })
	time.Sleep(minQuick + 100*time.Millisecond)
	t0 := time.Now()
	srv.DropConnections()
	until(t, "drop", func() bool { return s.Sessions() >= 1 && s.Restarts() >= 1 })
	until(t, "reconnect", func() bool { return s.Sessions() >= 2 && s.Status().Connected })
	if d := time.Since(t0); d > 4*time.Second {
		t.Fatalf("reconnect took %v", d)
	}
	st := s.Status()
	if st.Drops != 1 || st.LastDropAt.IsZero() || st.Reconnecting || !strings.Contains(st.LastDropReason, "closed the connection") {
		t.Fatalf("%+v", st)
	}
}

func TestReconnectingFlag(t *testing.T) {
	s, srv := startSup(t, Config{}, backoff.Policy{Base: time.Hour, Max: time.Hour})
	if s.Status().Reconnecting {
		t.Fatal("reconnecting before the first connect")
	}
	s.Connect()
	until(t, "connect", func() bool { return s.Status().Connected })
	srv.Close() // nobody answers anymore
	until(t, "reconnecting", func() bool { st := s.Status(); return !st.Connected && st.Reconnecting && st.Drops == 1 })
}

func TestExplain(t *testing.T) {
	closed := "The camera closed the connection (it does not keep the tunnel open permanently)."
	for _, c := range []struct {
		err  error
		want string
	}{
		{fmt.Errorf("ptpip: event channel: %w", io.EOF), closed},
		{errors.New("read tcp 1.2.3.4:5: read: connection reset by peer"), closed},
		{io.ErrUnexpectedEOF, closed},
		{ErrNotConnected, "The camera is not connected."},
		{context.DeadlineExceeded, "The camera stopped answering."},
		{errors.New("something else"), "something else"},
	} {
		if got := Explain(c.err); got != c.want {
			t.Errorf("%v: %q", c.err, got)
		}
	}
}

func TestKeepalive(t *testing.T) {
	s, srv := startSup(t, Config{Keepalive: 30 * time.Millisecond}, backoff.Policy{Base: 20 * time.Millisecond, Max: 50 * time.Millisecond})
	s.Connect()
	until(t, "connect", func() bool { return s.Status().Connected })
	s.Props() // the properties are only read once somebody wants them
	n := srv.Ops.Load()
	until(t, "idle polls", func() bool { return srv.Ops.Load() >= n+3 })
	// A broken connection ends the session; the keepalive must not hold on to it.
	srv.DropConnections()
	until(t, "second session", func() bool { return s.Sessions() >= 2 && s.Status().Connected })
}

func TestPropCache(t *testing.T) {
	s, _ := startSup(t, Config{}, backoff.Default) // no keepalive: only events refresh
	s.Connect()
	until(t, "connect", func() bool { return s.Status().Connected })
	until(t, "props", func() bool { ps, _ := s.Props(); return len(ps) >= 3 })
	ps, v := s.Props()
	cur := func(code uint16) int64 {
		ps, _ := s.Props()
		for _, p := range ps {
			if p.Code == code {
				return p.Current.Int
			}
		}
		return -1
	}
	for i := 1; i < len(ps); i++ {
		if ps[i-1].Code >= ps[i].Code {
			t.Fatal("not sorted")
		}
	}
	ch, stop := s.Subscribe()
	defer stop()
	c, _ := s.Client()
	if err := c.SonySetProp(context.Background(), 0x500A, 4, ptpip.SonyValue{Int: 0x8002}); err != nil {
		t.Fatal(err)
	}
	until(t, "refresh", func() bool { return cur(0x500A) == 0x8002 })
	if _, v2 := s.Props(); v2 <= v {
		t.Fatalf("version %d -> %d", v, v2)
	}
	for {
		select {
		case e := <-ch:
			if e.Props {
				return
			}
		case <-time.After(time.Second):
			t.Fatal("no props event")
		}
	}
}

func TestRunAloneNeverDials(t *testing.T) {
	_, srv := startSup(t, Config{}, backoff.Default)
	time.Sleep(200 * time.Millisecond)
	if n := srv.Ops.Load(); n != 0 {
		t.Fatalf("%d operations without anybody wanting the camera", n)
	}
}

func TestAcquireIdleDisconnect(t *testing.T) {
	s, _ := startSup(t, Config{Linger: 100 * time.Millisecond}, backoff.Default)
	c, release, err := s.Acquire(context.Background())
	if err != nil || c == nil {
		t.Fatal(c, err)
	}
	if st := s.Status(); !st.Connected || st.Connecting {
		t.Fatalf("%+v", st)
	}
	time.Sleep(1500 * time.Millisecond) // a lease keeps the session open
	if !s.Status().Connected {
		t.Fatal("closed while leased")
	}
	release()
	release() // idempotent
	until(t, "idle disconnect", func() bool { return !s.Status().Connected })
	if st := s.Status(); st.Drops != 0 || st.Reconnecting || st.Connecting || s.Failures() != 0 {
		t.Fatalf("%+v", st)
	}
	// the next use opens a new session
	if _, release, err = s.Acquire(context.Background()); err != nil || s.Sessions() != 2 {
		t.Fatal(err, s.Sessions())
	}
	release()
}

func TestNoPropsUntilWanted(t *testing.T) {
	s, srv := startSup(t, Config{Keepalive: 30 * time.Millisecond}, backoff.Default)
	_, release, err := s.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	time.Sleep(300 * time.Millisecond)
	n := srv.Ops.Load()
	if ps, _ := s.Props(); len(ps) != 0 { // this call is the first wish
		t.Fatal("properties read before they were wanted")
	}
	until(t, "props", func() bool { ps, _ := s.Props(); return len(ps) > 0 })
	until(t, "polls", func() bool { return srv.Ops.Load() >= n+3 })
}

func TestDisconnect(t *testing.T) {
	s, _ := startSup(t, Config{}, backoff.Default)
	_, release, err := s.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Disconnect(); err == nil || !strings.Contains(err.Error(), "busy") {
		t.Fatalf("want busy, got %v", err)
	}
	if !s.Status().Connected {
		t.Fatal("closed despite the lease")
	}
	release()
	if err := s.Disconnect(); err != nil {
		t.Fatal(err)
	}
	until(t, "disconnect", func() bool { return !s.Status().Connected })
	time.Sleep(300 * time.Millisecond) // and it stays closed
	if st := s.Status(); st.Connected || st.Connecting || st.Drops != 0 || s.Sessions() != 1 {
		t.Fatalf("%+v", st)
	}
}

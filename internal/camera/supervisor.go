// Package camera keeps one PTP/IP session to the camera alive.
package camera

import (
	"cmp"
	"context"
	"crypto/rand"
	"errors"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/knallli/alpha2go/discovery"
	"github.com/knallli/alpha2go/ptpip"
	"github.com/knallli/alpha2web/internal/backoff"
)

// ErrNotConnected means there is no session right now.
var ErrNotConnected = errors.New("camera not connected")

// Config says how to reach the camera. SSH is optional (Access Authentication).
type Config struct {
	Host    string // empty = find the camera on the local network
	MAC     string // optional, makes the scan faster (ARP lookup)
	SSH     *ptpip.SSHConfig
	DataDir string // remembers the last address and the initiator GUID

	// Discovery tunes the network scan (tests; zero values are fine).
	Discovery discovery.Options
}

// Status is a snapshot for the UI and the health endpoint.
type Status struct {
	Connected bool
	Model     string
	Serial    string
	Host      string
	Since     time.Time
	LastError string
}

// Supervisor dials the camera, reconnects with backoff and hands the live
// client to whoever asks.
type Supervisor struct {
	cfg     Config
	log     *slog.Logger
	restart backoff.Policy

	mu     sync.Mutex
	client *ptpip.Client
	st     Status
	subs   map[chan Event]struct{}
	cancel context.CancelFunc // ends the current session
	wake   chan struct{}      // interrupts the retry wait
}

// Event is what subscribers receive: session changes and raw camera events.
type Event struct {
	Connected   *bool // set on connect (true) and disconnect (false)
	ptpip.Event       // valid if Connected is nil
}

// Subscribe returns a channel of events and a function to stop. Slow
// subscribers lose events rather than block the camera.
func (s *Supervisor) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, 64)
	s.mu.Lock()
	if s.subs == nil {
		s.subs = map[chan Event]struct{}{}
	}
	s.subs[ch] = struct{}{}
	s.mu.Unlock()
	return ch, func() {
		s.mu.Lock()
		delete(s.subs, ch)
		s.mu.Unlock()
	}
}

func (s *Supervisor) publish(e Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for ch := range s.subs {
		select {
		case ch <- e:
		default:
		}
	}
}

func New(cfg Config, restart backoff.Policy, log *slog.Logger) *Supervisor {
	return &Supervisor{cfg: cfg, restart: restart, log: log, st: Status{Host: cfg.Host}, wake: make(chan struct{}, 1)}
}

// open connects to one address and opens the session.
func open(ctx context.Context, host string, opts ptpip.Options) (*ptpip.Client, *ptpip.DeviceInfo, error) {
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	opts.DialTimeout = 5 * time.Second
	c, err := ptpip.Dial(cctx, host, opts)
	if err != nil {
		return nil, nil, err
	}
	di, err := c.GetDeviceInfo(cctx)
	if err == nil {
		err = c.OpenSession(cctx, 1)
	}
	if err == nil && di.IsSony() {
		_, err = c.SonyHandshake(cctx)
	}
	if err != nil {
		c.Close()
		return nil, nil, err
	}
	return c, di, nil
}

// candidates returns the configured host, or else the last known address and
// whatever a network scan finds. With Access Authentication on, the camera
// only answers on its SSH port, so that is what the scan looks for.
func (s *Supervisor) candidates(ctx context.Context, cfg Config) ([]string, error) {
	if cfg.Host != "" {
		return []string{cfg.Host}, nil
	}
	var out []string
	seen := map[string]bool{}
	add := func(h string) {
		if host, _, err := net.SplitHostPort(h); err == nil && cfg.SSH != nil {
			h = host // the scanned port is SSH's; PTP/IP goes through the tunnel on its default port
		}
		if h != "" && !seen[h] {
			seen[h] = true
			out = append(out, h)
		}
	}
	if cfg.DataDir != "" {
		if b, err := os.ReadFile(filepath.Join(cfg.DataDir, "camera-host")); err == nil {
			add(string(b))
		}
	}
	opts := cfg.Discovery
	opts.MAC = cfg.MAC
	if cfg.SSH != nil && opts.Port == 0 {
		opts.Port = cmp.Or(cfg.SSH.Port, 22)
	}
	sctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	found := discovery.Find(sctx, opts)
	s.log.Debug("network scan", "found", found)
	for _, f := range found {
		add(f)
	}
	if len(out) == 0 {
		return nil, errors.New("no camera found on the network")
	}
	return out, nil
}

// guid returns a stable initiator identity (cameras remember paired hosts).
func guid(dir string) (g [16]byte) {
	if dir == "" {
		return
	}
	p := filepath.Join(dir, "ptpip-guid")
	if b, err := os.ReadFile(p); err == nil && len(b) == 16 {
		copy(g[:], b)
		return
	}
	rand.Read(g[:])
	os.MkdirAll(dir, 0o755)
	os.WriteFile(p, g[:], 0o600)
	return
}

// SetConfig switches to a new camera address; a running session is dropped
// and the supervisor reconnects at once.
func (s *Supervisor) SetConfig(cfg Config) {
	s.mu.Lock()
	s.cfg = cfg
	s.st.Host = cfg.Host
	cancel := s.cancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Status returns the current state.
func (s *Supervisor) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.st
}

// Client returns the live client; it is only valid until the session ends.
func (s *Supervisor) Client() (*ptpip.Client, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.client == nil {
		return nil, ErrNotConnected
	}
	return s.client, nil
}

// Run blocks until ctx is done.
func (s *Supervisor) Run(ctx context.Context) {
	fails := 0
	for ctx.Err() == nil {
		err := s.session(ctx)
		if ctx.Err() != nil {
			return
		}
		fails++
		s.log.Warn("camera session ended", "err", err)
		s.mu.Lock()
		s.st.LastError = err.Error()
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-s.wake:
			fails = 0
		case <-time.After(s.restart.Delay(fails)):
		}
	}
}

func (s *Supervisor) session(parent context.Context) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	s.mu.Lock()
	cfg := s.cfg
	s.cancel = cancel
	s.mu.Unlock()

	var d *ptpip.SSHDialer
	opts := ptpip.Options{Name: "alpha2web", GUID: guid(cfg.DataDir)}
	if cfg.SSH != nil {
		d = ptpip.NewSSHDialer(*cfg.SSH)
		defer d.Close()
		opts.DialContext = d.DialContext
	}
	hosts, err := s.candidates(ctx, cfg)
	if err != nil {
		return err
	}
	var c *ptpip.Client
	var di *ptpip.DeviceInfo
	for _, h := range hosts {
		if c, di, err = open(ctx, h, opts); err == nil {
			cfg.Host = h
			break
		}
		var ife *ptpip.InitFailError
		if errors.As(err, &ife) || errors.Is(err, ptpip.ErrSSHAuth) {
			return err // a camera answered but refused
		}
	}
	if c == nil {
		return err
	}
	defer c.Close()
	if cfg.DataDir != "" {
		os.WriteFile(filepath.Join(cfg.DataDir, "camera-host"), []byte(cfg.Host), 0o600)
	}
	s.mu.Lock()
	s.client = c
	s.st = Status{Connected: true, Model: di.Model, Serial: di.SerialNumber, Host: cfg.Host, Since: time.Now()}
	s.mu.Unlock()
	s.log.Info("camera connected", "model", di.Model)
	up, down := true, false
	s.publish(Event{Connected: &up})
	defer func() {
		s.mu.Lock()
		s.client = nil
		s.st.Connected = false
		s.mu.Unlock()
		s.publish(Event{Connected: &down})
	}()

	for {
		select {
		case <-ctx.Done():
			if parent.Err() == nil {
				return errors.New("camera settings changed")
			}
			return ctx.Err()
		case <-c.Done():
			return c.Err()
		case e := <-c.Events():
			s.publish(Event{Event: e})
		}
	}
}

// LiveView opens the camera's live-view stream.
func (s *Supervisor) LiveView(ctx context.Context) (*ptpip.SonyLiveView, error) {
	c, err := s.Client()
	if err != nil {
		return nil, err
	}
	return c.SonyOpenLiveView(ctx)
}

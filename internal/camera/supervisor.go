// Package camera keeps one PTP/IP session to the camera alive.
package camera

import (
	"cmp"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/knallli/alpha2go/discovery"
	"github.com/knallli/alpha2go/ptpip"
	"github.com/knallli/alpha2web/internal/backoff"
)

// ErrNotConnected means there is no session right now.
var ErrNotConnected = errors.New("camera not connected")

// DefaultWait is how long handlers wait for a reconnect before giving up.
const DefaultWait = 8 * time.Second

// minQuick is how long a session must have lived for an immediate retry; a
// camera that drops us right after the handshake gets the normal backoff.
const minQuick = time.Second

// Config says how to reach the camera. SSH is optional (Access Authentication).
type Config struct {
	Host    string // empty = find the camera on the local network
	MAC     string // optional, makes the scan faster (ARP lookup)
	SSH     *ptpip.SSHConfig
	DataDir string // remembers the last address and the initiator GUID

	// Keepalive is the interval of the idle property poll that keeps the
	// session busy and the property cache fresh; 0 disables it.
	Keepalive time.Duration

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

	Reconnecting   bool      // a session existed earlier in this process and there is none now
	Drops          int       // sessions that ended without being asked to
	LastDropAt     time.Time // zero if never dropped
	LastDropReason string    // human readable; LastError has the raw error
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
	up     chan struct{}      // closed on the next connect (WaitClient)

	props    ptpip.SonyProps
	sorted   []ptpip.SonyProp // snapshot of props, replaced (never modified) on change
	propsVer uint64

	sessions, failures, restarts atomic.Uint64
}

// Sessions counts successful connects.
func (s *Supervisor) Sessions() uint64 { return s.sessions.Load() }

// Failures counts attempts that ended without ever connecting.
func (s *Supervisor) Failures() uint64 { return s.failures.Load() }

// Restarts counts session endings that led to a new attempt.
func (s *Supervisor) Restarts() uint64 { return s.restarts.Load() }

// MAC returns the configured camera MAC address.
func (s *Supervisor) MAC() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg.MAC
}

// Event is what subscribers receive: session changes and raw camera events.
type Event struct {
	Connected   *bool // set on connect (true) and disconnect (false)
	Props       bool  // the property cache changed (see Props)
	ptpip.Event       // valid if Connected and Props are unset
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

// WaitClient is Client, but waits up to timeout for a reconnect.
func (s *Supervisor) WaitClient(ctx context.Context, timeout time.Duration) (*ptpip.Client, error) {
	t := time.NewTimer(timeout)
	defer t.Stop()
	for {
		s.mu.Lock()
		c := s.client
		if c == nil && s.up == nil {
			s.up = make(chan struct{})
		}
		up := s.up
		s.mu.Unlock()
		if c != nil {
			return c, nil
		}
		select {
		case <-up:
		case <-t.C:
			return nil, ErrNotConnected
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// Props returns the cached extended properties sorted by code and a version
// that grows with every change. The slice must not be modified.
func (s *Supervisor) Props() ([]ptpip.SonyProp, uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sorted, s.propsVer
}

// mergeProps folds a full or changed-only list into the cache.
func (s *Supervisor) mergeProps(ps []ptpip.SonyProp) {
	s.mu.Lock()
	first := s.props == nil
	if first {
		s.props = ptpip.SonyProps{}
	}
	changed := s.props.Update(ps)
	if len(changed) == 0 && !first {
		s.mu.Unlock()
		return
	}
	s.sorted = slices.SortedFunc(func(yield func(ptpip.SonyProp) bool) {
		for _, p := range s.props {
			if !yield(p) {
				return
			}
		}
	}, func(a, b ptpip.SonyProp) int { return int(a.Code) - int(b.Code) })
	s.propsVer++
	s.mu.Unlock()
	s.publish(Event{Props: true})
}

// maintain fills the property cache and, while the session is otherwise idle,
// polls changed properties every `every`. A broken connection ends the
// session by itself (c.Done); answers like "not supported" only end the poll.
// The context must not be given a timeout: a cancelled transaction tears the
// connection down, and a busy client is not a problem.
func (s *Supervisor) maintain(ctx context.Context, c *ptpip.Client, every time.Duration, nudge <-chan struct{}) {
	refresh := func(changedOnly bool) {
		ps, err := c.SonyGetProps(ctx, changedOnly)
		if err != nil {
			s.log.Debug("property poll", "err", err)
			return
		}
		s.mergeProps(ps)
	}
	refresh(false)
	var tick <-chan time.Time
	if every > 0 {
		t := time.NewTicker(every)
		defer t.Stop()
		tick = t.C
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.Done():
			return
		case <-tick:
		case <-nudge:
		}
		refresh(true)
	}
}

// Explain turns a camera error into text for people. Errors that mean "the
// camera closed the connection" get one explanation instead of a raw "EOF".
func Explain(err error) string {
	var ne net.Error
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrNotConnected):
		return "The camera is not connected."
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, net.ErrClosed), errors.Is(err, syscall.ECONNRESET), errors.Is(err, syscall.EPIPE),
		strings.HasSuffix(err.Error(), ": EOF"), strings.Contains(err.Error(), "connection reset"):
		return "The camera closed the connection (it does not keep the tunnel open permanently)."
	case errors.Is(err, os.ErrDeadlineExceeded), errors.Is(err, context.DeadlineExceeded), errors.As(err, &ne) && ne.Timeout():
		return "The camera stopped answering."
	}
	return err.Error()
}

var errSettings = errors.New("camera settings changed")

// Run blocks until ctx is done.
func (s *Supervisor) Run(ctx context.Context) {
	fails := 0
	for ctx.Err() == nil {
		before := s.sessions.Load()
		started := time.Now()
		err := s.session(ctx)
		if ctx.Err() != nil {
			return
		}
		s.restarts.Add(1)
		was := s.sessions.Load() != before
		if !was {
			s.failures.Add(1)
		}
		s.log.Warn("camera session ended", "err", err)
		s.mu.Lock()
		s.st.LastError = err.Error()
		if was && !errors.Is(err, errSettings) {
			s.st.Drops++
			s.st.LastDropAt = time.Now()
			s.st.LastDropReason = Explain(err)
		}
		s.mu.Unlock()
		if was && time.Since(started) >= minQuick {
			fails = 0 // a session that worked: try again at once, backoff only if that fails
			continue
		}
		fails++
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
	s.st.Connected, s.st.Reconnecting, s.st.Model, s.st.Serial, s.st.Host, s.st.Since = true, false, di.Model, di.SerialNumber, cfg.Host, time.Now()
	s.st.LastError = ""
	if s.up != nil {
		close(s.up)
		s.up = nil
	}
	s.mu.Unlock()
	s.sessions.Add(1)
	s.log.Info("camera connected", "model", di.Model)
	up, down := true, false
	s.publish(Event{Connected: &up})
	defer func() {
		s.mu.Lock()
		s.client = nil
		s.st.Connected, s.st.Reconnecting = false, true
		s.props, s.sorted = nil, nil
		s.propsVer++
		s.mu.Unlock()
		s.publish(Event{Connected: &down})
	}()

	nudge := make(chan struct{}, 1)
	if di.IsSony() {
		go s.maintain(ctx, c, cfg.Keepalive, nudge)
	}
	for {
		select {
		case <-ctx.Done():
			if parent.Err() == nil {
				return errSettings
			}
			return ctx.Err()
		case <-c.Done():
			return c.Err()
		case e := <-c.Events():
			s.publish(Event{Event: e})
			if e.Code == ptpip.EvSonyPropChanged {
				select {
				case nudge <- struct{}{}:
				default:
				}
			}
		}
	}
}

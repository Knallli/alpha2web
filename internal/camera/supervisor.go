// Package camera opens a PTP/IP session to the camera on demand and closes it
// when unused.
package camera

import (
	"cmp"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
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

// DefaultLinger is how long an unused session stays open.
const DefaultLinger = 5 * time.Minute

// never stands in for "no idle disconnect" (Linger < 0).
const never = 100 * 365 * 24 * time.Hour

// ConnectTimeout is how long Acquire waits for a session.
const ConnectTimeout = 60 * time.Second

// minQuick is how long a session must have lived for an immediate retry; a
// camera that drops us right after the handshake gets the normal backoff.
const minQuick = time.Second

// settle is the pause before an immediate retry: a camera that has just lost
// a session needs a moment before it takes a new one (it answers slowly or
// not at all otherwise).
var settle = 2 * time.Second

// Config says how to reach the camera. SSH is optional (Access Authentication).
type Config struct {
	Host    string // empty = find the camera on the local network
	MAC     string // optional, makes the scan faster (ARP lookup)
	SSH     *ptpip.SSHConfig
	DataDir string // remembers the last address and the initiator GUID

	// Keepalive is the interval of the idle property poll that keeps the
	// session busy and the property cache fresh. 0 only reads the properties
	// once after connecting; negative leaves the camera completely alone.
	Keepalive time.Duration

	// Linger is how long an unused session stays open; 0 = DefaultLinger, < 0 = until Disconnect.
	Linger time.Duration

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

	Connecting     bool      // a session is wanted and there is none yet (attempt pending or about to start)
	Reconnecting   bool      // a session dropped while wanted and there is none yet
	Drops          int       // sessions that ended without being asked to
	LastDropAt     time.Time // zero if never dropped
	LastDropReason string    // human readable; LastError has the raw error
}

// Supervisor opens a PTP/IP session to the camera on demand and closes it when
// unused. Users hold a lease (Acquire) while they work; the Connect button
// keeps the session wanted for the linger time.
type Supervisor struct {
	cfg     Config
	log     *slog.Logger
	restart backoff.Policy

	mu      sync.Mutex
	client  *ptpip.Client
	st      Status
	subs    map[chan Event]struct{}
	cancel  context.CancelFunc // ends the current session
	wake    chan struct{}      // Run re-checks wanted
	cfgWake chan struct{}      // SetConfig: ends the settle/backoff wait
	up      chan struct{}      // closed on the next connect (Acquire)
	tried   chan struct{}      // closed (and replaced) when an attempt ends without a session
	tryErr  error              // error of that attempt
	stop    error              // why cancel was called (errSettings, errDisconnect)
	card    chan struct{}      // held during one card operation, see LockCard

	users     int       // active leases
	lastUsed  time.Time // last release
	holdUntil time.Time // Connect/Extend: keep the session wanted until then

	propsWanted bool          // per session: someone needs the properties
	propsCh     chan struct{} // closed when propsWanted is set

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
	return &Supervisor{cfg: cfg, restart: restart, log: log, st: Status{Host: cfg.Host},
		wake: make(chan struct{}, 1), cfgWake: make(chan struct{}, 1), tried: make(chan struct{}), card: make(chan struct{}, 1)}
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
	if cancel != nil {
		s.stop = errSettings
	}
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	poke(s.wake)
	poke(s.cfgWake)
}

func poke(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

func (s *Supervisor) lingerLocked() time.Duration {
	if s.cfg.Linger < 0 {
		return never
	}
	return cmp.Or(s.cfg.Linger, DefaultLinger)
}

// wantedLocked says whether anything needs the camera: a lease or a pressed Connect.
func (s *Supervisor) wantedLocked() bool { return s.users > 0 || time.Now().Before(s.holdUntil) }

// Connect is the Connect button: the session is wanted for the linger time.
// It does not wait for the connection.
func (s *Supervisor) Connect() {
	s.mu.Lock()
	s.holdUntil = time.Now().Add(s.lingerLocked())
	s.mu.Unlock()
	poke(s.wake)
}

// Extend keeps an open (or opening) session for another linger time; it never
// opens one. Pages that poll while open call it.
func (s *Supervisor) Extend() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.client != nil || s.wantedLocked() {
		if t := time.Now().Add(s.lingerLocked()); t.After(s.holdUntil) {
			s.holdUntil = t
		}
	}
}

// Disconnect closes the session now instead of after the linger time; it
// refuses while operations are running.
func (s *Supervisor) Disconnect() error {
	s.mu.Lock()
	s.holdUntil = time.Time{}
	if s.users > 0 {
		n := s.users
		s.mu.Unlock()
		return fmt.Errorf("camera busy: %d operations running", n)
	}
	cancel := s.cancel
	if cancel != nil {
		s.stop = errDisconnect
	}
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return nil
}

// Acquire returns the client, opening the session if needed (it waits up to
// ConnectTimeout, or returns the error of a failed attempt). Call release when
// done; the session closes after the linger time. The client is only valid
// while the session lasts.
func (s *Supervisor) Acquire(ctx context.Context) (*ptpip.Client, func(), error) {
	s.mu.Lock()
	s.users++
	tried := s.tried
	s.mu.Unlock()
	poke(s.wake)
	t := time.NewTimer(ConnectTimeout)
	defer t.Stop()
	for {
		s.mu.Lock()
		c := s.client
		if c == nil && s.up == nil {
			s.up = make(chan struct{})
		}
		up := s.up
		s.mu.Unlock()
		var err error
		if c != nil {
			var once sync.Once
			return c, func() {
				once.Do(func() {
					s.mu.Lock()
					s.users--
					s.lastUsed = time.Now()
					s.mu.Unlock()
				})
			}, nil
		}
		select {
		case <-up:
			continue
		case <-tried:
			s.mu.Lock()
			err = s.tryErr
			s.mu.Unlock()
		case <-t.C:
			err = ErrNotConnected
		case <-ctx.Done():
			err = ctx.Err()
		}
		s.mu.Lock()
		s.users--
		s.mu.Unlock()
		return nil, nil, err
	}
}

// LockCard waits until no other card operation (listing, thumbnail, file
// transfer, delete) runs and returns the unlock function: the camera answers
// DeviceBusy to overlapping ones, e.g. a gallery pull during a sync.
func (s *Supervisor) LockCard(ctx context.Context) (func(), error) {
	select {
	case s.card <- struct{}{}:
		var once sync.Once
		return func() { once.Do(func() { <-s.card }) }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Status returns the current state.
func (s *Supervisor) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.st
	st.Connecting = s.client == nil && s.wantedLocked()
	return st
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

// Props returns the cached extended properties sorted by code and a version
// that grows with every change. The slice must not be modified. Calling it
// makes the supervisor read the properties of the current session (only the
// Control page needs them).
func (s *Supervisor) Props() ([]ptpip.SonyProp, uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.client != nil && !s.propsWanted {
		s.propsWanted = true
		close(s.propsCh)
	}
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

// trace logs every camera operation at debug level (LOG_LEVEL=debug), and slow
// or failed ones as warnings, to find out which request makes a camera stall.
func (s *Supervisor) trace(e ptpip.TraceEvent) {
	args := []any{"op", ptpip.OpName(e.Op), "code", fmt.Sprintf("0x%04X", e.Op), "params", e.Params, "phase", e.Phase}
	switch {
	case e.Phase == "end" && (e.Err != nil || e.Dur > 3*time.Second):
		s.log.Warn("ptp op", append(args, "took", e.Dur.Round(time.Millisecond), "err", e.Err)...)
	case e.Phase == "queued" || e.Dur == 0:
		s.log.Debug("ptp op", args...)
	default:
		s.log.Debug("ptp op", append(args, "took", e.Dur.Round(time.Millisecond))...)
	}
}

// maintain waits until the properties are wanted (Props), fills the property
// cache and, while the session is otherwise idle, polls changed properties
// every `every`. A broken connection ends the
// session by itself (c.Done); answers like "not supported" only end the poll.
// The context must not be given a timeout: a cancelled transaction tears the
// connection down, and a busy client is not a problem.
func (s *Supervisor) maintain(ctx context.Context, c *ptpip.Client, every time.Duration, wanted, nudge <-chan struct{}) {
	if every < 0 { // CAMERA_KEEPALIVE=0: no traffic of our own after the handshake
		return
	}
	refresh := func(changedOnly bool) {
		ps, err := c.SonyGetProps(ctx, changedOnly)
		if err != nil {
			s.log.Debug("property poll", "err", err)
			return
		}
		s.mergeProps(ps)
	}
	select { // a gallery refresh must not cause property traffic
	case <-ctx.Done():
		return
	case <-c.Done():
		return
	case <-wanted:
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

var (
	errSettings   = errors.New("camera settings changed")
	errDisconnect = errors.New("camera disconnected on request")
	errIdle       = errors.New("camera unused")
)

// await blocks until something wants the camera; false means ctx is done.
func (s *Supervisor) await(ctx context.Context) bool {
	for {
		select { // drain first: the check below sees every change made before it
		case <-s.wake:
		default:
		}
		s.mu.Lock()
		w := s.wantedLocked()
		s.mu.Unlock()
		if w {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-s.wake:
		}
	}
}

// pause waits d; SetConfig ends it early. False means ctx is done.
func (s *Supervisor) pause(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-s.cfgWake:
	case <-time.After(d):
	}
	return true
}

// Run blocks until ctx is done. It opens a session only while something wants
// the camera and goes back to waiting when the session ends for good.
func (s *Supervisor) Run(ctx context.Context) {
	fails := 0
	for ctx.Err() == nil {
		if !s.await(ctx) {
			return
		}
		before := s.sessions.Load()
		started := time.Now()
		err := s.session(ctx)
		if ctx.Err() != nil {
			return
		}
		was := s.sessions.Load() != before
		switch {
		case errors.Is(err, errIdle):
			s.log.Info("camera disconnected (idle)")
			fails = 0
			s.mu.Lock()
			s.st.Reconnecting = false
			s.mu.Unlock()
		case errors.Is(err, errSettings), errors.Is(err, errDisconnect):
			s.log.Info("camera session closed", "reason", err)
			fails = 0
		case !was:
			s.failures.Add(1)
			s.log.Warn("camera connect failed", "err", err)
			s.mu.Lock()
			s.st.LastError = err.Error()
			s.holdUntil = time.Time{} // a failed button press must not retry forever
			s.tryErr = err
			close(s.tried)
			s.tried = make(chan struct{})
			again := s.users > 0
			s.st.Reconnecting = s.st.Reconnecting && again
			s.mu.Unlock()
			if !again {
				fails = 0
				continue
			}
			fails++
			s.restarts.Add(1)
			if !s.pause(ctx, s.restart.Delay(fails)) {
				return
			}
		default:
			s.log.Warn("camera session ended", "err", err)
			s.mu.Lock()
			s.st.LastError = err.Error()
			s.st.Drops++
			s.st.LastDropAt = time.Now()
			s.st.LastDropReason = Explain(err)
			again := s.wantedLocked()
			s.st.Reconnecting = again
			s.mu.Unlock()
			if !again {
				fails = 0
				continue
			}
			s.restarts.Add(1)
			d := settle
			if time.Since(started) < minQuick {
				fails++ // dropped right after the handshake: normal backoff
				d = s.restart.Delay(fails)
			} else {
				fails = 0
			}
			if !s.pause(ctx, d) {
				return
			}
			s.mu.Lock()
			s.st.Reconnecting = s.wantedLocked() // gave up while waiting
			s.mu.Unlock()
		}
	}
}

func (s *Supervisor) session(parent context.Context) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	s.mu.Lock()
	cfg := s.cfg
	s.cancel, s.stop = cancel, nil
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.cancel = nil
		s.mu.Unlock()
	}()
	// a cancel from SetConfig/Disconnect during the attempt is not a failure
	ended := func(err error) error {
		s.mu.Lock()
		defer s.mu.Unlock()
		if ctx.Err() != nil && parent.Err() == nil && s.stop != nil {
			return s.stop
		}
		return err
	}

	var d *ptpip.SSHDialer
	opts := ptpip.Options{Name: "alpha2web", GUID: guid(cfg.DataDir), Trace: s.trace}
	if cfg.SSH != nil {
		d = ptpip.NewSSHDialer(*cfg.SSH)
		defer d.Close()
		opts.DialContext = d.DialContext
	}
	hosts, err := s.candidates(ctx, cfg)
	if err != nil {
		return ended(err)
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
		return ended(err)
	}
	defer func() {
		// End the PTP session properly when we can: a camera that only sees the
		// TCP connection vanish stays unresponsive for a while.
		select {
		case <-c.Done():
		default:
			cctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			c.CloseSession(cctx)
			cancel()
		}
		c.Close()
	}()
	if cfg.DataDir != "" {
		os.WriteFile(filepath.Join(cfg.DataDir, "camera-host"), []byte(cfg.Host), 0o600)
	}
	s.mu.Lock()
	s.client = c
	s.lastUsed = time.Now()
	s.propsWanted, s.propsCh = false, make(chan struct{})
	propsCh := s.propsCh
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
		s.st.Connected = false
		s.propsWanted = false
		s.props, s.sorted = nil, nil
		s.propsVer++
		s.mu.Unlock()
		s.publish(Event{Connected: &down})
	}()

	nudge := make(chan struct{}, 1)
	if di.IsSony() {
		go s.maintain(ctx, c, cfg.Keepalive, propsCh, nudge)
	}
	idle := time.NewTicker(time.Second)
	defer idle.Stop()
	for {
		select {
		case <-ctx.Done():
			if parent.Err() == nil {
				return ended(ctx.Err())
			}
			return ctx.Err()
		case <-idle.C:
			s.mu.Lock()
			done := s.users == 0 && !time.Now().Before(s.lastUsed.Add(s.lingerLocked())) && !time.Now().Before(s.holdUntil)
			if done {
				s.client = nil // Acquire from now on waits for the next session
			}
			s.mu.Unlock()
			if done {
				return errIdle
			}
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

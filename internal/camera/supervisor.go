// Package camera keeps one PTP/IP session to the camera alive.
package camera

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/knallli/alpha2go/ptpip"
	"github.com/knallli/alpha2web/internal/backoff"
)

// ErrNotConnected means there is no session right now.
var ErrNotConnected = errors.New("camera not connected")

// Config says how to reach the camera. SSH is optional (Access Authentication).
type Config struct {
	Host string
	SSH  *ptpip.SSHConfig
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
}

func New(cfg Config, restart backoff.Policy, log *slog.Logger) *Supervisor {
	return &Supervisor{cfg: cfg, restart: restart, log: log, st: Status{Host: cfg.Host}}
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
		case <-time.After(s.restart.Delay(fails)):
		}
	}
}

func (s *Supervisor) session(ctx context.Context) error {
	opts := ptpip.Options{Name: "alpha2web"}
	if s.cfg.SSH != nil {
		d := ptpip.NewSSHDialer(*s.cfg.SSH)
		defer d.Close()
		opts.DialContext = d.DialContext
	}
	c, err := ptpip.Dial(ctx, s.cfg.Host, opts)
	if err != nil {
		return err
	}
	defer c.Close()
	di, err := c.GetDeviceInfo(ctx)
	if err != nil {
		return err
	}
	if err := c.OpenSession(ctx, 1); err != nil {
		return err
	}
	if di.IsSony() {
		if _, err := c.SonyHandshake(ctx); err != nil {
			return err
		}
	}
	s.mu.Lock()
	s.client = c
	s.st = Status{Connected: true, Model: di.Model, Serial: di.SerialNumber, Host: s.cfg.Host, Since: time.Now()}
	s.mu.Unlock()
	s.log.Info("camera connected", "model", di.Model)
	defer func() {
		s.mu.Lock()
		s.client = nil
		s.st.Connected = false
		s.mu.Unlock()
	}()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.Done():
		return c.Err()
	}
}

// Package logbuf keeps the most recent log records in memory for the web UI,
// as an slog.Handler that wraps the normal stdout handler.
package logbuf

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// Entry is one log record.
type Entry struct {
	Seq   uint64    `json:"seq"`
	Time  time.Time `json:"time"`
	Level string    `json:"level"`
	Msg   string    `json:"msg"`
	Attrs string    `json:"attrs"`
}

// Ring is a fixed-size buffer of recent entries.
type Ring struct {
	mu   sync.Mutex
	buf  []Entry
	next uint64
}

// NewRing keeps the last n entries.
func NewRing(n int) *Ring { return &Ring{buf: make([]Entry, 0, n)} }

func (r *Ring) add(e Entry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.next++
	e.Seq = r.next
	if len(r.buf) < cap(r.buf) {
		r.buf = append(r.buf, e)
		return
	}
	copy(r.buf, r.buf[1:])
	r.buf[len(r.buf)-1] = e
}

// Since returns entries with Seq > seq at or above minLevel, oldest first.
func (r *Ring) Since(seq uint64, minLevel slog.Level) []Entry {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := []Entry{}
	for _, e := range r.buf {
		var l slog.Level
		l.UnmarshalText([]byte(e.Level))
		if e.Seq > seq && l >= minLevel {
			out = append(out, e)
		}
	}
	return out
}

// Handler tees records into a Ring.
type Handler struct {
	inner  slog.Handler
	ring   *Ring
	attrs  []slog.Attr
	prefix string
}

// NewHandler wraps inner.
func NewHandler(inner slog.Handler, ring *Ring) *Handler { return &Handler{inner: inner, ring: ring} }

// Enabled implements slog.Handler.
func (h *Handler) Enabled(ctx context.Context, l slog.Level) bool { return h.inner.Enabled(ctx, l) }

// Handle implements slog.Handler.
func (h *Handler) Handle(ctx context.Context, r slog.Record) error {
	var b strings.Builder
	write := func(a slog.Attr) {
		if a.Equal(slog.Attr{}) {
			return
		}
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		v := a.Value.Resolve().String()
		if strings.ContainsAny(v, " \"=") {
			v = fmt.Sprintf("%q", v)
		}
		b.WriteString(h.prefix + a.Key + "=" + v)
	}
	for _, a := range h.attrs {
		write(a)
	}
	r.Attrs(func(a slog.Attr) bool { write(a); return true })
	h.ring.add(Entry{Time: r.Time, Level: r.Level.String(), Msg: r.Message, Attrs: b.String()})
	return h.inner.Handle(ctx, r)
}

// WithAttrs implements slog.Handler.
func (h *Handler) WithAttrs(as []slog.Attr) slog.Handler {
	return &Handler{inner: h.inner.WithAttrs(as), ring: h.ring, attrs: append(append([]slog.Attr{}, h.attrs...), as...), prefix: h.prefix}
}

// WithGroup implements slog.Handler.
func (h *Handler) WithGroup(name string) slog.Handler {
	return &Handler{inner: h.inner.WithGroup(name), ring: h.ring, attrs: h.attrs, prefix: h.prefix + name + "."}
}

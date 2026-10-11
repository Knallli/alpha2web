package webui

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/knallli/alpha2go/ptpip"
	"github.com/knallli/alpha2web/internal/camera"
)

// liveGiveUp is how long a stream waits for the camera before it ends.
const liveGiveUp = 60 * time.Second

// liveState is what the one live-view stream reports to /liveview/status.
type liveState struct {
	mu       sync.Mutex
	open     bool
	frames   uint64
	fps      float64
	err      string
	winStart time.Time
	winN     int
}

func (l *liveState) begin() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.open {
		return false
	}
	l.open, l.frames, l.fps, l.err, l.winStart, l.winN = true, 0, 0, "", time.Time{}, 0
	return true
}

func (l *liveState) end() {
	l.mu.Lock()
	l.open, l.fps = false, 0
	l.mu.Unlock()
}

func (l *liveState) frame() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.frames++
	l.err = ""
	now := time.Now()
	if l.winStart.IsZero() {
		l.winStart = now
	}
	if l.winN++; now.Sub(l.winStart) >= time.Second {
		l.fps = float64(l.winN) / now.Sub(l.winStart).Seconds()
		l.winStart, l.winN = now, 0
	}
}

func (l *liveState) fail(err error) {
	l.mu.Lock()
	l.err = camera.Explain(err)
	l.mu.Unlock()
}

// streamMJPEG sends frames from next as multipart/x-mixed-replace until next
// fails or the client goes away. It returns how many frames went out.
func streamMJPEG(w http.ResponseWriter, r *http.Request, next func() ([]byte, error)) (int, error) {
	rc := http.NewResponseController(w)
	n := 0
	for r.Context().Err() == nil {
		jpeg, err := next()
		if err != nil {
			return n, err
		}
		if n == 0 {
			// headers wait for the first frame so a failure can still be an HTTP error
			w.Header().Set("Content-Type", "multipart/x-mixed-replace; boundary=frame")
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("X-Accel-Buffering", "no")
		}
		fmt.Fprintf(w, "--frame\r\nContent-Type: image/jpeg\r\nContent-Length: %d\r\n\r\n", len(jpeg))
		w.Write(jpeg)
		w.Write([]byte("\r\n"))
		rc.Flush()
		n++
	}
	return n, r.Context().Err()
}

// openLive opens the stream on c. The PTP transaction that reads the URL must
// not be cancelled with the browser request (that would break the camera
// session), so the stream gets its own context, which ends with the returned
// cancel func.
func (h *Handler) openLive(r *http.Request, c *ptpip.Client) (*ptpip.SonyLiveView, context.CancelFunc, error) {
	sctx, cancel := context.WithCancel(context.WithoutCancel(r.Context()))
	t := time.AfterFunc(20*time.Second, cancel)
	lv, err := c.SonyOpenLiveView(sctx)
	t.Stop()
	if err != nil {
		cancel()
		return nil, nil, err
	}
	return lv, cancel, nil
}

func (h *Handler) liveStream(w http.ResponseWriter, r *http.Request) {
	if !h.live.begin() {
		http.Error(w, "live view already open in another tab", http.StatusConflict)
		return
	}
	defer h.live.end()
	// one lease for the whole stream: the session stays open, and is reopened after a drop
	c, done, err := h.use(r)
	if err != nil {
		http.Error(w, camera.Explain(err), http.StatusServiceUnavailable)
		return
	}
	defer done()
	log := h.Log()
	var (
		lv     *ptpip.SonyLiveView
		cancel context.CancelFunc
		stop   func() bool
		n      int // frames of the current stream
		bad    time.Time
	)
	closeStream := func(err error) {
		stop()
		lv.Close()
		cancel()
		log.Info("live view closed", "frames", n, "err", err)
		lv, c = nil, nil // c may belong to a dead session; fetch the current one
	}
	defer func() {
		if lv != nil {
			closeStream(r.Context().Err())
		}
	}()
	fail := func(what string, err error) error {
		if bad.IsZero() {
			bad = time.Now()
		}
		h.live.fail(err)
		log.Warn("live view "+what, "err", err)
		if time.Since(bad) > liveGiveUp {
			return err
		}
		select {
		case <-r.Context().Done():
		case <-time.After(time.Second):
		}
		return nil
	}
	sent, err := streamMJPEG(w, r, func() ([]byte, error) {
		for r.Context().Err() == nil {
			if lv == nil {
				var err error
				if c == nil {
					c, err = h.Camera.Client()
				}
				if err == nil {
					lv, cancel, err = h.openLive(r, c)
				}
				if err != nil {
					c = nil
					if err = fail("open failed", err); err != nil {
						return nil, err
					}
					continue
				}
				stop = context.AfterFunc(r.Context(), cancel) // unblocks a pending read
				n = 0
				log.Info("live view opened")
			}
			f, err := lv.Next()
			if err != nil {
				if r.Context().Err() != nil {
					break
				}
				closeStream(err)
				if err = fail("stream failed", err); err != nil {
					return nil, err
				}
				continue
			}
			if len(f.JPEG) == 0 {
				continue
			}
			n++
			bad = time.Time{}
			h.live.frame()
			return f.JPEG, nil
		}
		return nil, r.Context().Err()
	})
	if sent == 0 && err != nil && r.Context().Err() == nil {
		http.Error(w, camera.Explain(err), http.StatusServiceUnavailable)
	}
}

func (h *Handler) liveStatus(w http.ResponseWriter, _ *http.Request) {
	st := h.Camera.Status()
	l := &h.live
	l.mu.Lock()
	out := map[string]any{
		"connected": st.Connected, "connecting": st.Connecting, "reconnecting": st.Reconnecting, "error": l.err,
		"frames": l.frames, "fps": l.fps, "drops": st.Drops, "lastDropReason": st.LastDropReason,
	}
	l.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, out)
}

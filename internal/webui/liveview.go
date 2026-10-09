package webui

import (
	"fmt"
	"net/http"
)

// streamMJPEG sends frames from next as multipart/x-mixed-replace until next
// fails or the client goes away.
func streamMJPEG(w http.ResponseWriter, r *http.Request, next func() ([]byte, error)) {
	w.Header().Set("Content-Type", "multipart/x-mixed-replace; boundary=frame")
	w.Header().Set("Cache-Control", "no-store")
	fl, _ := w.(http.Flusher)
	for r.Context().Err() == nil {
		jpeg, err := next()
		if err != nil {
			return
		}
		fmt.Fprintf(w, "--frame\r\nContent-Type: image/jpeg\r\nContent-Length: %d\r\n\r\n", len(jpeg))
		w.Write(jpeg)
		w.Write([]byte("\r\n"))
		if fl != nil {
			fl.Flush()
		}
	}
}

func (h *Handler) liveStream(w http.ResponseWriter, r *http.Request) {
	lv, err := h.Camera.LiveView(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	defer lv.Close()
	streamMJPEG(w, r, func() ([]byte, error) {
		f, err := lv.Next()
		return f.JPEG, err
	})
}

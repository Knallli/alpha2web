package webui

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestStreamMJPEG(t *testing.T) {
	frames := [][]byte{{0xFF, 0xD8, 1}, {0xFF, 0xD8, 2}}
	w := httptest.NewRecorder()
	streamMJPEG(w, httptest.NewRequest("GET", "/", nil), func() ([]byte, error) {
		if len(frames) == 0 {
			return nil, io.EOF
		}
		f := frames[0]
		frames = frames[1:]
		return f, nil
	})
	if n := strings.Count(w.Body.String(), "--frame"); n != 2 {
		t.Fatalf("frames: %d", n)
	}
	if !strings.HasPrefix(w.Header().Get("Content-Type"), "multipart/x-mixed-replace") {
		t.Fatal("content type")
	}
}

package settings

import (
	"path/filepath"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", "s.json")
	s, err := Open(p)
	if err != nil || s.Get().CameraHost != "" {
		t.Fatal(err)
	}
	if err := s.Set(Values{CameraHost: "h", Plugins: map[string]map[string]string{"x": {"k": "v"}}}); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(p)
	if err != nil || s2.Get().CameraHost != "h" || s2.Get().Plugins["x"]["k"] != "v" {
		t.Fatalf("%v %+v", err, s2.Get())
	}
}

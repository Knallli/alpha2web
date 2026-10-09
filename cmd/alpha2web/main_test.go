package main

import (
	"testing"

	"github.com/knallli/alpha2web/internal/settings"
)

func TestEffective(t *testing.T) {
	v := settings.Values{CameraHost: "saved", CameraUser: "u", CameraPassword: "p"}
	if c := effective(v, ""); c.Host != "saved" || c.SSH == nil || c.SSH.Password != "p" {
		t.Fatalf("%+v", c)
	}
	t.Setenv("CAMERA_HOST", "env")
	if c := effective(v, ""); c.Host != "env" || !locked("host") {
		t.Fatalf("%+v", c)
	}
	if c := effective(settings.Values{}, ""); c.SSH != nil {
		t.Fatal("no user: no ssh")
	}
}

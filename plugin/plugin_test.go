package plugin

import (
	"context"
	"log/slog"
	"testing"
)

type fake struct{}

func (fake) Name() string                                    { return "fake" }
func (fake) Run(context.Context, Camera, *slog.Logger) error { return nil }

func TestRegister(t *testing.T) {
	Register(fake{})
	if len(All()) != 1 {
		t.Fatal("want 1 plugin")
	}
	defer func() {
		if recover() == nil {
			t.Fatal("duplicate should panic")
		}
	}()
	Register(fake{})
}

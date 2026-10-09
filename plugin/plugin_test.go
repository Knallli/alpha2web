package plugin

import (
	"context"
	"log/slog"
	"testing"
)

type fake struct{}

func (fake) Name() string { return "fake" }

var ran = make(chan struct{}, 1)

func (fake) Run(context.Context, Camera, *slog.Logger) error { ran <- struct{}{}; return nil }

func TestRegister(t *testing.T) {
	Register(fake{})
	if len(All()) != 1 {
		t.Fatal("want 1 plugin")
	}
	RunAll(context.Background(), nil, slog.Default())
	select {
	case <-ran:
	default:
		t.Fatal("plugin did not run")
	}
	defer func() {
		if recover() == nil {
			t.Fatal("duplicate should panic")
		}
	}()
	Register(fake{})
}

package plugin

import (
	"context"
	"log/slog"
	"testing"
)

type fake struct{ got chan string }

func (fake) Name() string    { return "fake" }
func (fake) Fields() []Field { return nil }
func (f fake) Run(_ context.Context, env Env) error {
	f.got <- env.Config()["k"]
	return nil
}

func TestRegisterAndRun(t *testing.T) {
	f := fake{make(chan string, 1)}
	Register(f)
	if len(All()) != 1 {
		t.Fatal("want 1 plugin")
	}
	RunAll(context.Background(), Env{Log: slog.Default()}, func(name string) map[string]string {
		return map[string]string{"k": name}
	})
	if v := <-f.got; v != "fake" {
		t.Fatalf("config %q", v)
	}
	defer func() {
		if recover() == nil {
			t.Fatal("duplicate should panic")
		}
	}()
	Register(f)
}

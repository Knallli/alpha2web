package camera

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/knallli/alpha2go/ptpip/ptpiptest"
	"github.com/knallli/alpha2web/internal/backoff"
)

func TestReconnect(t *testing.T) {
	srv, err := ptpiptest.Start(ptpiptest.Options{Dir: t.TempDir(), A6700: true})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	s := New(Config{Host: srv.Addr()}, backoff.Policy{Base: 20 * time.Millisecond, Max: 50 * time.Millisecond}, slog.Default())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	wait := func(want bool) {
		t.Helper()
		for i := 0; i < 200; i++ {
			if s.Status().Connected == want {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("connected never became %v", want)
	}
	wait(true)
	if m := s.Status().Model; m != "ILCE-6700" {
		t.Fatalf("model %q", m)
	}
	if _, err := s.Client(); err != nil {
		t.Fatal(err)
	}
	srv.DropConnections()
	wait(false)
	wait(true)
}

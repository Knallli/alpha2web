package logbuf

import (
	"io"
	"log/slog"
	"testing"
)

func TestRingAndHandler(t *testing.T) {
	ring := NewRing(3)
	log := slog.New(NewHandler(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug}), ring))
	log.Debug("d")
	log.With("file", "a b.JPG").Info("uploaded", "id", 7)
	log.Warn("w")
	log.Error("e")
	all := ring.Since(0, slog.LevelDebug)
	if len(all) != 3 || all[0].Msg != "uploaded" || all[0].Attrs != `file="a b.JPG" id=7` {
		t.Fatalf("entries %+v", all)
	}
	if warn := ring.Since(0, slog.LevelWarn); len(warn) != 2 {
		t.Fatalf("level filter: %+v", warn)
	}
	if after := ring.Since(all[1].Seq, slog.LevelDebug); len(after) != 1 || after[0].Msg != "e" {
		t.Fatalf("since: %+v", after)
	}
}

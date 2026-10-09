package backoff

import (
	"testing"
	"time"
)

func TestDelayExponentialAndCapped(t *testing.T) {
	p := Policy{Base: time.Second, Max: 10 * time.Second}
	want := []time.Duration{0, time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 10 * time.Second, 10 * time.Second}
	for attempt, w := range want {
		if got := p.Delay(attempt); got != w {
			t.Errorf("Delay(%d) = %v, want %v", attempt, got, w)
		}
	}
	if got := p.Delay(1000); got != 10*time.Second {
		t.Errorf("Delay(1000) = %v, want cap", got)
	}
}

func TestDelayJitterBounds(t *testing.T) {
	p := Policy{Base: 10 * time.Second, Max: time.Minute, Jitter: 0.2}
	for i := 0; i < 1000; i++ {
		d := p.Delay(1)
		if d < 8*time.Second || d > 12*time.Second {
			t.Fatalf("jittered delay %v out of bounds", d)
		}
	}
}

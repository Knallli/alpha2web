// Package backoff computes capped exponential retry delays with jitter.
package backoff

import (
	"math/rand/v2"
	"time"
)

// Policy describes an exponential backoff schedule.
type Policy struct {
	Base   time.Duration // delay after the first failure
	Max    time.Duration // upper bound for any delay
	Jitter float64       // fraction (0..1) of random spread applied to each delay
}

// Default is used for camera and Immich retries unless configured otherwise.
var Default = Policy{Base: 5 * time.Second, Max: 15 * time.Minute, Jitter: 0.2}

// Delay returns the wait time after the given number of consecutive failures
// (attempt >= 1). Attempt values <= 0 yield zero.
func (p Policy) Delay(attempt int) time.Duration {
	if attempt <= 0 {
		return 0
	}
	d := p.Base
	for i := 1; i < attempt && d < p.Max; i++ {
		d *= 2
	}
	if p.Max > 0 && d > p.Max {
		d = p.Max
	}
	if p.Jitter > 0 {
		spread := float64(d) * p.Jitter
		d = time.Duration(float64(d) - spread + rand.Float64()*2*spread)
	}
	if d < 0 {
		d = 0
	}
	return d
}

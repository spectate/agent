// Package backoff implements capped exponential backoff with jitter.
package backoff

import (
	"math/rand/v2"
	"time"
)

// Backoff yields growing, jittered delays: attempt n waits a random duration in
// [d/2, d] where d = min(Max, Base*2^n) ("equal jitter"), so a fleet that lost
// the API at the same moment does not reconnect in lockstep.
type Backoff struct {
	Base    time.Duration
	Max     time.Duration
	attempt int
	rand    func() float64
}

// New returns a backoff starting at base and capped at max.
func New(base, max time.Duration) *Backoff {
	return &Backoff{Base: base, Max: max, rand: rand.Float64}
}

// Next returns the delay before the next attempt and advances the attempt counter.
func (b *Backoff) Next() time.Duration {
	d := b.Base
	for i := 0; i < b.attempt && d < b.Max; i++ {
		d *= 2
	}
	if d > b.Max {
		d = b.Max
	}
	b.attempt++
	half := d / 2
	return half + time.Duration(b.rand()*float64(d-half))
}

// Attempts returns the number of consecutive failures recorded.
func (b *Backoff) Attempts() int { return b.attempt }

// Reset is called after a success.
func (b *Backoff) Reset() { b.attempt = 0 }

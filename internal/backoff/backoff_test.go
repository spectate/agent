package backoff

import (
	"testing"
	"time"
)

func TestGrowsAndCaps(t *testing.T) {
	b := New(5*time.Second, time.Minute)
	b.rand = func() float64 { return 1 } // upper bound
	want := []time.Duration{5, 10, 20, 40, 60, 60}
	for i, w := range want {
		if got := b.Next(); got != w*time.Second {
			t.Fatalf("attempt %d: %v, want %v", i, got, w*time.Second)
		}
	}
	b.Reset()
	if b.Attempts() != 0 || b.Next() != 5*time.Second {
		t.Fatal("reset should restart at base")
	}
}

func TestJitterStaysWithinHalfAndFull(t *testing.T) {
	b := New(time.Second, 10*time.Second)
	for range 1000 {
		b.Reset()
		for i := range 6 {
			d := b.Next()
			full := min(time.Second<<i, 10*time.Second)
			if d < full/2 || d > full {
				t.Fatalf("attempt %d: %v outside [%v, %v]", i, d, full/2, full)
			}
		}
	}
}

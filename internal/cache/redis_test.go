package cache

import (
	"testing"
	"time"
)

func TestBackoffDelayGrows(t *testing.T) {
	base := 40 * time.Millisecond
	var prev time.Duration
	for attempt := 0; attempt < 8; attempt++ {
		d := BackoffDelay(attempt, base)
		// Full jitter range is [0.8x, 1.2x] of the pure exponent.
		pure := float64(base) * float64(uint64(1)<<uint(attempt))
		min, max := 0.8*pure, 1.2*pure
		if float64(d) < min || float64(d) > max {
			t.Fatalf("attempt %d: delay %v outside jitter band [%v, %v]", attempt, d, min, max)
		}
		if attempt > 0 && float64(d) < float64(prev)*0.8 {
			t.Fatalf("delay collapsed between attempts: %v then %v", prev, d)
		}
		prev = d
	}
}

func TestBackoffDelayClampsNegative(t *testing.T) {
	d := BackoffDelay(-5, time.Second)
	min, max := 0.8*float64(time.Second), 1.2*float64(time.Second)
	if float64(d) < min || float64(d) > max {
		t.Fatalf("negative attempts should clamp to attempt 0 jitter band, got %v", d)
	}
}

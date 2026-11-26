package engine

import (
	"testing"
	"time"

	"github.com/rajkumaar23/offerhound/internal/config"
)

func TestLimiterAdaptiveBackoff(t *testing.T) {
	l := newLimiter(config.RateLimit{
		RequestsPerRun: 1000,
		DelayBetween:   config.Duration(600 * time.Millisecond),
	})
	if l.delay != 600*time.Millisecond {
		t.Fatalf("base delay=%v", l.delay)
	}
	l.penalize()
	if l.delay != 1200*time.Millisecond {
		t.Fatalf("after penalize delay=%v want 1200ms", l.delay)
	}
	for i := 0; i < 10; i++ {
		l.penalize()
	}
	if l.delay != 30*time.Second {
		t.Fatalf("penalize must cap at 30s, got %v", l.delay)
	}
	// recovery: needs a long clean streak, halves at most per streak
	for i := 0; i < 40; i++ {
		l.onSuccess()
	}
	if l.delay != 15*time.Second {
		t.Fatalf("after one clean streak delay=%v want 15s", l.delay)
	}
	for i := 0; i < 40; i++ {
		l.onSuccess()
	}
	if l.delay != 7500*time.Millisecond {
		t.Fatalf("second streak delay=%v want 7.5s", l.delay)
	}
	// penalize resets the streak: no instant recovery after fresh 429
	before := l.delay // 7.5s
	l.penalize()      // -> 15s, streak reset
	if l.delay != before*2 {
		t.Fatalf("penalize must double: %v", l.delay)
	}
	for i := 0; i < 39; i++ {
		l.onSuccess()
	}
	if l.delay != before*2 {
		t.Fatalf("39 successes must not recover (streak was reset): %v", l.delay)
	}
	l.onSuccess() // completes the 40-streak
	if l.delay != before {
		t.Fatalf("streak should complete at 40 and halve back: %v", l.delay)
	}
	// recovery never goes below the configured base
	for i := 0; i < 4000; i++ {
		l.onSuccess()
	}
	if l.delay < 600*time.Millisecond {
		t.Fatalf("delay fell below base: %v", l.delay)
	}
}

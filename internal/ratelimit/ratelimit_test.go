package ratelimit

import (
	"testing"
	"time"
)

func TestNew_ZeroRate_AllowAll(t *testing.T) {
	t.Parallel()
	l := New(0, time.Minute)
	for i := 0; i < 100; i++ {
		if !l.Allow("ip") {
			t.Fatalf("zero-rate limiter denied request %d", i)
		}
	}
}

func TestAllow_DeniesOverflow(t *testing.T) {
	t.Parallel()
	l := New(3, time.Minute)
	l.now = staticClock(time.Unix(1000, 0))

	for i := 0; i < 3; i++ {
		if !l.Allow("ip") {
			t.Fatalf("burst request %d denied; want allowed (capacity=3)", i)
		}
	}
	if l.Allow("ip") {
		t.Errorf("4th request allowed; want denied (bucket should be empty)")
	}
}

func TestAllow_RefillsOverTime(t *testing.T) {
	t.Parallel()
	now := time.Unix(1000, 0)
	clock := newStepClock(now)
	l := New(6, time.Minute) // 6/min ⇒ refill 1 token / 10s
	l.now = clock.now

	// Drain the bucket.
	for i := 0; i < 6; i++ {
		l.Allow("ip")
	}
	if l.Allow("ip") {
		t.Fatalf("expected denial after drain")
	}

	// Advance 30s ⇒ 3 tokens refill.
	clock.advance(30 * time.Second)
	for i := 0; i < 3; i++ {
		if !l.Allow("ip") {
			t.Errorf("post-refill request %d denied", i)
		}
	}
	if l.Allow("ip") {
		t.Errorf("post-refill 4th request allowed; only 3 tokens should have refilled")
	}
}

func TestAllow_PerKeyIsolation(t *testing.T) {
	t.Parallel()
	l := New(2, time.Minute)
	l.now = staticClock(time.Unix(2000, 0))

	for i := 0; i < 2; i++ {
		if !l.Allow("ip-a") {
			t.Fatalf("ip-a request %d denied", i)
		}
	}
	if l.Allow("ip-a") {
		t.Errorf("ip-a 3rd allowed; want denied")
	}
	// ip-b has its own bucket.
	for i := 0; i < 2; i++ {
		if !l.Allow("ip-b") {
			t.Errorf("ip-b request %d denied — buckets must be per-key", i)
		}
	}
}

func staticClock(t time.Time) func() time.Time {
	return func() time.Time { return t }
}

type stepClock struct {
	t time.Time
}

func newStepClock(start time.Time) *stepClock { return &stepClock{t: start} }
func (s *stepClock) now() time.Time           { return s.t }
func (s *stepClock) advance(d time.Duration)  { s.t = s.t.Add(d) }

package auth

import (
	"fmt"
	"testing"
	"time"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func TestLimiterBlocksAfterMaxFailures(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	l := NewLimiter(3, time.Minute, 100, clk.now)

	for i := range 3 {
		if d := l.RetryAfter("k"); d != 0 {
			t.Fatalf("blocked after %d failures", i)
		}
		l.Fail("k")
		clk.advance(time.Second)
	}
	if d := l.RetryAfter("k"); d != 57*time.Second {
		t.Fatalf("RetryAfter = %v, want 57s (window started at the first failure)", d)
	}
	if d := l.RetryAfter("other"); d != 0 {
		t.Errorf("other key blocked: %v", d)
	}

	clk.advance(57 * time.Second)
	if d := l.RetryAfter("k"); d != 0 {
		t.Errorf("still blocked after the window: %v", d)
	}
	l.Fail("k")
	if d := l.RetryAfter("k"); d != 0 {
		t.Errorf("a new window should start from zero: %v", d)
	}
}

func TestLimiterReset(t *testing.T) {
	l := NewLimiter(1, time.Minute, 100, nil)
	l.Fail("k")
	if l.RetryAfter("k") == 0 {
		t.Fatal("not blocked")
	}
	l.Reset("k")
	if d := l.RetryAfter("k"); d != 0 {
		t.Errorf("blocked after reset: %v", d)
	}
}

func TestLimiterFailsClosedWhenFull(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	l := NewLimiter(5, time.Minute, 3, clk.now)
	for i := range 3 {
		l.Fail(fmt.Sprint(i))
	}
	l.Fail("overflow") // not tracked: the table is full
	if d := l.RetryAfter("new"); d != time.Minute {
		t.Errorf("new key while full: RetryAfter = %v, want the window (fail closed)", d)
	}
	if d := l.RetryAfter("0"); d != 0 {
		t.Errorf("tracked key below the limit is blocked: %v", d)
	}
	// Expired entries are swept to make room.
	clk.advance(time.Minute)
	if d := l.RetryAfter("new"); d != 0 {
		t.Errorf("new key after expiry: %v", d)
	}
	l.Fail("new")
	if len(l.entries) != 1 {
		t.Errorf("entries = %d, want 1 after sweep", len(l.entries))
	}
}

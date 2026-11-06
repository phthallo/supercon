package main

import (
	"testing"
	"time"
)

func testLimiter() (*Limiter, *time.Time) {
	now := time.Unix(1_000_000, 0)
	l := NewLimiter(30 * time.Second)
	l.now = func() time.Time { return now }
	return l, &now
}

func TestLimiterCooldown(t *testing.T) {
	l, now := testLimiter()
	if ok, _ := l.Take("1.1.1.1", "a"); !ok {
		t.Fatal("first paint refused")
	}
	if ok, wait := l.Take("1.1.1.1", "a"); ok || wait != 30*time.Second {
		t.Fatalf("second paint: ok=%v wait=%v", ok, wait)
	}
	*now = now.Add(30 * time.Second)
	if ok, _ := l.Take("1.1.1.1", "a"); !ok {
		t.Fatal("paint after cooldown refused")
	}
}

func TestLimiterNewCookieSameIP(t *testing.T) {
	l, _ := testLimiter()
	l.Take("1.1.1.1", "a")
	if ok, _ := l.Take("1.1.1.1", "b"); ok {
		t.Fatal("clearing cookies reset the cooldown")
	}
}

func TestLimiterSameCookieNewIP(t *testing.T) {
	l, _ := testLimiter()
	l.Take("1.1.1.1", "a")
	if ok, _ := l.Take("2.2.2.2", "a"); ok {
		t.Fatal("switching networks reset the cooldown")
	}
}

func TestLimiterIndependentUsers(t *testing.T) {
	l, _ := testLimiter()
	l.Take("1.1.1.1", "a")
	if ok, _ := l.Take("2.2.2.2", "b"); !ok {
		t.Fatal("unrelated user was blocked")
	}
}

func TestLimiterRefund(t *testing.T) {
	l, _ := testLimiter()
	l.Take("1.1.1.1", "a")
	l.Refund("1.1.1.1", "a")
	if wait := l.Wait("1.1.1.1", "a"); wait != 0 {
		t.Fatalf("refund left wait %v", wait)
	}
}

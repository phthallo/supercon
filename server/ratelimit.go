package main

import (
	"sync"
	"time"
)

// Limiter enforces one paint per cooldown per IP and per cookie. Both keys must
// be clear, so clearing cookies or switching networks alone does not reset it.
type Limiter struct {
	cooldown time.Duration
	now      func() time.Time

	mu       sync.Mutex
	byIP     map[string]time.Time
	byCookie map[string]time.Time
}

func NewLimiter(cooldown time.Duration) *Limiter {
	return &Limiter{cooldown: cooldown, now: time.Now, byIP: map[string]time.Time{}, byCookie: map[string]time.Time{}}
}

// Wait reports how long until ip and cookie may paint again; zero means now.
func (l *Limiter) Wait(ip, cookie string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.waitLocked(ip, cookie)
}

func (l *Limiter) waitLocked(ip, cookie string) time.Duration {
	now := l.now()
	var wait time.Duration
	for _, until := range []time.Time{l.byIP[ip], l.byCookie[cookie]} {
		if d := until.Sub(now); d > wait {
			wait = d
		}
	}
	return wait
}

// Take claims a paint if allowed, otherwise returns the remaining wait.
func (l *Limiter) Take(ip, cookie string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if wait := l.waitLocked(ip, cookie); wait > 0 {
		return false, wait
	}
	until := l.now().Add(l.cooldown)
	l.byIP[ip] = until
	l.byCookie[cookie] = until
	if len(l.byIP)+len(l.byCookie) > 50_000 {
		l.pruneLocked()
	}
	return true, l.cooldown
}

// Refund undoes a Take whose paint failed to save.
func (l *Limiter) Refund(ip, cookie string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.byIP, ip)
	delete(l.byCookie, cookie)
}

func (l *Limiter) pruneLocked() {
	now := l.now()
	for _, m := range []map[string]time.Time{l.byIP, l.byCookie} {
		for k, until := range m {
			if !until.After(now) {
				delete(m, k)
			}
		}
	}
}

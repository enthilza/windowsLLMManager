package security

import (
	"sync"
	"time"
)

type bucket struct {
	tokens float64
	last   time.Time
}

type RateLimiter struct {
	mu       sync.Mutex
	rate     float64
	capacity float64
	buckets  map[string]bucket
}

func NewRateLimiter(perSecond, burst int) *RateLimiter {
	return &RateLimiter{rate: float64(perSecond), capacity: float64(burst), buckets: make(map[string]bucket)}
}

func (r *RateLimiter) Allow(ip string, now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	b, ok := r.buckets[ip]
	if !ok {
		b = bucket{tokens: r.capacity, last: now}
	}
	elapsed := now.Sub(b.last).Seconds()
	b.tokens += elapsed * r.rate
	if b.tokens > r.capacity {
		b.tokens = r.capacity
	}
	b.last = now
	allowed := b.tokens >= 1
	if allowed {
		b.tokens--
	}
	r.buckets[ip] = b
	return allowed
}

package security

import (
	"net/http/httptest"
	"testing"
	"time"
)

func TestTrustedProxyHeader(t *testing.T) {
	resolver := NewClientIPResolver("10.0.0.5")
	req := httptest.NewRequest("GET", "https://host/health", nil)
	req.RemoteAddr = "10.0.0.5:1234"
	req.Header.Set("CF-Connecting-IP", "203.0.113.9")
	if got := resolver.Resolve(req); got != "203.0.113.9" {
		t.Fatalf("unexpected forwarded IP %s", got)
	}
	req.RemoteAddr = "10.0.0.6:1234"
	if got := resolver.Resolve(req); got != "10.0.0.6" {
		t.Fatalf("untrusted source spoofed client IP: %s", got)
	}
}

func TestBlockAfterThreshold(t *testing.T) {
	b := NewBlocklist(2)
	if _, blocked := b.RecordFailure("192.0.2.1", time.Now()); blocked {
		t.Fatal("blocked too early")
	}
	if count, blocked := b.RecordFailure("192.0.2.1", time.Now()); !blocked || count != 2 {
		t.Fatalf("expected block on second failure: count=%d blocked=%t", count, blocked)
	}
}

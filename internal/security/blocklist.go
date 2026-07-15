package security

import (
	"sort"
	"sync"
	"time"
)

type BlockEntry struct {
	IP             string
	BlockedAt      time.Time
	FailedAttempts int
}

type Blocklist struct {
	mu        sync.RWMutex
	threshold int
	failures  map[string]int
	blocked   map[string]BlockEntry
}

func NewBlocklist(threshold int) *Blocklist {
	return &Blocklist{threshold: threshold, failures: make(map[string]int), blocked: make(map[string]BlockEntry)}
}

func (b *Blocklist) IsBlocked(ip string) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	_, ok := b.blocked[ip]
	return ok
}

func (b *Blocklist) RecordFailure(ip string, now time.Time) (int, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if entry, ok := b.blocked[ip]; ok {
		return entry.FailedAttempts, true
	}
	b.failures[ip]++
	count := b.failures[ip]
	if count >= b.threshold {
		b.blocked[ip] = BlockEntry{IP: ip, BlockedAt: now.UTC(), FailedAttempts: count}
		delete(b.failures, ip)
		return count, true
	}
	return count, false
}

func (b *Blocklist) RecordSuccess(ip string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.failures, ip)
}

func (b *Blocklist) Remove(ip string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, existed := b.blocked[ip]
	delete(b.blocked, ip)
	delete(b.failures, ip)
	return existed
}

func (b *Blocklist) List() []BlockEntry {
	b.mu.RLock()
	defer b.mu.RUnlock()
	result := make([]BlockEntry, 0, len(b.blocked))
	for _, entry := range b.blocked {
		result = append(result, entry)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].IP < result[j].IP })
	return result
}

package powershell

import "sync"

type boundedBuffer struct {
	mu        sync.Mutex
	b         []byte
	max       int
	truncated bool
}

func newBoundedBuffer(max int) *boundedBuffer { return &boundedBuffer{max: max} }

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	remaining := b.max - len(b.b)
	if remaining > 0 {
		if remaining > len(p) {
			remaining = len(p)
		}
		b.b = append(b.b, p[:remaining]...)
	}
	if remaining < len(p) {
		b.truncated = true
	}
	return len(p), nil
}

func (b *boundedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(append([]byte(nil), b.b...))
}

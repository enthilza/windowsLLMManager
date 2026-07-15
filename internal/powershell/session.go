package powershell

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"windowsllmmanager/internal/api"
)

var (
	ErrSessionNotFound = errors.New("session not found")
	ErrSessionLimit    = errors.New("maximum number of sessions reached")
)

type Session struct {
	id        string
	createdAt time.Time
	maxOutput int

	execMu   sync.Mutex
	stateMu  sync.RWMutex
	process  *childProcess
	lastUsed time.Time
	busy     bool
	closed   bool
}

func newSession(id string, maxOutput int) (*Session, error) {
	p, err := startChild(true, maxOutput)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	return &Session{id: id, createdAt: now, lastUsed: now, maxOutput: maxOutput, process: p}, nil
}

func (s *Session) Exec(ctx context.Context, command, format string) (api.ExecutionResult, error) {
	s.execMu.Lock()
	defer s.execMu.Unlock()
	started := time.Now()
	s.stateMu.Lock()
	if s.closed || s.process == nil {
		s.stateMu.Unlock()
		return api.ExecutionResult{}, ErrSessionNotFound
	}
	p := s.process
	s.busy = true
	s.lastUsed = time.Now().UTC()
	s.stateMu.Unlock()
	defer func() {
		s.stateMu.Lock()
		s.busy = false
		s.lastUsed = time.Now().UTC()
		s.stateMu.Unlock()
	}()

	id, err := randomID()
	if err != nil {
		return api.ExecutionResult{}, err
	}
	if _, err := io.WriteString(p.stdin, buildWrapper(command, format, id, s.maxOutput)+"\r\n"); err != nil {
		restarted := s.restartLocked()
		return shellExitedResult(format, started, restarted), fmt.Errorf("%w: write command: %v", ErrShellExited, err)
	}
	frameCh := make(chan struct {
		result framedResult
		err    error
	}, 1)
	go func() {
		result, err := readFrame(p.stdout, id, s.maxOutput)
		frameCh <- struct {
			result framedResult
			err    error
		}{result, err}
	}()
	select {
	case <-ctx.Done():
		p.kill()
		<-p.waitDone
		restarted := s.restartLocked()
		return timeoutResult(format, started, restarted), ErrTimedOut
	case framed := <-frameCh:
		if framed.err != nil {
			p.kill()
			<-p.waitDone
			restarted := s.restartLocked()
			return shellExitedResult(format, started, restarted), fmt.Errorf("%w: %v", ErrShellExited, framed.err)
		}
		return toAPIResult(framed.result, started, false), nil
	}
}

func shellExitedResult(format string, started time.Time, restarted bool) api.ExecutionResult {
	return api.ExecutionResult{
		Success: false, ExitCode: -1, Format: format, Output: json.RawMessage("null"),
		Stderr: []string{}, DurationMS: time.Since(started).Milliseconds(), SessionRestarted: restarted,
	}
}

func (s *Session) Restart() error {
	s.execMu.Lock()
	defer s.execMu.Unlock()
	if !s.restartLocked() {
		return errors.New("failed to restart PowerShell session")
	}
	return nil
}

func (s *Session) restartLocked() bool {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	if s.closed {
		return false
	}
	if s.process != nil {
		s.process.kill()
	}
	p, err := startChild(true, s.maxOutput)
	if err != nil {
		s.process = nil
		return false
	}
	s.process = p
	s.lastUsed = time.Now().UTC()
	return true
}

func (s *Session) Abort() {
	s.stateMu.Lock()
	if s.closed {
		s.stateMu.Unlock()
		return
	}
	s.closed = true
	p := s.process
	s.process = nil
	s.stateMu.Unlock()
	if p != nil {
		p.kill()
	}
}

func (s *Session) Info() api.SessionInfoResponse {
	s.stateMu.RLock()
	defer s.stateMu.RUnlock()
	return api.SessionInfoResponse{
		SessionID: s.id, CreatedAt: s.createdAt.Format(time.RFC3339Nano),
		LastUsedAt: s.lastUsed.Format(time.RFC3339Nano), UptimeSec: int64(time.Since(s.createdAt).Seconds()), Busy: s.busy,
	}
}

func (s *Session) IdleFor(now time.Time) time.Duration {
	s.stateMu.RLock()
	defer s.stateMu.RUnlock()
	if s.busy {
		return 0
	}
	return now.Sub(s.lastUsed)
}

type SessionManager struct {
	mu          sync.RWMutex
	sessions    map[string]*Session
	maxSessions int
	maxOutput   int
	idleTimeout time.Duration
	stopReaper  chan struct{}
}

func NewSessionManager(maxSessions, maxOutput int, idleTimeout time.Duration) *SessionManager {
	m := &SessionManager{
		sessions: make(map[string]*Session), maxSessions: maxSessions, maxOutput: maxOutput,
		idleTimeout: idleTimeout, stopReaper: make(chan struct{}),
	}
	go m.reapLoop()
	return m
}

func (m *SessionManager) Create() (*Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.sessions) >= m.maxSessions {
		return nil, ErrSessionLimit
	}
	id, err := randomID()
	if err != nil {
		return nil, err
	}
	s, err := newSession(id, m.maxOutput)
	if err != nil {
		return nil, err
	}
	m.sessions[id] = s
	return s, nil
}

func (m *SessionManager) Get(id string) (*Session, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.sessions[id]
	if !ok {
		return nil, ErrSessionNotFound
	}
	return s, nil
}

func (m *SessionManager) Delete(id string) bool {
	m.mu.Lock()
	s, ok := m.sessions[id]
	if ok {
		delete(m.sessions, id)
	}
	m.mu.Unlock()
	if ok {
		s.Abort()
	}
	return ok
}

func (m *SessionManager) Count() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.sessions)
}

func (m *SessionManager) KillAll() {
	m.mu.Lock()
	sessions := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		sessions = append(sessions, s)
	}
	m.sessions = make(map[string]*Session)
	m.mu.Unlock()
	for _, s := range sessions {
		s.Abort()
	}
}

func (m *SessionManager) Close() {
	select {
	case <-m.stopReaper:
	default:
		close(m.stopReaper)
	}
	m.KillAll()
}

func (m *SessionManager) reapLoop() {
	interval := m.idleTimeout / 2
	if interval > time.Minute {
		interval = time.Minute
	}
	if interval < time.Second {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case now := <-ticker.C:
			m.mu.RLock()
			ids := make([]string, 0)
			for id, s := range m.sessions {
				if s.IdleFor(now) > m.idleTimeout {
					ids = append(ids, id)
				}
			}
			m.mu.RUnlock()
			for _, id := range ids {
				m.Delete(id)
			}
		case <-m.stopReaper:
			return
		}
	}
}

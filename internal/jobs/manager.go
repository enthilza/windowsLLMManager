package jobs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"time"

	"windowsllmmanager/internal/api"
	"windowsllmmanager/internal/powershell"
)

const (
	StatusRunning    = "running"
	StatusCancelling = "cancelling"
	StatusCompleted  = "completed"
	StatusFailed     = "failed"
	StatusTimedOut   = "timed_out"
	StatusCancelled  = "cancelled"
)

var (
	ErrLimit      = errors.New("maximum number of asynchronous jobs reached")
	ErrNotFound   = errors.New("job not found")
	ErrNotRunning = errors.New("job is not running")
	ErrBlocked    = errors.New("asynchronous execution is blocked")
)

type Snapshot struct {
	JobID        string
	Status       string
	CreatedAt    time.Time
	StartedAt    time.Time
	CompletedAt  time.Time
	Timeout      time.Duration
	Execution    *api.ExecutionResult
	Error        string
	CancelReason string
}

type job struct {
	snapshot Snapshot
	cancel   context.CancelFunc
	onDone   func(Snapshot)
}

type Runner interface {
	Run(context.Context, string, string) (api.ExecutionResult, error)
}

type Manager struct {
	mu         sync.Mutex
	runner     Runner
	jobs       map[string]*job
	maxActive  int
	maxResults int
	retention  time.Duration
	blocked    bool
}

func NewManager(runner Runner, maxActive, maxResults int, retention time.Duration) *Manager {
	return &Manager{runner: runner, jobs: make(map[string]*job), maxActive: maxActive, maxResults: maxResults, retention: retention}
}

func (m *Manager) Submit(command, format string, timeout time.Duration, onDone func(Snapshot)) (Snapshot, error) {
	id, err := randomID()
	if err != nil {
		return Snapshot{}, err
	}
	now := time.Now().UTC()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	m.mu.Lock()
	m.pruneExpiredLocked(now)
	if m.blocked {
		m.mu.Unlock()
		cancel()
		return Snapshot{}, ErrBlocked
	}
	if m.activeLocked() >= m.maxActive {
		m.mu.Unlock()
		cancel()
		return Snapshot{}, ErrLimit
	}
	m.pruneCapacityLocked()
	j := &job{snapshot: Snapshot{JobID: id, Status: StatusRunning, CreatedAt: now, StartedAt: now, Timeout: timeout}, cancel: cancel, onDone: onDone}
	m.jobs[id] = j
	snapshot := cloneSnapshot(j.snapshot)
	m.mu.Unlock()
	go m.run(ctx, j, command, format)
	return snapshot, nil
}

func (m *Manager) run(ctx context.Context, j *job, command, format string) {
	result, runErr := m.runner.Run(ctx, command, format)
	now := time.Now().UTC()
	m.mu.Lock()
	if j.snapshot.Status == StatusCancelling {
		j.snapshot.Status = StatusCancelled
		result.TimedOut = false
	} else if errors.Is(runErr, powershell.ErrTimedOut) {
		j.snapshot.Status = StatusTimedOut
	} else if runErr != nil {
		j.snapshot.Status = StatusFailed
		j.snapshot.Error = runErr.Error()
	} else {
		j.snapshot.Status = StatusCompleted
	}
	j.snapshot.CompletedAt = now
	j.snapshot.Execution = cloneExecution(result)
	j.cancel()
	final := cloneSnapshot(j.snapshot)
	m.mu.Unlock()
	if j.onDone != nil {
		j.onDone(final)
	}
}

func (m *Manager) Get(id string) (Snapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pruneExpiredLocked(time.Now().UTC())
	j, ok := m.jobs[id]
	if !ok {
		return Snapshot{}, ErrNotFound
	}
	return cloneSnapshot(j.snapshot), nil
}

func (m *Manager) Cancel(id, reason string) (Snapshot, error) {
	m.mu.Lock()
	j, ok := m.jobs[id]
	if !ok {
		m.mu.Unlock()
		return Snapshot{}, ErrNotFound
	}
	if j.snapshot.Status != StatusRunning {
		snapshot := cloneSnapshot(j.snapshot)
		m.mu.Unlock()
		return snapshot, ErrNotRunning
	}
	j.snapshot.Status = StatusCancelling
	j.snapshot.CancelReason = reason
	cancel := j.cancel
	snapshot := cloneSnapshot(j.snapshot)
	m.mu.Unlock()
	cancel()
	return snapshot, nil
}

func (m *Manager) CancelAll(reason string) int {
	m.mu.Lock()
	cancels := make([]context.CancelFunc, 0)
	for _, j := range m.jobs {
		if j.snapshot.Status == StatusRunning {
			j.snapshot.Status = StatusCancelling
			j.snapshot.CancelReason = reason
			cancels = append(cancels, j.cancel)
		}
	}
	m.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
	return len(cancels)
}

func (m *Manager) BlockAndCancelAll(reason string) int {
	m.mu.Lock()
	m.blocked = true
	cancels := make([]context.CancelFunc, 0)
	for _, j := range m.jobs {
		if j.snapshot.Status == StatusRunning {
			j.snapshot.Status = StatusCancelling
			j.snapshot.CancelReason = reason
			cancels = append(cancels, j.cancel)
		}
	}
	m.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
	return len(cancels)
}

func (m *Manager) ActiveCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.activeLocked()
}

func (m *Manager) Close() { m.BlockAndCancelAll("server_shutdown") }

func (m *Manager) activeLocked() int {
	count := 0
	for _, j := range m.jobs {
		if j.snapshot.Status == StatusRunning || j.snapshot.Status == StatusCancelling {
			count++
		}
	}
	return count
}

func (m *Manager) pruneExpiredLocked(now time.Time) {
	for id, j := range m.jobs {
		if !j.snapshot.CompletedAt.IsZero() && now.Sub(j.snapshot.CompletedAt) >= m.retention {
			delete(m.jobs, id)
		}
	}
}

func (m *Manager) pruneCapacityLocked() {
	for len(m.jobs) >= m.maxResults {
		var oldestID string
		var oldest time.Time
		for id, j := range m.jobs {
			if j.snapshot.CompletedAt.IsZero() {
				continue
			}
			if oldestID == "" || j.snapshot.CompletedAt.Before(oldest) {
				oldestID, oldest = id, j.snapshot.CompletedAt
			}
		}
		if oldestID == "" {
			return
		}
		delete(m.jobs, oldestID)
	}
}

func cloneSnapshot(source Snapshot) Snapshot {
	result := source
	result.Execution = cloneExecutionValue(source.Execution)
	return result
}

func cloneExecution(source api.ExecutionResult) *api.ExecutionResult {
	copy := source
	copy.Output = append([]byte(nil), source.Output...)
	copy.Stderr = append([]string(nil), source.Stderr...)
	return &copy
}

func cloneExecutionValue(source *api.ExecutionResult) *api.ExecutionResult {
	if source == nil {
		return nil
	}
	return cloneExecution(*source)
}

func randomID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

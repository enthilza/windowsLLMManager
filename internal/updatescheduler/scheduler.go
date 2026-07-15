package updatescheduler

import (
	"context"
	"log"
	"sync"
	"time"
)

type Process interface {
	Wait() error
}

type Launcher interface {
	Start(path string, args ...string) (Process, error)
}

type Scheduler struct {
	path       string
	configPath string
	interval   time.Duration
	launcher   Launcher
	logger     *log.Logger

	mu      sync.Mutex
	running bool
}

func New(path, configPath string, interval time.Duration, logger *log.Logger) *Scheduler {
	if logger == nil {
		logger = log.Default()
	}
	return &Scheduler{path: path, configPath: configPath, interval: interval, launcher: processLauncher{}, logger: logger}
}

func newWithLauncher(path, configPath string, interval time.Duration, logger *log.Logger, launcher Launcher) *Scheduler {
	s := New(path, configPath, interval, logger)
	s.launcher = launcher
	return s
}

func (s *Scheduler) Start(ctx context.Context) {
	if s.interval <= 0 {
		s.logger.Printf("automatic update checks are disabled")
		return
	}
	s.logger.Printf("automatic updater scheduler enabled: interval=%s", s.interval)
	go s.run(ctx)
}

func (s *Scheduler) run(ctx context.Context) {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.launch()
		}
	}
}

func (s *Scheduler) launch() {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		s.logger.Printf("automatic update check skipped: previous updater process is still running")
		return
	}
	s.running = true
	s.mu.Unlock()

	process, err := s.launcher.Start(s.path, "--check-only", "--config", s.configPath)
	if err != nil {
		s.setRunning(false)
		s.logger.Printf("automatic updater launch failed: %v", err)
		return
	}
	s.logger.Printf("automatic updater launched")
	go func() {
		err := process.Wait()
		s.setRunning(false)
		if err != nil {
			s.logger.Printf("automatic updater exited with error: %v", err)
			return
		}
		s.logger.Printf("automatic updater finished")
	}()
}

func (s *Scheduler) setRunning(value bool) {
	s.mu.Lock()
	s.running = value
	s.mu.Unlock()
}
